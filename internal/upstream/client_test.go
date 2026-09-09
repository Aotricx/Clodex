package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/oauth"
)

func TestClientSendsPinnedCodexHTTPContract(t *testing.T) {
	t.Parallel()

	var got requestCapture
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		got = requestCapture{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()

	client := testClient(t, server)
	credentials, err := client.Auth.Ensure(context.Background())
	if err != nil {
		t.Fatalf("load test credentials: %v", err)
	}
	response, err := client.Stream(context.Background(), Session{
		SessionID: "11111111-1111-4111-8111-111111111111",
		ThreadID:  "22222222-2222-4222-8222-222222222222",
	}, codexwire.Request{
		Model:        "gpt-5.4-mini",
		Instructions: "be exact",
		Input:        []codexwire.InputItem{},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer response.Body.Close()

	if got.method != http.MethodPost || got.path != "/backend-api/codex/responses" {
		t.Fatalf("request = %s %s", got.method, got.path)
	}
	wantHeaders := map[string]string{
		"Authorization":       "Bearer " + credentials.AccessToken(),
		"ChatGPT-Account-ID":  "acct_test",
		"Originator":          "codex_cli_rs",
		"Version":             CodexProtocolVersion,
		"Session-ID":          "11111111-1111-4111-8111-111111111111",
		"Thread-ID":           "22222222-2222-4222-8222-222222222222",
		"X-Client-Request-ID": "22222222-2222-4222-8222-222222222222",
		"Accept":              "text/event-stream",
		"Content-Type":        "application/json",
	}
	for name, want := range wantHeaders {
		if value := got.header.Get(name); value != want {
			t.Errorf("%s = %q, want %q", name, value, want)
		}
	}
	if userAgent := got.header.Get("User-Agent"); !strings.Contains(userAgent, "codex_cli_rs/"+CodexProtocolVersion) ||
		!strings.Contains(userAgent, runtime.GOOS) || !strings.Contains(userAgent, runtime.GOARCH) {
		t.Errorf("User-Agent = %q", userAgent)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &envelope); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if string(envelope["store"]) != "false" || string(envelope["stream"]) != "true" {
		t.Fatalf("store/stream = %s/%s", envelope["store"], envelope["stream"])
	}
}

func TestClientRejectsNonHTTPSNonLoopbackEndpoint(t *testing.T) {
	t.Parallel()
	client := testClientWithoutServer(t)
	client.Endpoint = "http://example.com/backend-api/codex/responses"
	_, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "HTTPS") {
		t.Fatalf("Stream error = %v, want HTTPS rejection", err)
	}
}

func TestClientEmptyEndpointUsesDefaultEndpoint(t *testing.T) {
	t.Parallel()
	client := testClientWithoutServer(t)
	client.Endpoint = ""
	parsed, err := client.endpointURL()
	if err != nil {
		t.Fatalf("empty Endpoint: %v", err)
	}
	if parsed.String() != DefaultEndpoint {
		t.Fatalf("endpoint = %q, want %q", parsed.String(), DefaultEndpoint)
	}
}

func TestClientRejectsProductionEndpointWithNonDefaultPort(t *testing.T) {
	t.Parallel()
	client := testClientWithoutServer(t)
	client.Endpoint = "https://chatgpt.com:444/backend-api/codex/responses"
	_, err := client.endpointURL()
	if err == nil || !strings.Contains(err.Error(), "HTTPS chatgpt.com") {
		t.Fatalf("endpointURL() error = %v, want Host pin rejection of chatgpt.com:444", err)
	}
}

func TestClientClassifiesCredentialFailuresWithoutLeakingSecrets(t *testing.T) {
	client := testClientWithoutServer(t)
	client.Auth = &auth.Coordinator{Store: &auth.Store{Path: filepath.Join(t.TempDir(), "missing-auth.json")}}
	_, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
	var authenticationError *AuthenticationError
	if !errors.As(err, &authenticationError) {
		t.Fatalf("Stream() error = %T %v, want *AuthenticationError", err, err)
	}
	if strings.Contains(err.Error(), "access_token") || strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("authentication error leaked credential field: %v", err)
	}
}

func TestClientSetsResponsesLiteHeaderFromConcreteRequest(t *testing.T) {
	t.Parallel()
	header := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header <- r.Header.Get("X-OpenAI-Internal-Codex-Responses-Lite")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()

	client := testClient(t, server)
	response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{
		Model:     "gpt-5.6-sol",
		Reasoning: &codexwire.Reasoning{Context: codexwire.ReasoningContextAllTurns},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if got := <-header; got != "true" {
		t.Fatalf("responses-lite header = %q, want true", got)
	}
}

func TestClientDumpsFirst401ThenRefreshesExactlyOnce(t *testing.T) {
	t.Parallel()
	dumpDir := t.TempDir()
	newAccess := testJWT(map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix(), "chatgpt_account_id": "acct_test", "rotation": 2})
	var responses int
	var refreshes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			refreshes++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oauth.TokenSet{AccessToken: newAccess})
		case "/backend-api/codex/responses":
			responses++
			if responses == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"expired access token"}}`)
				return
			}
			if got := r.Header.Get("Authorization"); got != "Bearer "+newAccess {
				t.Errorf("second Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := testClient(t, server)
	client.DumpDir = dumpDir
	client.Auth.OAuth = &oauth.Client{Issuer: server.URL, HTTPClient: server.Client()}
	response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if responses != 2 || refreshes != 1 {
		t.Fatalf("responses/refreshes = %d/%d, want 2/1", responses, refreshes)
	}
	files, err := filepath.Glob(filepath.Join(dumpDir, "*.wire.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("401 dumps = %#v, %v; want first failure dumped", files, err)
	}
}

func TestClientAutomaticallyDumpsRedactedNonSuccessPair(t *testing.T) {
	t.Parallel()
	dumpDir := t.TempDir()
	secretJWT := testJWT(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "secret": "upstream"})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Debug-Account-ID", "acct_test")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"failed for Bearer fake-access-token","access_token":"`+secretJWT+`"}}`)
	}))
	defer server.Close()

	client := testClient(t, server)
	client.DumpDir = dumpDir
	response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{
		Model: "gpt-5.4-mini",
		Input: []codexwire.InputItem{codexwire.Message{Role: "user", Content: []codexwire.ContentItem{
			codexwire.InputText{Text: "token=private-prompt-secret"},
		}}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(body), "failed for Bearer fake-access-token") {
		t.Fatalf("preserved body = %q, %v", body, err)
	}

	files, err := filepath.Glob(filepath.Join(dumpDir, "*.wire.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("dump files = %#v, %v", files, err)
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("dump mode = %o", info.Mode().Perm())
	}
	dump, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fake-access-token", secretJWT, "acct_test", "private-prompt-secret"} {
		if strings.Contains(string(dump), secret) {
			t.Errorf("dump contains secret %q: %s", secret, dump)
		}
	}
	if !strings.Contains(string(dump), "<redacted>") || !strings.Contains(string(dump), `"status": 502`) {
		t.Fatalf("dump missing redaction/status: %s", dump)
	}
}

