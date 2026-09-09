package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/codexwire"
	clodexstatus "github.com/Aotricx/Clodex/internal/status"
	"github.com/Aotricx/Clodex/internal/tokenizer"
)

type catalogResolverFunc func(context.Context) (catalog.Resolution, error)

func (f catalogResolverFunc) Resolve(ctx context.Context) (catalog.Resolution, error) {
	return f(ctx)
}

func TestHandlerHealthRootProbeStatusAndUnknownRoute(t *testing.T) {
	handler, state := testHandler(t, nil)

	root := httptest.NewRecorder()
	handler.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	assertAnthropicError(t, root, http.StatusNotFound, "not_found_error")

	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/", nil))
	if head.Code != http.StatusNotFound || head.Body.Len() != 0 || head.Header().Get("Clodex-Version") != "test-version" || head.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("HEAD / = %d headers=%v body=%q", head.Code, head.Header(), head.Body.String())
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Header().Get("Content-Type") != "application/json" || health.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /healthz = %d headers=%v", health.Code, health.Header())
	}
	var healthBody map[string]string
	if err := json.Unmarshal(health.Body.Bytes(), &healthBody); err != nil || healthBody["status"] != "ok" || healthBody["service"] != "clodex" || healthBody["version"] != "test-version" {
		t.Fatalf("health body = %s, error=%v", health.Body.Bytes(), err)
	}

	state.RecordWarning("test.warning")
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/status", nil))
	var snapshot clodexstatus.Snapshot
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &snapshot); err != nil || snapshot.Version != "test-version" || snapshot.TranslationWarnings.ByKind["test.warning"] != 1 {
		t.Fatalf("status = %s, error=%v", statusResponse.Body.Bytes(), err)
	}

	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/missing", nil))
	assertAnthropicError(t, missing, http.StatusNotFound, "not_found_error")
}

