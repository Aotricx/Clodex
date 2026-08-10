package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/engine"
	"github.com/Aotricx/Clodex/internal/retry"
	clodexstatus "github.com/Aotricx/Clodex/internal/status"
	"github.com/Aotricx/Clodex/internal/upstream"
)

func TestMessagesBufferedAndStreamingUseSameReducer(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "streaming"}[streaming], func(t *testing.T) {
			transport := &messageTransport{body: messageSuccessSSE("hello")}
			service := testMessagesService(t, transport)
			body := `{"model":"gpt-5.4-mini:low","max_tokens":32000,"messages":[{"role":"user","content":"say hello"}],"stream":` + map[bool]string{false: "false", true: "true"}[streaming] + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", strings.NewReader(body))
			request.Header.Set("x-claude-code-session-id", "123e4567-e89b-12d3-a456-426614174000")
			response := httptest.NewRecorder()
			service.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("response = %d %s", response.Code, response.Body.String())
			}
			if streaming {
				if response.Header().Get("Content-Type") != "text/event-stream" {
					t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
				}
				stream := response.Body.String()
				for _, marker := range []string{"event: message_start", "event: content_block_start", `"text":"hello"`, "event: content_block_stop", "event: message_delta", `"input_tokens":7`, `"output_tokens":3`, "event: message_stop"} {
					if !strings.Contains(stream, marker) {
						t.Errorf("stream missing %q:\n%s", marker, stream)
					}
				}
				assertOrder(t, stream, "event: message_start", "event: content_block_start", "event: content_block_delta", "event: content_block_stop", "event: message_delta", "event: message_stop")
			} else {
				if response.Header().Get("Content-Type") != "application/json" {
					t.Fatalf("content type = %q", response.Header().Get("Content-Type"))
				}
				var decoded struct {
					Model      string `json:"model"`
					StopReason string `json:"stop_reason"`
					Content    []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					Usage struct {
						InputTokens  int64 `json:"input_tokens"`
						OutputTokens int64 `json:"output_tokens"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded.Model != "gpt-5.4-mini:low" || decoded.StopReason != "end_turn" || len(decoded.Content) != 1 || decoded.Content[0].Text != "hello" || decoded.Usage.InputTokens != 7 || decoded.Usage.OutputTokens != 3 {
					t.Fatalf("buffered response = %s, error=%v", response.Body.Bytes(), err)
				}
			}
			sessions := transport.Sessions()
			if len(sessions) != 1 || sessions[0].SessionID != "123e4567-e89b-12d3-a456-426614174000" || sessions[0].ThreadID == "" || sessions[0].ThreadID == sessions[0].SessionID {
				t.Fatalf("upstream sessions = %#v", sessions)
			}
			requests := transport.Requests()
			if len(requests) != 1 || requests[0].PromptCacheKey != sessions[0].ThreadID || requests[0].Model != "gpt-5.4-mini" || requests[0].Reasoning == nil || requests[0].Reasoning.Effort != "low" {
				t.Fatalf("upstream requests = %#v", requests)
			}
		})
	}
}

func TestMessagesFailureBeforeSemanticOutputPreservesHTTPError(t *testing.T) {
	transport := &messageTransport{status: http.StatusForbidden, body: []byte(`{"detail":"account cannot use model"}`)}
	service := testMessagesService(t, transport)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4-mini:low","max_tokens":1,"messages":[{"role":"user","content":"x"}],"stream":true}`))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	assertAnthropicError(t, response, http.StatusForbidden, "permission_error")
	if !strings.Contains(response.Body.String(), "account cannot use model") {
		t.Fatalf("response hid real reason: %s", response.Body.String())
	}
}

func TestMessagesEmptyCompletionNeverCommitsStreaming200(t *testing.T) {
	transport := &messageTransport{body: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")}
	service := testMessagesService(t, transport)
	zero := 0
	service.engine.MaxEmptyRetries = &zero
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4-mini:low","max_tokens":1,"messages":[{"role":"user","content":"x"}],"stream":true}`))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	assertAnthropicError(t, response, http.StatusServiceUnavailable, "api_error")
	if transport.Attempts() != 1 || strings.Contains(response.Body.String(), "message_start") {
		t.Fatalf("attempts/body = %d/%s", transport.Attempts(), response.Body.String())
	}
}

func TestMessagesSynchronizesRetryBudgetAndCircuitStatus(t *testing.T) {
	transport := &messageTransport{body: []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0,\"total_tokens\":1}}}\n\n")}
	service := testMessagesService(t, transport)
	if got := service.status.Snapshot().Retry.RemainingBudget; got != 100 {
		t.Fatalf("initial remaining budget = %d, want 100", got)
	}
	one := 1
	service.engine.MaxEmptyRetries = &one
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4-mini:low","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if got := service.status.Snapshot().Retry.RemainingBudget; got != 99 {
		t.Fatalf("remaining budget after retry = %d, want 99", got)
	}
}

func TestMessagesStreamingEmitsIdlePingAfterCommit(t *testing.T) {
	first := make(chan struct{})
	release := make(chan struct{})
	transport := &blockingMessageTransport{firstWritten: first, release: release}
	service := testMessagesService(t, transport)
	service.heartbeatInterval = 5 * time.Millisecond
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4-mini:low","max_tokens":1,"messages":[{"role":"user","content":"x"}],"stream":true}`))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		service.ServeHTTP(response, request)
		close(done)
	}()
	<-first
	time.Sleep(20 * time.Millisecond)
	close(release)
	<-done
	if !strings.Contains(response.Body.String(), "event: ping\ndata: {\"type\":\"ping\"}") {
		t.Fatalf("stream missing ping: %s", response.Body.String())
	}
}

