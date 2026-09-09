package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/engine"
	"github.com/Aotricx/Clodex/internal/oauth"
	"github.com/Aotricx/Clodex/internal/retry"
	clodexstatus "github.com/Aotricx/Clodex/internal/status"
	"github.com/Aotricx/Clodex/internal/upstream"
)

func TestMessagesServiceParallelStreamingAndBufferedSessions(t *testing.T) {
	const sessions = 64
	random := &overlapRejectingReader{}
	service := concurrentMessagesService(t, &messageTransport{body: messageSuccessSSE("parallel")}, random)
	start := make(chan struct{})
	errorsCh := make(chan error, sessions)
	var group sync.WaitGroup

	for index := range sessions {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			stream := index%2 == 0
			body := `{"model":"gpt-5.6-luna:low","max_tokens":32,"messages":[{"role":"user","content":"parallel"}],"stream":` + map[bool]string{false: "false", true: "true"}[stream] + `}`
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			request.Header.Set("x-claude-code-session-id", "parallel-session-"+strconv.Itoa(index))
			response := httptest.NewRecorder()
			service.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "parallel") {
				errorsCh <- errors.New(response.Body.String())
			}
		}()
	}
	close(start)
	group.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("parallel Messages request failed: %v", err)
	}
}

func TestMessagesStreamingSurvivesSlowBackpressuredWriter(t *testing.T) {
	service := concurrentMessagesService(t, &messageTransport{body: messageSuccessSSE(strings.Repeat("x", 4096))}, nil)
	writer := newBackpressuredWriter()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.6-luna:low","max_tokens":8192,"messages":[{"role":"user","content":"slow"}],"stream":true}`))
	done := make(chan struct{})
	go func() {
		service.ServeHTTP(writer, request)
		close(done)
	}()

	select {
	case <-writer.blocked:
	case <-time.After(time.Second):
		t.Fatal("stream never reached backpressured writer")
	}
	select {
	case <-done:
		t.Fatal("stream completed while downstream writer was blocked")
	case <-time.After(20 * time.Millisecond):
	}
	close(writer.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not resume after downstream writer released")
	}
	if writer.status != http.StatusOK || !strings.Contains(writer.body.String(), "event: message_stop") {
		t.Fatalf("slow stream = status %d body %q", writer.status, writer.body.String())
	}
}

func TestMessagesMidStreamCancellationCancelsUpstreamAndReleasesGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()
	transport := &cancelAwareTransport{contextCanceled: make(chan struct{}), bodyDone: make(chan struct{})}
	service := concurrentMessagesService(t, transport, nil)
	requestContext, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-5.6-luna:low","max_tokens":32,"messages":[{"role":"user","content":"cancel"}],"stream":true}`)).WithContext(requestContext)
	writer := &signalWriter{header: make(http.Header), firstWrite: make(chan struct{})}
	handlerDone := make(chan struct{})
	go func() {
		service.ServeHTTP(writer, request)
		close(handlerDone)
	}()

	select {
	case <-writer.firstWrite:
	case <-time.After(time.Second):
		t.Fatal("stream never produced semantic output")
	}
	cancel()
	for name, channel := range map[string]<-chan struct{}{
		"upstream context cancellation": transport.contextCanceled,
		"transport body goroutine":      transport.bodyDone,
		"message handler":               handlerDone,
	} {
		select {
		case <-channel:
		case <-time.After(time.Second):
			t.Fatalf("%s did not finish", name)
		}
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if current := runtime.NumGoroutine(); current > baseline {
		t.Fatalf("goroutines did not return to baseline: before=%d after=%d", baseline, current)
	}
}

func TestHandlerConcurrentStatusCountTokensAndCatalogManagerRefresh(t *testing.T) {
	const callers = 32
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	refreshEntered := make(chan struct{})
	refreshRelease := make(chan struct{})
	var releaseOnce sync.Once
	var discoveryCalls atomic.Int64
	discovery := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if discoveryCalls.Add(1) == 1 {
			close(refreshEntered)
		}
		<-refreshRelease
		_, _ = io.WriteString(writer, concurrentCatalogJSON)
	}))
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(refreshRelease) })
		discovery.Close()
	})

	manager := &catalog.Manager{
		CachePath: filepath.Join(t.TempDir(), "models.json"),
		Discovery: &catalog.DiscoveryClient{
			HTTPClient: discovery.Client(), Auth: concurrentDiscoveryAuth(t, now),
			Endpoint: discovery.URL + "/models", Now: func() time.Time { return now },
		},
		Now: func() time.Time { return now },
	}
	refreshDone := make(chan error, 1)
	go func() {
		_, err := manager.Resolve(context.Background())
		refreshDone <- err
	}()
	select {
	case <-refreshEntered:
	case <-time.After(time.Second):
		t.Fatal("catalog refresh never reached discovery")
	}

	var countResolvers atomic.Int64
	allCountsEntered := make(chan struct{})
	resolver := catalogResolverFunc(func(ctx context.Context) (catalog.Resolution, error) {
		if countResolvers.Add(1) == callers {
			close(allCountsEntered)
		}
		return manager.Resolve(ctx)
	})
	state := clodexstatus.New("test")
	handler, err := New(Options{
		Version: "test", Status: state, Catalog: resolver, Counter: mustCounter(t), DefaultModel: "gpt-5.6-luna:low",
		Messages: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})
	if err != nil {
		t.Fatal(err)
	}

	statusResults := make(chan int, callers)
	countResults := make(chan int, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(2)
		go func() {
			defer group.Done()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/status", nil))
			statusResults <- response.Code
		}()
		go func() {
			defer group.Done()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"gpt-5.6-luna:low","messages":[{"role":"user","content":"count"}]}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			countResults <- response.Code
		}()
	}
	select {
	case <-allCountsEntered:
	case <-time.After(time.Second):
		t.Fatal("count_tokens callers did not join catalog refresh")
	}
	for range callers {
		select {
		case statusCode := <-statusResults:
			if statusCode != http.StatusOK {
				t.Errorf("concurrent /status = %d", statusCode)
			}
		case <-time.After(time.Second):
			t.Fatal("/status blocked behind catalog refresh")
		}
	}
	releaseOnce.Do(func() { close(refreshRelease) })
	group.Wait()
	for range callers {
		if statusCode := <-countResults; statusCode != http.StatusOK {
			t.Errorf("concurrent count_tokens = %d", statusCode)
		}
	}
	if err := <-refreshDone; err != nil {
		t.Fatalf("catalog refresh error = %v", err)
	}
	if got := discoveryCalls.Load(); got != 1 {
		t.Fatalf("catalog discovery calls = %d, want 1", got)
	}
}