func TestClientDumpsSemanticRetryWirePairWithoutDebugMode(t *testing.T) {
	dumpDir := t.TempDir()
	client := testClientWithoutServer(t)
	client.DumpDir = dumpDir
	client.DebugWire = false
	client.DumpRetry(
		context.Background(),
		Session{SessionID: "session", ThreadID: "thread"},
		codexwire.Request{Model: "gpt-5.4-mini", Instructions: "access_token=prompt-secret"},
		"empty_completion",
		1,
		json.RawMessage(`{"type":"response.completed","authorization":"Bearer response-secret"}`),
	)

	files, err := filepath.Glob(filepath.Join(dumpDir, "*.wire.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("dump files = %#v, %v", files, err)
	}
	dump, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"prompt-secret", "response-secret"} {
		if strings.Contains(string(dump), secret) {
			t.Errorf("semantic retry dump leaked %q: %s", secret, dump)
		}
	}
	for _, marker := range []string{"empty_completion", "gpt-5.4-mini", "<redacted>"} {
		if !strings.Contains(string(dump), marker) {
			t.Errorf("semantic retry dump missing %q: %s", marker, dump)
		}
	}
}

func TestClientIsSafeForConcurrentSessions(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()
	client := testClient(t, server)

	const sessions = 32
	var wg sync.WaitGroup
	errs := make(chan error, sessions)
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := client.Stream(context.Background(), Session{SessionID: "session", ThreadID: "thread"}, codexwire.Request{Model: "m"})
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientReusesDefaultHTTPTransport(t *testing.T) {
	t.Parallel()
	client := testClientWithoutServer(t)
	first := client.httpClient()
	second := client.httpClient()
	if first != second {
		t.Fatal("default HTTP client was rebuilt; connection pooling would be lost")
	}
}

func TestClientDoesNotFollowRedirectsAndTreatsThemAsProtocolFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		status int
	}{
		{name: "307", status: http.StatusTemporaryRedirect},
		{name: "308", status: http.StatusPermanentRedirect},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				if r.URL.Path == "/backend-api/codex/responses" {
					http.Redirect(w, r, "/hijacked", tt.status)
					return
				}
				t.Errorf("followed redirect to %s with Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
			}))
			defer server.Close()

			client := testClientWithoutServer(t)
			client.Endpoint = server.URL + "/backend-api/codex/responses"
			response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream error = %v, want (response, nil) like other non-2xx protocol failures", err)
			}
			defer response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				t.Fatalf("StatusCode = %d, want non-2xx so engine uses failure.FromHTTP", response.StatusCode)
			}
			if response.StatusCode != tt.status {
				t.Fatalf("StatusCode = %d, want %d", response.StatusCode, tt.status)
			}
			mu.Lock()
			got := append([]string(nil), paths...)
			mu.Unlock()
			if len(got) != 1 || got[0] != "/backend-api/codex/responses" {
				t.Fatalf("request paths = %#v, want only the original POST (no follow)", got)
			}
		})
	}
}

