package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/anthropicstream"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/engine"
	"github.com/Aotricx/Clodex/internal/model"
	"github.com/Aotricx/Clodex/internal/redact"
	"github.com/Aotricx/Clodex/internal/reducer"
	"github.com/Aotricx/Clodex/internal/requestprep"
	"github.com/Aotricx/Clodex/internal/retry"
	clodexstatus "github.com/Aotricx/Clodex/internal/status"
	"github.com/Aotricx/Clodex/internal/translate"
	"github.com/Aotricx/Clodex/internal/upstream"
)

const defaultHeartbeatInterval = 10 * time.Second

// MessagesOptions contains dependencies for the concrete Messages pipeline.
type MessagesOptions struct {
	Catalog      CatalogResolver
	Status       *clodexstatus.State
	DefaultModel string
	Engine       *engine.Engine
	Random       io.Reader
}

// MessagesService decodes, prepares, executes, and renders one Messages turn.
type MessagesService struct {
	catalog           CatalogResolver
	status            *clodexstatus.State
	defaultModel      string
	engine            *engine.Engine
	random            io.Reader
	randomMu          sync.Mutex
	heartbeatInterval time.Duration
	// now is the timing clock; nil means time.Now. Injectable so timing
	// tests can step it deterministically.
	now func() time.Time
}

func (service *MessagesService) clock() time.Time {
	if service.now != nil {
		return service.now()
	}
	return time.Now()
}

// recordTiming folds one successful turn into the bounded /status timing
// ring. firstByteAt is zero when no byte was written before completion
// (defensive; successful turns always stamp it).
func (service *MessagesService) recordTiming(received, firstByteAt time.Time, result *engine.Result) {
	if service.status == nil || result == nil {
		return
	}
	finished := service.clock()
	sample := clodexstatus.TimingSample{
		TotalMillis:      finished.Sub(received).Milliseconds(),
		UpstreamMillis:   result.Timing.TransportWait.Milliseconds(),
		FirstEventMillis: result.Timing.FirstEvent.Milliseconds(),
		CommitMillis:     result.Timing.Commit.Milliseconds(),
		Attempts:         result.Attempts,
	}
	if !firstByteAt.IsZero() {
		sample.TTFTMillis = firstByteAt.Sub(received).Milliseconds()
	}
	service.status.RecordTiming(sample)
}

// NewMessages validates the complete runtime message pipeline.
func NewMessages(options MessagesOptions) (*MessagesService, error) {
	if options.Catalog == nil || options.Status == nil || options.Engine == nil {
		return nil, errors.New("create Messages service: catalog, status, and engine are required")
	}
	if strings.TrimSpace(options.DefaultModel) == "" {
		return nil, errors.New("create Messages service: default model is required")
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	service := &MessagesService{
		catalog: options.Catalog, status: options.Status, defaultModel: options.DefaultModel,
		engine: options.Engine, random: random, heartbeatInterval: defaultHeartbeatInterval,
	}
	service.syncRetryStatus()
	return service, nil
}

func (service *MessagesService) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	defer service.syncRetryStatus()
	received := service.clock()
	decoded, err := anthropic.DecodeRequest(request.Body)
	if err != nil {
		service.writeAnyError(writer, err)
		return
	}
	resolution, err := service.catalog.Resolve(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "api_error", redact.Text(err.Error()))
		return
	}
	service.status.SetCatalog(string(resolution.Source), resolution.Catalog.FetchedAt)

	session, err := service.session(request.Header.Get("x-claude-code-session-id"))
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
		return
	}
	prepared, err := requestprep.Prepare(resolution.Catalog, decoded, service.defaultModel, session.ThreadID)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request_error", redact.Text(err.Error()))
		return
	}
	service.recordTranslationWarnings(prepared.Translation.Warnings)
	messageID, err := service.messageID()
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
		return
	}

	turn := engine.Request{
		Session: session, Wire: prepared.Translation.Request, Streaming: decoded.Stream,
		FloorEffort: effortFloor(prepared.Selection.Model),
	}
	if decoded.Stream {
		service.stream(writer, request, decoded, messageID, turn, prepared.Translation.StopSequences, received)
		return
	}
	service.buffered(writer, request.Context(), decoded, messageID, turn, prepared.Translation.StopSequences, received)
}

