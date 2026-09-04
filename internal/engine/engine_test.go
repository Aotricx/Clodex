package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/reducer"
	"github.com/Aotricx/Clodex/internal/retry"
	"github.com/Aotricx/Clodex/internal/status"
	"github.com/Aotricx/Clodex/internal/upstream"
)

func TestRegressionPR70TerminalOnlyCompletedRetriesBounded(t *testing.T) {
	assertEmptyRegression(t, "regression_terminal_only_completed.sse")
}

func TestRegressionPR71DoneWithoutSemanticOutput(t *testing.T) {
	assertEmptyRegression(t, "regression_terminal_only_done.sse")
}

func TestEngineUsesIndependentEmptyAndTransientRetryBounds(t *testing.T) {
	t.Run("empty completion bound", func(t *testing.T) {
		transport := &scriptedTransport{fallback: reply{status: 200, body: fixtureBytes(t, "regression_terminal_only_completed.sse")}}
		engine := newTestEngine(t, transport)
		empty, transient := 2, 0
		engine.MaxEmptyRetries = &empty
		engine.MaxTransientRetries = &transient
		_, _ = engine.Run(context.Background(), testRequest(false), StreamHooks{})
		if got := transport.Attempts(); got != 3 {
			t.Fatalf("attempts = %d, want 3", got)
		}
	})

	t.Run("transient bound", func(t *testing.T) {
		transport := &scriptedTransport{fallback: reply{err: io.ErrUnexpectedEOF}}
		engine := newTestEngine(t, transport)
		empty, transient := 10, 1
		engine.MaxEmptyRetries = &empty
		engine.MaxTransientRetries = &transient
		_, _ = engine.Run(context.Background(), testRequest(false), StreamHooks{})
		if got := transport.Attempts(); got != 2 {
			t.Fatalf("attempts = %d, want 2", got)
		}
	})
}

func TestEmptyCompletionBoundSurvivesLowTransportCircuitThreshold(t *testing.T) {
	transport := &scriptedTransport{fallback: reply{status: 200, body: fixtureBytes(t, "regression_terminal_only_completed.sse")}}
	engine := newTestEngineWithThreshold(t, transport, 1)
	empty := 2
	engine.MaxEmptyRetries = &empty
	_, _ = engine.Run(context.Background(), testRequest(false), StreamHooks{})
	if got := transport.Attempts(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
}

func TestEveryRetryTriggerInvokesTransportWireDiagnostic(t *testing.T) {
	base := &scriptedTransport{fallback: reply{status: 200, body: fixtureBytes(t, "regression_terminal_only_done.sse")}}
	transport := &diagnosticTransport{scriptedTransport: base}
	engine := newTestEngine(t, transport)
	empty := 1
	engine.MaxEmptyRetries = &empty
	_, _ = engine.Run(context.Background(), testRequest(false), StreamHooks{})
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.diagnostics) != 1 || transport.diagnostics[0].reason != string(RetryEmptyCompletion) || transport.diagnostics[0].attempt != 1 || transport.diagnostics[0].session.SessionID != "session" || transport.diagnostics[0].request.Model != "gpt-5.4-mini" {
		t.Fatalf("diagnostics = %#v", transport.diagnostics)
	}
}