func TestHandlerRefreshesDynamicStatusBeforeSnapshot(t *testing.T) {
	state := clodexstatus.New("test-version")
	var calls atomic.Int64
	handler, err := New(Options{
		Version: "test-version", Status: state, Catalog: fallbackResolver(t), Counter: mustCounter(t), DefaultModel: "gpt-5.6-sol:medium",
		Messages: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		BeforeStatus: func(context.Context) error {
			calls.Add(1)
			state.SetAuth(clodexstatus.AuthSummary{Source: "codex-cli", Account: "sha256:test", Plan: "Pro"})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
	var snapshot clodexstatus.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil || calls.Load() != 1 || snapshot.Auth.Plan != "Pro" {
		t.Fatalf("status = %s calls=%d error=%v", response.Body.Bytes(), calls.Load(), err)
	}

	handler.beforeStatus = func(context.Context) error { return errors.New("auth file unreadable") }
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/status", nil))
	assertAnthropicError(t, failed, http.StatusServiceUnavailable, "api_error")
}

func TestHandlerModelsAndCapturedClaudeCountTokensRequest(t *testing.T) {
	var resolutions atomic.Int64
	handler, state := testHandler(t, catalogResolverFunc(func(context.Context) (catalog.Resolution, error) {
		resolutions.Add(1)
		cat, err := catalog.LoadFallback()
		return catalog.Resolution{Catalog: cat, Source: catalog.SourceFallback}, err
	}))

	models := httptest.NewRecorder()
	handler.ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models?limit=2", nil))
	if models.Code != http.StatusOK {
		t.Fatalf("models = %d %s", models.Code, models.Body.String())
	}
	var list ModelList
	if err := json.Unmarshal(models.Body.Bytes(), &list); err != nil || len(list.Data) != 2 || !list.HasMore {
		t.Fatalf("models = %s, error=%v", models.Body.Bytes(), err)
	}

	countRequest := `{"model":"gpt-5.6-luna:low","messages":[{"role":"user","content":"foo"}],"tools":[]}`
	count := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens?beta=true", strings.NewReader(countRequest))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-claude-code-session-id", "session-count")
	handler.ServeHTTP(count, request)
	if count.Code != http.StatusOK || count.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("count = %d headers=%v body=%s", count.Code, count.Header(), count.Body.String())
	}
	var countBody struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(count.Body.Bytes(), &countBody); err != nil || countBody.InputTokens <= 0 {
		t.Fatalf("count body = %s, error=%v", count.Body.Bytes(), err)
	}
	if resolutions.Load() != 2 {
		t.Fatalf("catalog resolutions = %d, want 2", resolutions.Load())
	}
	snapshot := state.Snapshot()
	if snapshot.Catalog.Source != string(catalog.SourceFallback) || snapshot.TranslationWarnings.ByKind["request.max_tokens_unsupported"] != 0 {
		t.Fatalf("status after count = %#v", snapshot)
	}
}

func TestCountTokensPromptCacheKeyMatchesMessagesSession(t *testing.T) {
	counter := &recordingCounter{}
	handler, err := New(Options{
		Version:      "test-version",
		Status:       clodexstatus.New("test-version"),
		Catalog:      fallbackResolver(t),
		Counter:      counter,
		DefaultModel: "gpt-5.6-sol:medium",
		Messages:     http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionHeader := "session-count"
	countBody := `{"model":"gpt-5.6-luna:low","messages":[{"role":"user","content":[{"type":"text","text":"count","cache_control":{"type":"ephemeral"}}]}]}`
	countRequest := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(countBody))
	countRequest.Header.Set("Content-Type", "application/json")
	countRequest.Header.Set("x-claude-code-session-id", sessionHeader)
	countResponse := httptest.NewRecorder()
	handler.ServeHTTP(countResponse, countRequest)
	if countResponse.Code != http.StatusOK {
		t.Fatalf("count_tokens = %d %s", countResponse.Code, countResponse.Body.String())
	}

	transport := &messageTransport{body: messageSuccessSSE("counted")}
	service := testMessagesService(t, transport)
	messageBody := `{"model":"gpt-5.6-luna:low","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text","text":"count","cache_control":{"type":"ephemeral"}}]}]}`
	messageRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(messageBody))
	messageRequest.Header.Set("Content-Type", "application/json")
	messageRequest.Header.Set("x-claude-code-session-id", sessionHeader)
	messageResponse := httptest.NewRecorder()
	service.ServeHTTP(messageResponse, messageRequest)
	if messageResponse.Code != http.StatusOK {
		t.Fatalf("messages = %d %s", messageResponse.Code, messageResponse.Body.String())
	}
	requests := transport.Requests()
	if len(requests) != 1 {
		t.Fatalf("messages upstream requests = %d", len(requests))
	}
	if counter.request.PromptCacheKey == "" || counter.request.PromptCacheKey != requests[0].PromptCacheKey {
		t.Fatalf("count_tokens prompt_cache_key = %q, messages = %q", counter.request.PromptCacheKey, requests[0].PromptCacheKey)
	}
	session, err := service.session(sessionHeader)
	if err != nil {
		t.Fatal(err)
	}
	if counter.request.PromptCacheKey != session.ThreadID {
		t.Fatalf("count_tokens prompt_cache_key = %q, want thread %q", counter.request.PromptCacheKey, session.ThreadID)
	}
}

type recordingCounter struct {
	request codexwire.Request
}

func (counter *recordingCounter) CountRequest(request codexwire.Request) (int, error) {
	counter.request = request
	return 1, nil
}

func TestHandlerRejectsOversizedJSONBodiesBeforeDecode(t *testing.T) {
	var messagesCalled atomic.Bool
	handler, err := New(Options{
		Version:      "test-version",
		Status:       clodexstatus.New("test-version"),
		Catalog:      fallbackResolver(t),
		Counter:      mustCounter(t),
		DefaultModel: "gpt-5.6-sol:medium",
		Messages: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			messagesCalled.Store(true)
			writer.WriteHeader(http.StatusNoContent)
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { maxJSONBodyBytes = defaultMaxJSONBodyBytes })
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run("content-length "+path, func(t *testing.T) {
			messagesCalled.Store(false)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			request.ContentLength = defaultMaxJSONBodyBytes + 1
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertAnthropicError(t, response, http.StatusRequestEntityTooLarge, "request_too_large")
			if messagesCalled.Load() {
				t.Fatal("messages handler ran for an oversized body")
			}
		})
		t.Run("chunked "+path, func(t *testing.T) {
			messagesCalled.Store(false)
			maxJSONBodyBytes = 64
			t.Cleanup(func() { maxJSONBodyBytes = defaultMaxJSONBodyBytes })
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"`+strings.Repeat("a", 128)+`"}`))
			request.Header.Set("Content-Type", "application/json")
			request.ContentLength = -1
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertAnthropicError(t, response, http.StatusRequestEntityTooLarge, "request_too_large")
			if messagesCalled.Load() {
				t.Fatal("messages handler ran for an oversized body")
			}
		})
	}
}

func TestHandlerValidatesMethodsContentTypeAndJSON(t *testing.T) {
	handler, _ := testHandler(t, nil)
	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		status      int
		errorType   string
		allow       string
	}{
		{name: "health method", method: http.MethodPost, path: "/healthz", status: 405, errorType: "invalid_request_error", allow: "GET, HEAD"},
		{name: "models method", method: http.MethodPost, path: "/v1/models", status: 405, errorType: "invalid_request_error", allow: "GET, HEAD"},
		{name: "count method", method: http.MethodGet, path: "/v1/messages/count_tokens", status: 405, errorType: "invalid_request_error", allow: "POST"},
		{name: "missing content type", method: http.MethodPost, path: "/v1/messages/count_tokens", body: `{}`, status: 400, errorType: "invalid_request_error"},
		{name: "wrong content type", method: http.MethodPost, path: "/v1/messages/count_tokens", contentType: "text/plain", body: `{}`, status: 400, errorType: "invalid_request_error"},
		{name: "malformed JSON", method: http.MethodPost, path: "/v1/messages/count_tokens", contentType: "application/json; charset=utf-8", body: `{"model":`, status: 400, errorType: "invalid_request_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				request.Header.Set("Content-Type", tc.contentType)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertAnthropicError(t, response, tc.status, tc.errorType)
			if tc.allow != "" && response.Header().Get("Allow") != tc.allow {
				t.Fatalf("Allow = %q, want %q", response.Header().Get("Allow"), tc.allow)
			}
		})
	}
}

func TestHandlerDelegatesMessagesWithActiveSessionAndQuery(t *testing.T) {
	state := clodexstatus.New("test-version")
	var called atomic.Bool
	messages := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		if r.URL.RawQuery != "beta=true" || state.Snapshot().ActiveSessions != 1 {
			t.Errorf("message query/sessions = %q/%d", r.URL.RawQuery, state.Snapshot().ActiveSessions)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler, err := New(Options{
		Version:      "test-version",
		Status:       state,
		Catalog:      fallbackResolver(t),
		Counter:      mustCounter(t),
		DefaultModel: "gpt-5.6-sol:medium",
		Messages:     messages,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages?beta=true", bytes.NewBufferString(`{}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if !called.Load() || response.Code != http.StatusNoContent || state.Snapshot().ActiveSessions != 0 {
		t.Fatalf("delegated=%v status=%d sessions=%d", called.Load(), response.Code, state.Snapshot().ActiveSessions)
	}
}

func TestNewHandlerRejectsMissingShippingDependencies(t *testing.T) {
	valid := Options{Version: "v", Status: clodexstatus.New("v"), Catalog: fallbackResolver(t), Counter: mustCounter(t), DefaultModel: "gpt-5.6-sol:medium", Messages: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	tests := []Options{
		{},
		func() Options { value := valid; value.Status = nil; return value }(),
		func() Options { value := valid; value.Catalog = nil; return value }(),
		func() Options { value := valid; value.Counter = nil; return value }(),
		func() Options { value := valid; value.DefaultModel = ""; return value }(),
		func() Options { value := valid; value.Messages = nil; return value }(),
	}
	for index, options := range tests {
		if _, err := New(options); err == nil {
			t.Errorf("New(options[%d]) error = nil", index)
		}
	}
}

func testHandler(t *testing.T, resolver CatalogResolver) (*Handler, *clodexstatus.State) {
	t.Helper()
	if resolver == nil {
		resolver = fallbackResolver(t)
	}
	state := clodexstatus.New("test-version")
	handler, err := New(Options{
		Version:      "test-version",
		Status:       state,
		Catalog:      resolver,
		Counter:      mustCounter(t),
		DefaultModel: "gpt-5.6-sol:medium",
		Messages:     http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, state
}

func fallbackResolver(t *testing.T) CatalogResolver {
	t.Helper()
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	return catalogResolverFunc(func(context.Context) (catalog.Resolution, error) {
		return catalog.Resolution{Catalog: cat, Source: catalog.SourceFallback}, nil
	})
}

func mustCounter(t *testing.T) *tokenizer.Counter {
	t.Helper()
	counter, err := tokenizer.New()
	if err != nil {
		t.Fatal(err)
	}
	return counter
}

func assertAnthropicError(t *testing.T, response *httptest.ResponseRecorder, status int, errorType string) {
	t.Helper()
	if response.Code != status || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Type != "error" || body.Error.Type != errorType || body.Error.Message == "" {
		t.Fatalf("error body = %s, error=%v", response.Body.Bytes(), err)
	}
}