func (service *MessagesService) syncRetryStatus() {
	if service == nil || service.engine == nil || service.engine.Retry == nil || service.status == nil {
		return
	}
	snapshot := service.engine.Retry.Snapshot()
	service.status.SetRemainingRetryBudget(int64(snapshot.RemainingBudget))
	service.status.SetCircuit(clodexstatus.CircuitState{
		Open: snapshot.State == retry.StateOpen, Failures: int64(snapshot.Failures), OpenUntil: snapshot.OpenUntil,
	})
}

func (service *MessagesService) buffered(writer http.ResponseWriter, ctx context.Context, request *anthropic.MessageRequest, messageID string, turn engine.Request, stops []string, received time.Time) {
	result, err := service.engine.Run(ctx, turn, engine.StreamHooks{})
	if err != nil {
		service.writeEngineError(writer, err)
		return
	}
	service.recordReducerWarnings(result.Response.Warnings)
	truncated, _, err := anthropicstream.ApplyStops(result.Response, stops)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
		return
	}
	body, err := anthropicstream.MarshalBuffered(messageID, request.Model, truncated)
	if err != nil {
		writeError(writer, http.StatusBadGateway, "api_error", redact.Text(err.Error()))
		return
	}
	copyTelemetryHeaders(writer.Header(), result.Headers)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	firstByteAt := service.clock()
	_, _ = writer.Write(body)
	service.recordTiming(received, firstByteAt, &result)
}

type streamSignal struct {
	ready    http.Header
	event    *reducer.Event
	result   *engine.Result
	err      error
	finished bool
}

