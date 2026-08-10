// Package engine orchestrates one translated Anthropic Messages turn against
// the concrete ChatGPT Codex transport.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/codexstream"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/failure"
	"github.com/Aotricx/Clodex/internal/ratelimit"
	"github.com/Aotricx/Clodex/internal/redact"
	"github.com/Aotricx/Clodex/internal/reducer"
	"github.com/Aotricx/Clodex/internal/retry"
	"github.com/Aotricx/Clodex/internal/status"
	"github.com/Aotricx/Clodex/internal/upstream"
)

const (
	DefaultEmptyRetries     = 10
	DefaultTransientRetries = 3
)

type RetryReason string

const (
	RetryEmptyCompletion RetryReason = "empty_completion"
	RetryNetwork         RetryReason = "network_failure"
	RetryHTTP            RetryReason = "http_failure"
	RetryStream          RetryReason = "stream_failure"
	RetryEffortFloor     RetryReason = "effort_floor"
)

// Transport is implemented by upstream.Client and bounded test transports.
type Transport interface {
	Stream(context.Context, upstream.Session, codexwire.Request) (*http.Response, error)
}

// RetryDiagnosticTransport persists the redacted request/response evidence for
// retry triggers that occur after a successful HTTP response was accepted.
type RetryDiagnosticTransport interface {
	DumpRetry(context.Context, upstream.Session, codexwire.Request, string, int, json.RawMessage)
}

type Request struct {
	Session     upstream.Session
	Wire        codexwire.Request
	Streaming   bool
	FloorEffort func(codexwire.Request) (codexwire.Request, bool, error)
}

type StreamHooks struct {
	// Ready commits downstream success headers. Engine calls it only after a
	// semantic event or an honest successful terminal decision is available.
	Ready func(http.Header) error
	Emit  func(reducer.Event) error
}

// DumpTrigger contains only redacted retry metadata; prompts and credentials
// are deliberately absent.
type DumpTrigger struct {
	Reason  RetryReason
	Attempt int
	Event   json.RawMessage
}

// Timing is one turn's latency decomposition, stamped via the injectable
// clock. Durations only — no wire content. The per-attempt legs describe the
// SUCCESSFUL attempt; Total spans the whole Run including retry waits, so
// pair it with Attempts when interpreting aggregates.
type Timing struct {
	// TransportWait is Transport.Stream call → upstream response headers.
	TransportWait time.Duration
	// FirstEvent is upstream headers → first SSE event decoded.
	FirstEvent time.Duration
	// Commit is upstream headers → commit-gate release (first client-visible
	// frame authorized). Zero for buffered turns, which never commit.
	Commit time.Duration
	// Total is Run entry → successful terminal.
	Total time.Duration
}

type Result struct {
	Response         reducer.Result
	Events           []reducer.Event
	Headers          http.Header
	Attempts         int
	EffortFloorCount int
	Timing           Timing
}

// Error is ready for the Anthropic HTTP boundary without hiding the real
// upstream status/message.
type Error struct {
	Failure failure.Failure
}

func (err *Error) Error() string {
	if err == nil {
		return ""
	}
	return err.Failure.Message
}

func (err *Error) AnthropicResponse() anthropic.ErrorResponse {
	return err.Failure.AnthropicResponse()
}

// Engine owns retry policy for all turns. Retry must be process-shared so its
// budget and circuit breaker protect the account globally.
type Engine struct {
	Transport Transport
	Retry     *retry.Controller
	// Nil uses the corresponding default. Pointers permit explicit zero.
	MaxEmptyRetries     *int
	MaxTransientRetries *int
	Wait                func(context.Context, int, string) error
	Now                 func() time.Time
	Dump                func(DumpTrigger)
	RateLimit           func(status.RateLimitSnapshot)
}