func assertEmptyRegression(t *testing.T, fixture string) {
	t.Helper()
	body := fixtureBytes(t, fixture)
	transport := &scriptedTransport{fallback: reply{status: 200, body: body}}
	var dumps []DumpTrigger
	engine := newTestEngine(t, transport)
	engine.Dump = func(trigger DumpTrigger) { dumps = append(dumps, trigger) }
	request := testRequest(true)

	var ready, emitted int
	_, err := engine.Run(context.Background(), request, StreamHooks{
		Ready: func(http.Header) error { ready++; return nil },
		Emit:  func(reducer.Event) error { emitted++; return nil },
	})
	var apiError *Error
	if !errors.As(err, &apiError) {
		t.Fatalf("Run error = %T %v, want *Error", err, err)
	}
	if apiError.Failure.StatusCode != http.StatusServiceUnavailable || apiError.Failure.Type != "api_error" || apiError.Failure.Message != "Codex completed without producing output" {
		t.Fatalf("failure = %#v", apiError.Failure)
	}
	wantEnvelope := anthropic.ErrorResponse{Type: "error", Error: anthropic.ErrorDetail{Type: "api_error", Message: "Codex completed without producing output"}}
	if !reflect.DeepEqual(apiError.AnthropicResponse(), wantEnvelope) {
		t.Fatalf("AnthropicResponse = %#v", apiError.AnthropicResponse())
	}
	if got := transport.Attempts(); got != 11 {
		t.Fatalf("attempts = %d, want 11", got)
	}
	requests := transport.RequestBodies()
	for i := 1; i < len(requests); i++ {
		if !bytes.Equal(requests[0], requests[i]) {
			t.Fatalf("attempt %d request changed\nfirst=%s\nnext=%s", i+1, requests[0], requests[i])
		}
	}
	if len(dumps) != 10 {
		t.Fatalf("semantic retry dumps = %d, want 10", len(dumps))
	}
	for _, dump := range dumps {
		if dump.Reason != RetryEmptyCompletion || bytes.Contains(dump.Event, []byte("secret")) {
			t.Fatalf("dump = %#v", dump)
		}
	}
	if ready != 0 || emitted != 0 {
		t.Fatalf("downstream committed before exhausted empty response: ready=%d emitted=%d", ready, emitted)
	}
}

func TestRegressionIncompleteIsMaxTokensNoRetry(t *testing.T) {
	transport := &scriptedTransport{fallback: reply{status: 200, body: fixtureBytes(t, "regression_incomplete.sse")}}
	engine := newTestEngine(t, transport)
	result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if transport.Attempts() != 1 || result.Response.StopReason != reducer.StopMaxTokens || result.Response.Usage != (reducer.Usage{}) {
		t.Fatalf("result/attempts = %#v / %d", result, transport.Attempts())
	}
}