func TestClientRejectsSuccessWithoutEventStream(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "json", contentType: "application/json", body: `{"ok":true,"choices":[]}`},
		{name: "html", contentType: "text/html", body: `<html><body>ok</body></html>`},
		{name: "empty content-type", contentType: "", body: `{"ok":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()

			client := testClient(t, server)
			response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream error = %v, want (response, nil) like other non-2xx protocol failures", err)
			}
			defer response.Body.Close()
			if response.StatusCode >= 200 && response.StatusCode < 300 {
				t.Fatalf("StatusCode = %d, want non-2xx so engine uses failure.FromHTTP", response.StatusCode)
			}
			if response.StatusCode != http.StatusUnsupportedMediaType {
				t.Fatalf("StatusCode = %d, want 415 Unsupported Media Type", response.StatusCode)
			}
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "event:") {
				t.Fatalf("returned body looks like SSE: %q", body)
			}
		})
	}
}

func TestClientAcceptsEventStreamWithCharsetParameter(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()

	client := testClient(t, server)
	response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200 for text/event-stream with charset", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("body = %q, %v", body, err)
	}
}

// The Codex backend streams /responses with no Content-Type header at all, so
// requiring one rejected every successful turn and relabeled it 415.
func TestClientAcceptsEventStreamWithoutContentType(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	defer server.Close()

	client := testClient(t, server)
	response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200 for an SSE body with no Content-Type", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("body = %q, %v", body, err)
	}
}

func TestClientSkipsWireDumpWhenDoCanceledOrDeadlineExceeded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want error
		arm  func(started <-chan struct{}) (context.Context, context.CancelFunc)
	}{
		{
			name: "canceled",
			want: context.Canceled,
			arm: func(started <-chan struct{}) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				go func() {
					<-started
					cancel()
				}()
				return ctx, cancel
			},
		},
		{
			name: "deadline exceeded",
			want: context.DeadlineExceeded,
			arm: func(started <-chan struct{}) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				go func() {
					<-started
					// Deadline is already armed; RoundTrip waits on ctx.Done().
				}()
				return ctx, cancel
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			started := make(chan struct{})
			dumpDir := t.TempDir()
			client := testClientWithoutServer(t)
			client.DumpDir = dumpDir
			client.Endpoint = "http://127.0.0.1/backend-api/codex/responses"
			client.HTTPClient = &http.Client{Transport: &blockingRoundTripper{started: started}}
			ctx, cancel := tt.arm(started)
			defer cancel()

			_, err := client.Stream(ctx, Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Stream error = %v, want %v", err, tt.want)
			}
			files, globErr := filepath.Glob(filepath.Join(dumpDir, "*.wire.json"))
			if globErr != nil {
				t.Fatal(globErr)
			}
			if len(files) != 0 {
				t.Fatalf("wire dumps after %v = %#v, want none", tt.want, files)
			}
		})
	}
}

func TestDumpFailureNeverMasksRealUpstreamResponse(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"real upstream outage"}}`)
	}))
	defer server.Close()

	notDirectory := filepath.Join(t.TempDir(), "regular-file")
	if err := os.WriteFile(notDirectory, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	dumpErrors := make(chan error, 1)
	client := testClient(t, server)
	client.DumpDir = notDirectory
	client.OnDumpError = func(err error) { dumpErrors <- err }
	response, err := client.Stream(context.Background(), Session{SessionID: "s", ThreadID: "t"}, codexwire.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Stream masked upstream response: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "real upstream outage") {
		t.Fatalf("response = %d %q, %v", response.StatusCode, body, err)
	}
	select {
	case dumpErr := <-dumpErrors:
		if dumpErr == nil || strings.Contains(dumpErr.Error(), "real upstream outage") {
			t.Fatalf("dump callback error = %v", dumpErr)
		}
	case <-time.After(time.Second):
		t.Fatal("dump failure callback not invoked")
	}
}