func (engine *Engine) Run(ctx context.Context, request Request, hooks StreamHooks) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("run Codex turn: nil context")
	}
	if engine == nil || engine.Transport == nil || engine.Retry == nil {
		return Result{}, errors.New("run Codex turn: transport and retry controller are required")
	}
	maxEmptyRetries, err := retryLimit(engine.MaxEmptyRetries, DefaultEmptyRetries, "empty completion")
	if err != nil {
		return Result{}, err
	}
	maxTransientRetries, err := retryLimit(engine.MaxTransientRetries, DefaultTransientRetries, "transient")
	if err != nil {
		return Result{}, err
	}
	current := request.Wire
	emptyRetriesUsed := 0
	transientRetriesUsed := 0
	effortFloorCount := 0
	effortFloored := false
	attempts := 0
	runStart := engine.now()

	for {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		if err := engine.Retry.Allow(); err != nil {
			return Result{}, protectionError(err)
		}
		attempts++
		streamStart := engine.now()
		response, transportErr := engine.Transport.Stream(ctx, request.Session, current)
		if transportErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			var authenticationError *upstream.AuthenticationError
			if errors.As(transportErr, &authenticationError) {
				engine.Retry.Failed(false)
				return Result{}, apiError(http.StatusUnauthorized, "authentication_error", authenticationError.Error(), "", false)
			}
			if transientRetriesUsed >= maxTransientRetries {
				engine.Retry.Failed(true)
				return Result{}, networkError(transportErr)
			}
			engine.dump(ctx, request.Session, current, DumpTrigger{Reason: RetryNetwork, Attempt: attempts, Event: redactedMessage(transportErr.Error())})
			if err := engine.prepareRetry(ctx, transientRetriesUsed+1, "", true); err != nil {
				return Result{}, err
			}
			transientRetriesUsed++
			continue
		}
		if response == nil {
			return Result{}, networkError(errors.New("Codex transport returned nil response"))
		}

		if response.StatusCode < 200 || response.StatusCode >= 300 {
			body, readErr := readAndClose(response.Body)
			if readErr != nil {
				return Result{}, networkError(readErr)
			}
			mapped := failure.FromHTTP(response.StatusCode, body, response.Header)
			if !effortFloored && unsupportedEffort(mapped) && request.FloorEffort != nil {
				floored, ok, floorErr := request.FloorEffort(current)
				if floorErr != nil {
					return Result{}, floorErr
				}
				if ok {
					engine.dump(ctx, request.Session, current, DumpTrigger{Reason: RetryEffortFloor, Attempt: attempts})
					if err := engine.prepareRetry(ctx, 1, "", false); err != nil {
						return Result{}, err
					}
					current = floored
					effortFloored = true
					effortFloorCount++
					continue
				}
			}
			if !mapped.Retryable || transientRetriesUsed >= maxTransientRetries {
				engine.Retry.Failed(mapped.Retryable)
				return Result{}, &Error{Failure: mapped}
			}
			engine.dump(ctx, request.Session, current, DumpTrigger{Reason: RetryHTTP, Attempt: attempts})
			if err := engine.prepareRetry(ctx, transientRetriesUsed+1, mapped.RetryAfter, true); err != nil {
				return Result{}, err
			}
			transientRetriesUsed++
			continue
		}

		attempt, err := engine.consume(ctx, request.Streaming, response, hooks, engine.now().Sub(streamStart))
		if err == nil {
			engine.Retry.Succeeded()
			attempt.result.Attempts = attempts
			attempt.result.EffortFloorCount = effortFloorCount
			attempt.result.Timing.Total = engine.now().Sub(runStart)
			return attempt.result, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}

		if attempt.committed {
			var committedError *Error
			if errors.As(err, &committedError) {
				engine.Retry.Failed(committedError.Failure.Retryable)
			}
			return Result{}, err
		}
		if errors.Is(err, reducer.ErrEmptyCompletion) {
			if emptyRetriesUsed >= maxEmptyRetries {
				return Result{}, emptyCompletionError()
			}
			engine.dump(ctx, request.Session, current, DumpTrigger{Reason: RetryEmptyCompletion, Attempt: attempts, Event: attempt.retryEvent})
			if retryErr := engine.prepareRetry(ctx, emptyRetriesUsed+1, "", false); retryErr != nil {
				return Result{}, retryErr
			}
			emptyRetriesUsed++
			continue
		}
		var apiError *Error
		if errors.As(err, &apiError) && apiError.Failure.Retryable && transientRetriesUsed < maxTransientRetries {
			engine.dump(ctx, request.Session, current, DumpTrigger{Reason: RetryStream, Attempt: attempts, Event: attempt.retryEvent})
			if retryErr := engine.prepareRetry(ctx, transientRetriesUsed+1, apiError.Failure.RetryAfter, true); retryErr != nil {
				return Result{}, retryErr
			}
			transientRetriesUsed++
			continue
		}
		if errors.As(err, &apiError) {
			engine.Retry.Failed(apiError.Failure.Retryable)
		}
		return Result{}, err
	}
}