func TestRegressionPR68CreditedRateLimitSnapshot(t *testing.T) {
	transport := &scriptedTransport{fallback: reply{status: 200, body: fixtureBytes(t, "regression_credited_rate_limit.sse")}}
	engine := newTestEngine(t, transport)
	var snapshots []status.RateLimitSnapshot
	engine.RateLimit = func(snapshot status.RateLimitSnapshot) { snapshots = append(snapshots, snapshot) }
	result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	if err != nil {
		t.Fatal(err)
	}
	if transport.Attempts() != 1 || result.Response.StopReason != reducer.StopEndTurn || len(snapshots) != 1 || snapshots[0].Credits == nil || !snapshots[0].Credits.HasCredits {
		t.Fatalf("result/snapshots = %#v / %#v", result, snapshots)
	}
	for name, want := range map[string]string{
		"X-Clodex-Rate-Limit-Reached":  "true",
		"X-Clodex-Credits-Has-Credits": "true",
	} {
		if got := result.Headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if result.Headers.Get("Retry-After") != "" {
		t.Fatalf("credited snapshot fabricated Retry-After: %q", result.Headers.Get("Retry-After"))
	}
}

func TestEngineRetriesTransientFailuresAndFailsFastPermanentHTTP(t *testing.T) {
	t.Run("transient network and 503", func(t *testing.T) {
		transport := &scriptedTransport{script: []reply{
			{err: io.ErrUnexpectedEOF},
			{status: 503, headers: http.Header{"Retry-After": {"7"}}, body: []byte(`{"error":{"message":"temporarily unavailable"}}`)},
			{status: 200, body: successText("ok")},
		}}
		engine := newTestEngine(t, transport)
		var waits []string
		engine.Wait = func(_ context.Context, _ int, retryAfter string) error { waits = append(waits, retryAfter); return nil }
		var dumps []DumpTrigger
		engine.Dump = func(trigger DumpTrigger) { dumps = append(dumps, trigger) }
		result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
		if err != nil || result.Response.Content[0].Text != "ok" || transport.Attempts() != 3 || !reflect.DeepEqual(waits, []string{"", "7"}) || len(dumps) != 2 {
			t.Fatalf("result=%#v err=%v attempts=%d waits=%#v dumps=%#v", result, err, transport.Attempts(), waits, dumps)
		}
	})

	t.Run("permanent 400", func(t *testing.T) {
		transport := &scriptedTransport{fallback: reply{status: 400, body: []byte(`{"detail":"bad schema"}`)}}
		engine := newTestEngine(t, transport)
		_, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
		var apiError *Error
		if !errors.As(err, &apiError) || transport.Attempts() != 1 || apiError.Failure.StatusCode != 400 || apiError.Failure.Message != "bad schema" {
			t.Fatalf("error=%v attempts=%d", err, transport.Attempts())
		}
	})
}

func TestEngineFailsAuthenticationTransportErrorWithoutRetry(t *testing.T) {
	transport := &scriptedTransport{fallback: reply{err: &upstream.AuthenticationError{Err: errors.New("no ChatGPT OAuth login")}}}
	engine := newTestEngine(t, transport)
	_, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	var apiError *Error
	if !errors.As(err, &apiError) || apiError.Failure.StatusCode != http.StatusUnauthorized || apiError.Failure.Type != "authentication_error" || transport.Attempts() != 1 {
		t.Fatalf("error=%#v attempts=%d", err, transport.Attempts())
	}
}

func TestEngineFloorsUnsupportedEffortExactlyOnce(t *testing.T) {
	transport := &scriptedTransport{script: []reply{
		{status: 400, body: []byte(`{"detail":"reasoning effort is unsupported for this account"}`)},
		{status: 200, body: successText("floored")},
	}}
	zero := 0
	engine := newTestEngine(t, transport)
	engine.MaxEmptyRetries = &zero
	engine.MaxTransientRetries = &zero
	request := testRequest(false)
	request.FloorEffort = func(request codexwire.Request) (codexwire.Request, bool, error) {
		if request.Reasoning == nil || request.Reasoning.Effort != "xhigh" {
			return request, false, nil
		}
		request.Reasoning = &codexwire.Reasoning{Effort: "high", Summary: request.Reasoning.Summary}
		return request, true, nil
	}
	result, err := engine.Run(context.Background(), request, StreamHooks{})
	if err != nil || transport.Attempts() != 2 || result.EffortFloorCount != 1 {
		t.Fatalf("result=%#v err=%v attempts=%d", result, err, transport.Attempts())
	}
	bodies := transport.RequestBodies()
	if !bytes.Contains(bodies[0], []byte(`"effort":"xhigh"`)) || !bytes.Contains(bodies[1], []byte(`"effort":"high"`)) {
		t.Fatalf("effort bodies = %s / %s", bodies[0], bodies[1])
	}
}

func TestEngineMapsTerminalResponseFailedHonestly(t *testing.T) {
	body := []byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"status\":400,\"message\":\"real terminal failure\"}}}\n\n")
	transport := &scriptedTransport{fallback: reply{status: 200, body: body}}
	engine := newTestEngine(t, transport)
	_, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	var apiError *Error
	if !errors.As(err, &apiError) || transport.Attempts() != 1 || apiError.Failure.StatusCode != 400 || apiError.Failure.Message != "real terminal failure" {
		t.Fatalf("error=%v attempts=%d", err, transport.Attempts())
	}
}

func TestStreamingDefersCommitUntilSemanticOutput(t *testing.T) {
	transport := &scriptedTransport{fallback: reply{status: 200, body: successText("streamed")}}
	engine := newTestEngine(t, transport)
	var order []string
	result, err := engine.Run(context.Background(), testRequest(true), StreamHooks{
		Ready: func(http.Header) error { order = append(order, "ready"); return nil },
		Emit: func(event reducer.Event) error {
			if len(order) == 0 || order[0] != "ready" {
				t.Fatal("event emitted before readiness")
			}
			order = append(order, "event")
			return nil
		},
	})
	if err != nil || result.Response.Content[0].Text != "streamed" || len(order) < 2 || order[0] != "ready" {
		t.Fatalf("result=%#v err=%v order=%#v", result, err, order)
	}
}