func TestDebugCaptureMarksBodyClosedBeforeEOFAsTruncated(t *testing.T) {
	t.Parallel()
	finished := make(chan bool, 1)
	body := &capturingBody{
		ReadCloser: io.NopCloser(strings.NewReader("unread response")),
		limit:      1024,
		finish: func(_ []byte, truncated bool) {
			finished <- truncated
		},
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if truncated := <-finished; !truncated {
		t.Fatal("body closed before EOF was recorded as complete")
	}
}

type requestCapture struct {
	method string
	path   string
	header http.Header
	body   []byte
}

type blockingRoundTripper struct {
	started chan struct{}
	once    sync.Once
}

func (t *blockingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.once.Do(func() { close(t.started) })
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func testClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client := testClientWithoutServer(t)
	client.HTTPClient = server.Client()
	client.Endpoint = server.URL + "/backend-api/codex/responses"
	return client
}

func testClientWithoutServer(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	exp := time.Now().Add(24 * time.Hour).Unix()
	idToken := testJWT(map[string]any{"exp": exp, "chatgpt_account_id": "acct_test"})
	accessToken := testJWT(map[string]any{"exp": exp, "chatgpt_account_id": "acct_test"})
	contents := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": idToken, "access_token": accessToken,
			"refresh_token": "fake-refresh-token", "account_id": "acct_test",
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.Marshal(contents)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return &Client{Auth: &auth.Coordinator{Store: &auth.Store{Path: path}}}
}

func testJWT(payload map[string]any) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body, _ := json.Marshal(payload)
	return header + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}