func retryLimit(configured *int, fallback int, name string) (int, error) {
	if configured == nil {
		return fallback, nil
	}
	if *configured < 0 {
		return 0, errors.New("run Codex turn: " + name + " retries must not be negative")
	}
	return *configured, nil
}

type attemptResult struct {
	result     Result
	committed  bool
	retryEvent json.RawMessage
}

func (engine *Engine) consume(ctx context.Context, streaming bool, response *http.Response, hooks StreamHooks, transportWait time.Duration) (attemptResult, error) {
	defer response.Body.Close()
	headers := response.Header.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	now := engine.now()
	headersAt := now
	var firstEventAt, commitAt time.Time
	if snapshot, ok := ratelimit.ParseHeaders(headers, now); ok {
		ratelimit.ApplyResponseHeaders(headers, snapshot)
		engine.rateLimit(snapshot)
	}

	parser := codexstream.New(ctx, response.Body, codexstream.Options{})
	reduce := reducer.New()
	var allEvents []reducer.Event
	var pending []reducer.Event
	committed := false
	var final *reducer.Result
	var retryEvent json.RawMessage

	commit := func(force bool) error {
		if !streaming || committed || !force {
			return nil
		}
		if hooks.Ready != nil {
			if err := hooks.Ready(headers.Clone()); err != nil {
				return err
			}
		}
		committed = true
		if commitAt.IsZero() {
			commitAt = engine.now()
		}
		if hooks.Emit != nil {
			for _, event := range pending {
				if err := hooks.Emit(event); err != nil {
					return err
				}
			}
		}
		pending = nil
		return nil
	}

	for {
		upstreamEvent, err := parser.Next()
		if err != nil {
			if errors.Is(err, io.EOF) && final != nil {
				timing := Timing{TransportWait: transportWait}
				if !firstEventAt.IsZero() {
					timing.FirstEvent = firstEventAt.Sub(headersAt)
				}
				if !commitAt.IsZero() {
					timing.Commit = commitAt.Sub(headersAt)
				}
				return attemptResult{result: Result{Response: *final, Events: allEvents, Headers: headers, Timing: timing}, committed: committed}, nil
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return attemptResult{committed: committed}, ctxErr
			}
			return attemptResult{committed: committed, retryEvent: retryEvent}, networkError(err)
		}
		if firstEventAt.IsZero() {
			firstEventAt = engine.now()
		}
		retryEvent = sanitizeEvent(upstreamEvent.Raw)
		if upstreamEvent.Type == "codex.rate_limits" {
			snapshot, parseErr := ratelimit.ParseEvent(upstreamEvent.Raw, engine.now())
			if parseErr != nil {
				return attemptResult{committed: committed, retryEvent: retryEvent}, apiError(http.StatusBadGateway, "api_error", parseErr.Error(), "", false)
			}
			ratelimit.ApplyResponseHeaders(headers, snapshot)
			engine.rateLimit(snapshot)
		}

		produced, pushErr := reduce.Push(upstreamEvent)
		allEvents = append(allEvents, produced...)
		if streaming {
			if committed {
				if hooks.Emit != nil {
					for _, event := range produced {
						if emitErr := hooks.Emit(event); emitErr != nil {
							return attemptResult{committed: true}, emitErr
						}
					}
				}
			} else {
				pending = append(pending, produced...)
				if semanticEvents(produced) {
					if err := commit(true); err != nil {
						return attemptResult{committed: committed}, err
					}
				}
			}
		}
		if pushErr != nil {
			if errors.Is(pushErr, reducer.ErrUpstreamFailed) {
				mapped := failureFromStream(upstreamEvent.Raw)
				return attemptResult{committed: committed, retryEvent: retryEvent}, &Error{Failure: mapped}
			}
			if errors.Is(pushErr, reducer.ErrEmptyCompletion) {
				return attemptResult{committed: committed, retryEvent: retryEvent}, reducer.ErrEmptyCompletion
			}
			return attemptResult{committed: committed, retryEvent: retryEvent}, apiError(http.StatusBadGateway, "api_error", pushErr.Error(), "", false)
		}
		for _, event := range produced {
			if event.Kind != reducer.KindTerminal {
				continue
			}
			result, resultErr := reduce.Result()
			if resultErr != nil {
				return attemptResult{committed: committed, retryEvent: retryEvent}, resultErr
			}
			final = &result
			if err := commit(true); err != nil {
				return attemptResult{committed: committed}, err
			}
		}
	}
}