func TestRunCapturesTimingWithSteppingClock(t *testing.T) {
	transport := &scriptedTransport{fallback: reply{status: 200, body: successText("timed")}}
	engine := newTestEngine(t, transport)
	// Stepping clock: every engine.now() call advances one second, so each
	// timing leg spans at least one tick and ordering is provable.
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tick := 0
	engine.Now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}
	result, err := engine.Run(context.Background(), testRequest(true), StreamHooks{
		Ready: func(http.Header) error { return nil },
		Emit:  func(reducer.Event) error { return nil },
	})
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	timing := result.Timing
	if timing.TransportWait <= 0 {
		t.Errorf("TransportWait = %v, want > 0", timing.TransportWait)
	}
	if timing.FirstEvent <= 0 {
		t.Errorf("FirstEvent = %v, want > 0", timing.FirstEvent)
	}
	if timing.Commit < timing.FirstEvent {
		t.Errorf("Commit = %v, want >= FirstEvent %v (commit follows the first event)", timing.Commit, timing.FirstEvent)
	}
	if timing.Total < timing.TransportWait+timing.Commit {
		t.Errorf("Total = %v, want >= TransportWait+Commit (%v)", timing.Total, timing.TransportWait+timing.Commit)
	}
}

func TestRunBufferedTimingHasNoCommitLeg(t *testing.T) {
	transport := &scriptedTransport{fallback: reply{status: 200, body: successText("buffered")}}
	engine := newTestEngine(t, transport)
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tick := 0
	engine.Now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}
	result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	if err != nil {
		t.Fatalf("Run error = %v", err)
	}
	if result.Timing.Commit != 0 {
		t.Errorf("Commit = %v, want 0 (buffered turns never commit)", result.Timing.Commit)
	}
	if result.Timing.Total <= 0 || result.Timing.TransportWait <= 0 || result.Timing.FirstEvent <= 0 {
		t.Errorf("Timing = %+v, want positive total/transport/first-event", result.Timing)
	}
}

func TestOverlongRetryAfterReturnsEngineErrorNotBareError(t *testing.T) {
	controller, err := retry.New(retry.Config{
		BaseDelay: time.Millisecond, MaxDelay: time.Second, RetryAfterLimit: 5 * time.Minute,
		Budget: 100, BudgetWindow: time.Hour, FailureThreshold: 100, CircuitCooldown: time.Minute,
	}, fixedClock{}, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	transport := &scriptedTransport{fallback: reply{
		status:  429,
		headers: http.Header{"Retry-After": {"3600"}},
		body:    []byte(`{"error":{"message":"rate limited"}}`),
	}}
	engine := &Engine{Transport: transport, Retry: controller}
	_, runErr := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	var apiError *Error
	if !errors.As(runErr, &apiError) {
		t.Fatalf("Run error = %T %v, want *Error", runErr, runErr)
	}
	if apiError.Failure.StatusCode != http.StatusServiceUnavailable && apiError.Failure.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want 503 or 429", apiError.Failure.StatusCode)
	}
	if apiError.Failure.Retryable {
		t.Fatal("Retryable = true, want false")
	}
	if apiError.Failure.RetryAfter != "3600" {
		t.Fatalf("RetryAfter = %q, want 3600 so Claude can see it", apiError.Failure.RetryAfter)
	}
}

func TestEmptyCompletionRetryHoldsHalfOpenProbe(t *testing.T) {
	clock := newTestClock(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))
	controller, err := retry.New(retry.Config{
		BaseDelay: time.Millisecond, MaxDelay: time.Second, RetryAfterLimit: time.Hour,
		Budget: 100, BudgetWindow: time.Hour, FailureThreshold: 1, CircuitCooldown: time.Minute,
	}, clock, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	controller.Failed(true)
	if snapshot := controller.Snapshot(); snapshot.State != retry.StateOpen {
		t.Fatalf("precondition: circuit state = %s, want open", snapshot.State)
	}
	clock.advance(time.Minute)

	transport := &scriptedTransport{script: []reply{
		{status: 200, body: fixtureBytes(t, "regression_terminal_only_completed.sse")},
		{status: 200, body: successText("recovered")},
	}}
	empty := 2
	engine := &Engine{
		Transport:       transport,
		Retry:           controller,
		MaxEmptyRetries: &empty,
		Wait:            func(context.Context, int, string) error { return nil },
	}
	result, runErr := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	if runErr != nil {
		t.Fatalf("Run error = %v, want empty-completion retry to reuse the half-open probe", runErr)
	}
	if got := transport.Attempts(); got != 2 {
		t.Fatalf("attempts = %d, want 2 (probe + empty retry, not circuit-open abort)", got)
	}
	if result.Response.Content[0].Text != "recovered" {
		t.Fatalf("content = %#v, want recovered", result.Response.Content)
	}
}