type overlapRejectingReader struct {
	active atomic.Bool
	next   atomic.Uint32
}

type backpressuredWriter struct {
	header  http.Header
	status  int
	body    bytes.Buffer
	blocked chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBackpressuredWriter() *backpressuredWriter {
	return &backpressuredWriter{header: make(http.Header), blocked: make(chan struct{}), release: make(chan struct{})}
}

func (writer *backpressuredWriter) Header() http.Header { return writer.header }

func (writer *backpressuredWriter) WriteHeader(status int) { writer.status = status }

func (writer *backpressuredWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.blocked) })
	<-writer.release
	return writer.body.Write(data)
}

func (writer *backpressuredWriter) Flush() {}

type signalWriter struct {
	header     http.Header
	body       bytes.Buffer
	firstWrite chan struct{}
	once       sync.Once
}

func (writer *signalWriter) Header() http.Header { return writer.header }

func (writer *signalWriter) WriteHeader(int) {}

func (writer *signalWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.firstWrite) })
	return writer.body.Write(data)
}

func (writer *signalWriter) Flush() {}

type cancelAwareTransport struct {
	contextCanceled chan struct{}
	bodyDone        chan struct{}
}

func (transport *cancelAwareTransport) Stream(ctx context.Context, _ upstream.Session, _ codexwire.Request) (*http.Response, error) {
	reader, writer := io.Pipe()
	go func() {
		defer close(transport.bodyDone)
		_, _ = writer.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"started\"}\n\n"))
		<-ctx.Done()
		close(transport.contextCanceled)
		_ = writer.CloseWithError(ctx.Err())
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: reader}, nil
}

func (reader *overlapRejectingReader) Read(buffer []byte) (int, error) {
	if !reader.active.CompareAndSwap(false, true) {
		return 0, errors.New("concurrent random read")
	}
	defer reader.active.Store(false)
	time.Sleep(2 * time.Millisecond)
	for index := range buffer {
		buffer[index] = byte(reader.next.Add(1))
	}
	return len(buffer), nil
}

func concurrentMessagesService(t *testing.T, transport engine.Transport, random io.Reader) *MessagesService {
	t.Helper()
	controller, err := retry.NewReal(retry.Config{
		BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond, RetryAfterLimit: time.Second,
		Budget: 1000, BudgetWindow: time.Minute, FailureThreshold: 1000, CircuitCooldown: time.Second,
	}, func() float64 { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewMessages(MessagesOptions{
		Catalog: fallbackResolver(t), Status: clodexstatus.New("test"), DefaultModel: "gpt-5.6-sol:medium",
		Engine: &engine.Engine{Transport: transport, Retry: controller, Wait: func(context.Context, int, string) error { return nil }},
		Random: random,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func concurrentDiscoveryAuth(t *testing.T, now time.Time) *auth.Coordinator {
	t.Helper()
	access := concurrentJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix()})
	id := concurrentJWT(t, map[string]any{
		"exp":                         now.Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "test-account"},
	})
	store := &auth.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	if _, err := store.SaveLogin(oauth.TokenSet{
		IDToken: id, AccessToken: access, RefreshToken: "test-refresh", AccountID: "test-account",
	}, now); err != nil {
		t.Fatal(err)
	}
	return &auth.Coordinator{Store: store, Now: func() time.Time { return now }}
}

func concurrentJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

const concurrentCatalogJSON = `{"models":[{
	"slug":"gpt-5.6-luna","display_name":"Mini","description":"test",
	"default_reasoning_level":"low","supported_reasoning_levels":[{"effort":"low"}],
	"context_window":272000,"max_context_window":272000,"effective_context_window_percent":95,
	"supports_parallel_tool_calls":true,"supports_image_detail_original":true,
	"input_modalities":["text","image"],"use_responses_lite":false
}]}`