func testMessagesService(t *testing.T, transport engine.Transport) *MessagesService {
	t.Helper()
	controller, err := retry.NewReal(retry.Config{
		BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, RetryAfterLimit: time.Second,
		Budget: 100, BudgetWindow: time.Minute, FailureThreshold: 20, CircuitCooldown: time.Second,
	}, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	turnEngine := &engine.Engine{Transport: transport, Retry: controller, Wait: func(context.Context, int, string) error { return nil }}
	service, err := NewMessages(MessagesOptions{
		Catalog:      fallbackResolver(t),
		Status:       clodexstatus.New("test"),
		DefaultModel: "gpt-5.6-sol:medium",
		Engine:       turnEngine,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func messageSuccessSSE(text string) []byte {
	encoded, _ := json.Marshal(text)
	return []byte("event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"output_index\":0,\"content_index\":0,\"text\":" + string(encoded) + "}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
}

func assertOrder(t *testing.T, source string, markers ...string) {
	t.Helper()
	offset := 0
	for _, marker := range markers {
		index := strings.Index(source[offset:], marker)
		if index < 0 {
			t.Fatalf("%q missing after offset %d", marker, offset)
		}
		offset += index + len(marker)
	}
}

type messageTransport struct {
	mu       sync.Mutex
	status   int
	body     []byte
	attempts int
	sessions []upstream.Session
	requests []codexwire.Request
}

func (transport *messageTransport) Stream(_ context.Context, session upstream.Session, request codexwire.Request) (*http.Response, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.attempts++
	transport.sessions = append(transport.sessions, session)
	transport.requests = append(transport.requests, request)
	statusCode := transport.status
	if statusCode == 0 {
		statusCode = http.StatusOK
	}
	return &http.Response{StatusCode: statusCode, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(transport.body))}, nil
}

func (transport *messageTransport) Attempts() int {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.attempts
}

func (transport *messageTransport) Sessions() []upstream.Session {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]upstream.Session(nil), transport.sessions...)
}

func (transport *messageTransport) Requests() []codexwire.Request {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return append([]codexwire.Request(nil), transport.requests...)
}

type blockingMessageTransport struct {
	firstWritten chan struct{}
	release      chan struct{}
}

func (transport *blockingMessageTransport) Stream(ctx context.Context, _ upstream.Session, _ codexwire.Request) (*http.Response, error) {
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"a\"}\n\n"))
		close(transport.firstWritten)
		select {
		case <-transport.release:
			_, _ = writer.Write(messageSuccessSSE("b"))
		case <-ctx.Done():
		}
		_ = writer.Close()
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
}

func TestMessagesStreamingRecordsTimingSample(t *testing.T) {
	transport := &messageTransport{body: messageSuccessSSE("timed")}
	service := testMessagesService(t, transport)
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tick := 0
	service.now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4-mini:low","max_tokens":1,"messages":[{"role":"user","content":"x"}],"stream":true}`))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	timing := service.status.Snapshot().Timing
	if timing.Count != 1 || len(timing.Samples) != 1 {
		t.Fatalf("timing = %+v, want exactly one sample", timing)
	}
	sample := timing.Samples[0]
	if sample.TTFTMillis <= 0 {
		t.Errorf("ttft_ms = %d, want > 0 (stepping clock)", sample.TTFTMillis)
	}
	if sample.TotalMillis < sample.TTFTMillis {
		t.Errorf("total_ms = %d, want >= ttft_ms %d", sample.TotalMillis, sample.TTFTMillis)
	}
	if sample.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", sample.Attempts)
	}
}

func TestMessagesBufferedRecordsTimingSample(t *testing.T) {
	transport := &messageTransport{body: messageSuccessSSE("timed")}
	service := testMessagesService(t, transport)
	base := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tick := 0
	service.now = func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4-mini:low","max_tokens":1,"messages":[{"role":"user","content":"x"}]}`))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", response.Code, response.Body.String())
	}
	timing := service.status.Snapshot().Timing
	if timing.Count != 1 || len(timing.Samples) != 1 {
		t.Fatalf("timing = %+v, want exactly one sample", timing)
	}
	sample := timing.Samples[0]
	if sample.TTFTMillis <= 0 || sample.TotalMillis <= 0 {
		t.Errorf("sample = %+v, want positive ttft/total", sample)
	}
	if sample.CommitMillis != 0 {
		t.Errorf("commit_ms = %d, want 0 (buffered turns never commit)", sample.CommitMillis)
	}
}

func TestMessagesFailureRecordsNoTimingSample(t *testing.T) {
	transport := &messageTransport{status: http.StatusForbidden, body: []byte(`{"error":{"message":"account cannot use model"}}`)}
	service := testMessagesService(t, transport)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.4-mini:low","max_tokens":1,"messages":[{"role":"user","content":"x"}],"stream":true}`))
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if timing := service.status.Snapshot().Timing; timing.Count != 0 || len(timing.Samples) != 0 {
		t.Fatalf("timing = %+v, want no samples on failure (retry counters cover failures)", timing)
	}
}