func semanticEvents(events []reducer.Event) bool {
	for _, event := range events {
		switch event.Kind {
		case reducer.KindContentDelta:
			if event.Delta.Text != "" {
				return true
			}
		case reducer.KindContentStart:
			if event.Block.Type == reducer.BlockToolUse || event.Block.Type == reducer.BlockServerToolUse {
				return true
			}
		}
	}
	return false
}

func (engine *Engine) prepareRetry(ctx context.Context, attempt int, retryAfter string, failureEvent bool) error {
	if failureEvent {
		engine.Retry.Failed(true)
	}
	if err := engine.Retry.ReserveRetry(); err != nil {
		return protectionError(err)
	}
	wait := engine.Wait
	if wait == nil {
		wait = engine.Retry.Wait
	}
	if err := wait(ctx, attempt, retryAfter); err != nil {
		return err
	}
	return nil
}

func (engine *Engine) dump(ctx context.Context, session upstream.Session, request codexwire.Request, trigger DumpTrigger) {
	if diagnostic, ok := engine.Transport.(RetryDiagnosticTransport); ok {
		diagnostic.DumpRetry(ctx, session, request, string(trigger.Reason), trigger.Attempt, trigger.Event)
	}
	if engine.Dump != nil {
		engine.Dump(trigger)
	}
}

func (engine *Engine) rateLimit(snapshot status.RateLimitSnapshot) {
	if engine.RateLimit != nil {
		engine.RateLimit(snapshot)
	}
}

func (engine *Engine) now() time.Time {
	if engine.Now != nil {
		return engine.Now()
	}
	return time.Now().UTC()
}

func unsupportedEffort(mapped failure.Failure) bool {
	if mapped.StatusCode != http.StatusBadRequest {
		return false
	}
	message := strings.ToLower(mapped.Message)
	return strings.Contains(message, "effort") &&
		(strings.Contains(message, "unsupported") || strings.Contains(message, "not supported") || strings.Contains(message, "invalid"))
}

func readAndClose(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, 1<<20))
}

func failureFromStream(raw []byte) failure.Failure {
	statusCode := http.StatusBadGateway
	var envelope struct {
		Status   int `json:"status"`
		Response struct {
			Error struct {
				Status int `json:"status"`
			} `json:"error"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		if envelope.Response.Error.Status != 0 {
			statusCode = envelope.Response.Error.Status
		} else if envelope.Status != 0 {
			statusCode = envelope.Status
		}
	}
	return failure.FromHTTP(statusCode, raw, nil)
}

func emptyCompletionError() *Error {
	return apiError(http.StatusServiceUnavailable, "api_error", "Codex completed without producing output", "", false)
}

func networkError(err error) *Error {
	return apiError(http.StatusServiceUnavailable, "api_error", redact.Text(err.Error()), "", true)
}

func protectionError(err error) *Error {
	return apiError(http.StatusServiceUnavailable, "api_error", err.Error(), "", false)
}

func apiError(statusCode int, errorType, message, retryAfter string, retryable bool) *Error {
	return &Error{Failure: failure.Failure{
		StatusCode: statusCode,
		Type:       errorType,
		Message:    message,
		RetryAfter: retryAfter,
		Retryable:  retryable,
	}}
}

func sanitizeEvent(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if sanitized, err := redact.JSON(raw); err == nil {
		return json.RawMessage(sanitized)
	}
	return redactedMessage(string(raw))
}

func redactedMessage(message string) json.RawMessage {
	encoded, _ := json.Marshal(map[string]string{"message": redact.Text(message)})
	return encoded
}

var _ Transport = (*upstream.Client)(nil)