func (service *MessagesService) stream(writer http.ResponseWriter, request *http.Request, decoded *anthropic.MessageRequest, messageID string, turn engine.Request, stops []string, received time.Time) {
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	signals := make(chan streamSignal)
	done := make(chan struct{})
	send := func(signal streamSignal) error {
		select {
		case signals <- signal:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() {
		defer close(done)
		result, err := service.engine.Run(ctx, turn, engine.StreamHooks{
			Ready: func(headers http.Header) error { return send(streamSignal{ready: headers}) },
			Emit: func(event reducer.Event) error {
				copy := event
				return send(streamSignal{event: &copy})
			},
		})
		_ = send(streamSignal{result: &result, err: err, finished: true})
	}()

	encoder, err := anthropicstream.New(writer, anthropicstream.Options{
		MessageID: messageID, Model: decoded.Model, StopSequences: stops,
		Flush: func() error {
			if flusher, ok := writer.(http.Flusher); ok {
				flusher.Flush()
			}
			return nil
		},
	})
	if err != nil {
		cancel()
		<-done
		writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
		return
	}

	committed := false
	var firstByteAt time.Time
	var heartbeat <-chan time.Time
	var timer *time.Timer
	resetHeartbeat := func() {
		if timer == nil {
			timer = time.NewTimer(service.heartbeatInterval)
		} else {
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(service.heartbeatInterval)
		}
		heartbeat = timer.C
	}
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()

	for {
		select {
		case <-request.Context().Done():
			cancel()
			<-done
			return
		case <-heartbeat:
			if err := encoder.Ping(); err != nil {
				if errors.Is(err, anthropicstream.ErrStreamFinished) {
					if timer != nil {
						timer.Stop()
					}
					heartbeat = nil
					continue
				}
				cancel()
				<-done
				return
			}
			resetHeartbeat()
		case signal := <-signals:
			if signal.ready != nil {
				copyTelemetryHeaders(writer.Header(), signal.ready)
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.Header().Set("Connection", "keep-alive")
				writer.WriteHeader(http.StatusOK)
				committed = true
				firstByteAt = service.clock()
				resetHeartbeat()
				continue
			}
			if signal.event != nil {
				if err := encoder.Encode(*signal.event); err != nil {
					cancel()
					<-done
					return
				}
				resetHeartbeat()
				continue
			}
			if !signal.finished {
				continue
			}
			if signal.err != nil {
				if !committed {
					service.writeEngineError(writer, signal.err)
				} else if request.Context().Err() == nil {
					_ = writeStreamError(writer, signal.err)
				}
				return
			}
			if signal.result != nil {
				service.recordReducerWarnings(signal.result.Response.Warnings)
				service.recordTiming(received, firstByteAt, signal.result)
			}
			return
		}
	}
}

func sessionFromHeader(claudeSession string) (upstream.Session, error) {
	return (&MessagesService{random: rand.Reader}).session(claudeSession)
}

func (service *MessagesService) session(claudeSession string) (upstream.Session, error) {
	claudeSession = strings.TrimSpace(claudeSession)
	var sessionID string
	if validUUID(claudeSession) {
		sessionID = strings.ToLower(claudeSession)
	} else if claudeSession != "" {
		sessionID = derivedUUID("clodex-session\x00" + claudeSession)
	} else {
		generated, err := service.randomUUID()
		if err != nil {
			return upstream.Session{}, fmt.Errorf("generate upstream session ID: %w", err)
		}
		sessionID = generated
	}
	return upstream.Session{SessionID: sessionID, ThreadID: derivedUUID("clodex-thread\x00" + sessionID)}, nil
}

func (service *MessagesService) messageID() (string, error) {
	buffer := make([]byte, 16)
	if err := service.readRandom(buffer); err != nil {
		return "", fmt.Errorf("generate Anthropic message ID: %w", err)
	}
	return "msg_" + hex.EncodeToString(buffer), nil
}

func (service *MessagesService) randomUUID() (string, error) {
	buffer := make([]byte, 16)
	if err := service.readRandom(buffer); err != nil {
		return "", err
	}
	buffer[6] = buffer[6]&0x0f | 0x40
	buffer[8] = buffer[8]&0x3f | 0x80
	return formatUUID(buffer), nil
}

func (service *MessagesService) readRandom(buffer []byte) error {
	service.randomMu.Lock()
	defer service.randomMu.Unlock()
	_, err := io.ReadFull(service.random, buffer)
	return err
}

func effortFloor(selected catalog.Model) func(codexwire.Request) (codexwire.Request, bool, error) {
	return func(request codexwire.Request) (codexwire.Request, bool, error) {
		if request.Reasoning == nil || request.Reasoning.Effort == "" {
			return request, false, nil
		}
		floor, err := model.NearestSupportedFloor(selected, request.Reasoning.Effort)
		if err != nil {
			return request, false, nil
		}
		reasoning := *request.Reasoning
		reasoning.Effort = floor
		request.Reasoning = &reasoning
		return request, true, nil
	}
}

func (service *MessagesService) recordTranslationWarnings(warnings []translate.Warning) {
	for _, warning := range warnings {
		service.status.RecordWarnings(warning.Kind, int64(warning.Count))
	}
}

func (service *MessagesService) recordReducerWarnings(warnings []reducer.Warning) {
	for _, warning := range warnings {
		service.status.RecordWarnings(warning.Kind, int64(warning.Count))
	}
}

func (service *MessagesService) writeAnyError(writer http.ResponseWriter, err error) {
	var requestError *anthropic.RequestError
	if errors.As(err, &requestError) {
		writeJSON(writer, requestError.StatusCode, requestError.Response, false)
		return
	}
	writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
}

func (service *MessagesService) writeEngineError(writer http.ResponseWriter, err error) {
	var turnError *engine.Error
	if errors.As(err, &turnError) {
		if turnError.Failure.RetryAfter != "" {
			writer.Header().Set("Retry-After", turnError.Failure.RetryAfter)
		}
		writeJSON(writer, turnError.Failure.StatusCode, turnError.AnthropicResponse(), false)
		return
	}
	writeError(writer, http.StatusInternalServerError, "api_error", redact.Text(err.Error()))
}

func writeStreamError(writer http.ResponseWriter, err error) error {
	errorResponse := anthropic.ErrorResponse{Type: "error", Error: anthropic.ErrorDetail{Type: "api_error", Message: redact.Text(err.Error())}}
	var turnError *engine.Error
	if errors.As(err, &turnError) {
		errorResponse = turnError.AnthropicResponse()
	}
	encoded, marshalErr := json.Marshal(errorResponse)
	if marshalErr != nil {
		return marshalErr
	}
	if _, writeErr := fmt.Fprintf(writer, "event: error\ndata: %s\n\n", encoded); writeErr != nil {
		return writeErr
	}
	if flusher, ok := writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func copyTelemetryHeaders(destination, source http.Header) {
	for name, values := range source {
		if !strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Clodex-") {
			continue
		}
		destination[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
}

func derivedUUID(seed string) string {
	digest := sha256.Sum256([]byte(seed))
	buffer := digest[:16]
	buffer[6] = buffer[6]&0x0f | 0x50
	buffer[8] = buffer[8]&0x3f | 0x80
	return formatUUID(buffer)
}

func formatUUID(buffer []byte) string {
	encoded := hex.EncodeToString(buffer)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func validUUID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", character) {
			return false
		}
	}
	return true
}