func TestFailedOpenCircuitSkipsWaitAndReturnsEngineError(t *testing.T) {
	controller, err := retry.New(retry.Config{
		BaseDelay: time.Millisecond, MaxDelay: time.Second, RetryAfterLimit: time.Hour,
		Budget: 100, BudgetWindow: time.Hour, FailureThreshold: 1, CircuitCooldown: time.Minute,
	}, fixedClock{}, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	waited := false
	transport := &scriptedTransport{fallback: reply{status: 503, body: []byte(`{"error":{"message":"unavailable"}}`)}}
	transient := 3
	engine := &Engine{
		Transport:           transport,
		Retry:               controller,
		MaxTransientRetries: &transient,
		Wait: func(context.Context, int, string) error {
			waited = true
			return nil
		},
	}
	_, runErr := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	var apiError *Error
	if !errors.As(runErr, &apiError) {
		t.Fatalf("Run error = %T %v, want *Error", runErr, runErr)
	}
	if apiError.Failure.StatusCode != http.StatusServiceUnavailable || apiError.Failure.Retryable {
		t.Fatalf("failure = %#v", apiError.Failure)
	}
	if !strings.Contains(apiError.Failure.Message, retry.ErrCircuitOpen.Error()) {
		t.Fatalf("message = %q, want circuit open", apiError.Failure.Message)
	}
	if waited {
		t.Fatal("Wait ran after Failed opened the circuit")
	}
	if got := transport.Attempts(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestConsumeIgnoresTrailingEventsAfterTerminal(t *testing.T) {
	rateLimits := []byte("event: codex.rate_limits\ndata: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"limit_reached\":true},\"credits\":{\"has_credits\":true,\"unlimited\":false}}\n\n")
	lateDelta := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"late\"}\n\n")
	malformedLimits := []byte("event: codex.rate_limits\ndata: {\"type\":\"codex.rate_limits\",\"credits\":{\"has_credits\":true}}\n\n")

	t.Run("trailing rate limits", func(t *testing.T) {
		var snapshots []status.RateLimitSnapshot
		transport := &scriptedTransport{fallback: reply{status: 200, body: concatSSE(successText("ok"), rateLimits)}}
		engine := newTestEngine(t, transport)
		engine.RateLimit = func(snapshot status.RateLimitSnapshot) { snapshots = append(snapshots, snapshot) }
		result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
		if err != nil {
			t.Fatalf("Run error = %v, want success after trailing rate_limits", err)
		}
		if result.Response.Content[0].Text != "ok" || transport.Attempts() != 1 {
			t.Fatalf("result=%#v attempts=%d", result, transport.Attempts())
		}
		if len(snapshots) != 1 || !snapshots[0].LimitReached {
			t.Fatalf("trailing rate_limits snapshots = %#v", snapshots)
		}
	})

	t.Run("trailing output delta", func(t *testing.T) {
		transport := &scriptedTransport{fallback: reply{status: 200, body: concatSSE(successText("ok"), lateDelta)}}
		engine := newTestEngine(t, transport)
		result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
		if err != nil {
			t.Fatalf("Run error = %v, want ignore trailing delta (not 502)", err)
		}
		if result.Response.Content[0].Text != "ok" {
			t.Fatalf("content = %#v", result.Response.Content)
		}
	})

	t.Run("malformed trailing rate limits", func(t *testing.T) {
		transport := &scriptedTransport{fallback: reply{status: 200, body: concatSSE(successText("ok"), malformedLimits)}}
		engine := newTestEngine(t, transport)
		result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
		if err != nil {
			t.Fatalf("Run error = %v, want skip malformed trailing rate_limits", err)
		}
		if result.Response.Content[0].Text != "ok" {
			t.Fatalf("content = %#v", result.Response.Content)
		}
	})
}

func TestEngineRetriesNilTransportResponse(t *testing.T) {
	transport := &scriptedTransport{script: []reply{
		{nilResponse: true},
		{status: 200, body: successText("recovered")},
	}}
	engine := newTestEngine(t, transport)
	result, err := engine.Run(context.Background(), testRequest(false), StreamHooks{})
	if err != nil {
		t.Fatalf("Run error = %v, want nil-response retry", err)
	}
	if transport.Attempts() != 2 || result.Response.Content[0].Text != "recovered" {
		t.Fatalf("result=%#v attempts=%d", result, transport.Attempts())
	}
}

func testRequest(streaming bool) Request {
	return Request{
		Session:   upstream.Session{SessionID: "session", ThreadID: "thread"},
		Streaming: streaming,
		Wire: codexwire.Request{
			Model:        "gpt-5.4-mini",
			Instructions: "full context secret",
			Input: []codexwire.InputItem{codexwire.Message{Role: "user", Content: []codexwire.ContentItem{
				codexwire.InputText{Text: "do the whole task"},
			}}},
			Reasoning: &codexwire.Reasoning{Effort: "xhigh", Summary: "auto"},
		},
	}
}

func newTestEngine(t *testing.T, transport Transport) *Engine {
	return newTestEngineWithThreshold(t, transport, 100)
}

func newTestEngineWithThreshold(t *testing.T, transport Transport, threshold int) *Engine {
	t.Helper()
	controller, err := retry.New(retry.Config{
		BaseDelay: time.Millisecond, MaxDelay: time.Second, RetryAfterLimit: time.Hour,
		Budget: 100, BudgetWindow: time.Hour, FailureThreshold: threshold, CircuitCooldown: time.Minute,
	}, fixedClock{}, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{
		Transport: transport,
		Retry:     controller,
		Now:       func() time.Time { return time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC) },
		Wait:      func(context.Context, int, string) error { return nil },
	}
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "fixtures", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func successText(text string) []byte {
	return []byte("event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":" + mustJSON(text) + "}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
}

func concatSSE(parts ...[]byte) []byte {
	var body []byte
	for _, part := range parts {
		body = append(body, part...)
	}
	return body
}

func mustJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

type reply struct {
	status      int
	headers     http.Header
	body        []byte
	err         error
	nilResponse bool
}

type scriptedTransport struct {
	mu       sync.Mutex
	script   []reply
	fallback reply
	attempts int
	requests [][]byte
}

func (transport *scriptedTransport) Stream(_ context.Context, _ upstream.Session, request codexwire.Request) (*http.Response, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	transport.requests = append(transport.requests, bytes.Clone(body))
	index := transport.attempts
	transport.attempts++
	selected := transport.fallback
	if index < len(transport.script) {
		selected = transport.script[index]
	}
	if selected.err != nil {
		return nil, selected.err
	}
	if selected.nilResponse {
		return nil, nil
	}
	statusCode := selected.status
	if statusCode == 0 {
		statusCode = 200
	}
	return &http.Response{
		StatusCode: statusCode,
		Header:     selected.headers.Clone(),
		Body:       io.NopCloser(bytes.NewReader(selected.body)),
	}, nil
}

func (transport *scriptedTransport) Attempts() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.attempts
}

func (transport *scriptedTransport) RequestBodies() [][]byte {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	result := make([][]byte, len(transport.requests))
	for i := range transport.requests {
		result[i] = bytes.Clone(transport.requests[i])
	}
	return result
}

type fixedClock struct{}

func (fixedClock) Now() time.Time                                   { return time.Unix(0, 0) }
func (fixedClock) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock(now time.Time) *testClock { return &testClock{now: now} }

func (clock *testClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *testClock) Sleep(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clock.mu.Lock()
	clock.now = clock.now.Add(delay)
	clock.mu.Unlock()
	return nil
}

func (clock *testClock) advance(delay time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(delay)
	clock.mu.Unlock()
}

type diagnosticRecord struct {
	session upstream.Session
	request codexwire.Request
	reason  string
	attempt int
	event   json.RawMessage
}

type diagnosticTransport struct {
	*scriptedTransport
	mu          sync.Mutex
	diagnostics []diagnosticRecord
}

func (transport *diagnosticTransport) DumpRetry(_ context.Context, session upstream.Session, request codexwire.Request, reason string, attempt int, event json.RawMessage) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.diagnostics = append(transport.diagnostics, diagnosticRecord{session: session, request: request, reason: reason, attempt: attempt, event: bytes.Clone(event)})
}
