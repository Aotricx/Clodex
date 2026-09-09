package catalog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/oauth"
)

func TestDiscoveryFetchUsesExactEndpointHeadersAndLiveEnvelope(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 123, time.UTC)
	coordinator, access := discoveryAuth(t, now, "account-test", nil)
	var requestSnapshot struct {
		method, path, rawQuery string
		header                 http.Header
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestSnapshot.method = r.Method
		requestSnapshot.path = r.URL.Path
		requestSnapshot.rawQuery = r.URL.RawQuery
		requestSnapshot.header = r.Header.Clone()
		w.Header().Set("ETag", `W/"models-7"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, liveModelsJSON("live-model"))
	}))
	defer server.Close()

	client := &DiscoveryClient{
		HTTPClient: server.Client(),
		Auth:       coordinator,
		Endpoint:   server.URL + "/backend-api/codex/models",
		Now:        func() time.Time { return now },
	}
	got, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch() error = %v", err)
	}
	if requestSnapshot.method != http.MethodGet || requestSnapshot.path != "/backend-api/codex/models" || requestSnapshot.rawQuery != "client_version=0.144.6" {
		t.Fatalf("request target = %s %s?%s", requestSnapshot.method, requestSnapshot.path, requestSnapshot.rawQuery)
	}
	for name, want := range map[string]string{
		"Authorization":      "Bearer " + access,
		"ChatGPT-Account-ID": "account-test",
		"originator":         "codex_cli_rs",
		"version":            ClientVersion,
	} {
		if value := requestSnapshot.header.Get(name); value != want {
			t.Errorf("header %s = %q, want %q", name, value, want)
		}
	}
	userAgent := requestSnapshot.header.Get("User-Agent")
	if !strings.Contains(userAgent, "codex_cli_rs/"+ClientVersion) || !strings.Contains(userAgent, runtime.GOOS+" "+runtime.GOARCH) || !strings.Contains(userAgent, "clodex/0.1.0") {
		t.Errorf("User-Agent = %q", userAgent)
	}
	if got.Source != SourceLive || !got.FetchedAt.Equal(now) || got.ETag != `W/"models-7"` || got.ClientVersion != ClientVersion || got.Backend != BackendURL {
		t.Fatalf("live metadata = source %q fetched %s etag %q client %q backend %q", got.Source, got.FetchedAt, got.ETag, got.ClientVersion, got.Backend)
	}
	if len(got.Models) != 1 || got.Models[0].Slug != "live-model" || len(got.Models[0].Raw) == 0 {
		t.Fatalf("models = %#v", got.Models)
	}
}

func TestDiscoveryFetchRejectsInvalidEndpointsAndResponses(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	coordinator, _ := discoveryAuth(t, now, "account-test", nil)
	for _, endpoint := range []string{
		"http://chatgpt.com/backend-api/codex/models",
		"https://example.com/backend-api/codex/models",
		"https://chatgpt.com:444/backend-api/codex/models",
		"https://chatgpt.com/backend-api/codex/responses",
		"https://chatgpt.com/backend-api/codex/models?other=1",
		"ftp://127.0.0.1/models",
	} {
		t.Run(endpoint, func(t *testing.T) {
			client := &DiscoveryClient{Auth: coordinator, Endpoint: endpoint, Now: func() time.Time { return now }}
			if _, err := client.endpointURL(); err == nil {
				t.Fatal("endpointURL() error = nil")
			}
		})
	}

	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
		want       string
	}{
		{name: "upstream status", status: 503, body: `{"detail":"catalog unavailable"}`, wantStatus: 503, want: "catalog unavailable"},
		{name: "malformed JSON", status: 200, body: `{"models":[`, want: "decode"},
		{name: "missing models", status: 200, body: `{}`, want: "models"},
		{name: "invalid model", status: 200, body: `{"models":[{"slug":"bad"}]}`, want: "context_window"},
		{name: "trailing JSON", status: 200, body: liveModelsJSON("m") + `{}`, want: "trailing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := &DiscoveryClient{HTTPClient: server.Client(), Auth: coordinator, Endpoint: server.URL + "/models", Now: func() time.Time { return now }}
			_, err := client.Fetch(context.Background())
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("Fetch() error = %v, want containing %q", err, tc.want)
			}
			if tc.wantStatus != 0 {
				var responseErr *DiscoveryHTTPError
				if !errors.As(err, &responseErr) || responseErr.StatusCode != tc.wantStatus {
					t.Fatalf("error = %#v, want DiscoveryHTTPError status %d", err, tc.wantStatus)
				}
			}
		})
	}
}

func TestDiscoveryFetchRedactsSecretsFromHTTPError(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	coordinator, access := discoveryAuth(t, now, "account-secret", nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"message":"Bearer %s account_id=account-secret"}}`, access))
	}))
	defer server.Close()

	client := &DiscoveryClient{HTTPClient: server.Client(), Auth: coordinator, Endpoint: server.URL + "/models", Now: func() time.Time { return now }}
	_, err := client.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch() error = nil")
	}
	message := err.Error()
	for _, secret := range []string{access, "account-secret"} {
		if strings.Contains(message, secret) {
			t.Fatalf("Fetch() error leaked %q: %s", secret, message)
		}
	}
	if !strings.Contains(message, "<redacted>") {
		t.Fatalf("Fetch() error = %q, want redaction marker", message)
	}
}

func TestDiscoveryFetchForcesExactlyOneRefreshOn401(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	newAccess := discoveryJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "generation": "new"})
	var authorityCalls atomic.Int64
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorityCalls.Add(1)
		if r.URL.Path != "/oauth/token" {
			t.Errorf("authority path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oauth.TokenSet{AccessToken: newAccess})
	}))
	defer authority.Close()
	coordinator, oldAccess := discoveryAuth(t, now, "account-test", &oauth.Client{Issuer: authority.URL, HTTPClient: authority.Client()})

	var attempts atomic.Int64
	var tokensMu sync.Mutex
	var tokens []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := attempts.Add(1)
		tokensMu.Lock()
		tokens = append(tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		tokensMu.Unlock()
		if attempt <= 2 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"detail":"expired"}`)
			return
		}
		_, _ = io.WriteString(w, liveModelsJSON("unreachable"))
	}))
	defer server.Close()
	client := &DiscoveryClient{HTTPClient: server.Client(), Auth: coordinator, Endpoint: server.URL + "/models", Now: func() time.Time { return now }}
	_, err := client.Fetch(context.Background())
	var responseErr *DiscoveryHTTPError
	if !errors.As(err, &responseErr) || responseErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("Fetch() error = %#v", err)
	}
	if attempts.Load() != 2 || authorityCalls.Load() != 1 {
		t.Fatalf("model attempts = %d, authority calls = %d; want 2, 1", attempts.Load(), authorityCalls.Load())
	}
	if !reflect.DeepEqual(tokens, []string{oldAccess, newAccess}) {
		t.Fatalf("bearer sequence = %#v", tokens)
	}
}

func TestManagerResolutionOrderAndHonestTelemetry(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	t.Run("fresh matching cache wins without network or auth", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		cached := validCatalog()
		cached.FetchedAt = now.Add(-time.Minute)
		cached.Models[0].Slug = "fresh-cache"
		if err := Save(path, cached); err != nil {
			t.Fatal(err)
		}
		manager := &Manager{CachePath: path, Discovery: &DiscoveryClient{}, Now: func() time.Time { return now }}
		got, err := manager.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.Source != SourceCache || got.Age != time.Minute || got.Catalog.Models[0].Slug != "fresh-cache" || got.LiveError != nil || got.CacheError != nil {
			t.Fatalf("resolution = %#v", got)
		}
	})

	t.Run("live success wins and atomically refreshes cache", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nested", "models.json")
		manager, calls, closeServer := liveManager(t, now, path, http.StatusOK, liveModelsJSON("remote"))
		defer closeServer()
		got, err := manager.Resolve(context.Background())
		if err != nil || got.Source != SourceLive || got.Catalog.Models[0].Slug != "remote" || got.Age != 0 || got.LiveError != nil || got.CacheError != nil {
			t.Fatalf("Resolve() = %#v, %v", got, err)
		}
		if calls.Load() != 1 {
			t.Fatalf("network calls = %d", calls.Load())
		}
		cached, err := LoadFile(path, SourceCache)
		if err != nil || cached.Models[0].Slug != "remote" || !cached.FetchedAt.Equal(now) {
			t.Fatalf("saved cache = %#v, %v", cached, err)
		}
		if runtime.GOOS != "windows" {
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("cache permissions = %#v, %v", info, err)
			}
		}
	})

	t.Run("live failure uses valid stale cache and reports failure", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		cached := validCatalog()
		cached.FetchedAt = now.Add(-time.Hour)
		cached.Models[0].Slug = "stale-cache"
		if err := Save(path, cached); err != nil {
			t.Fatal(err)
		}
		manager, calls, closeServer := liveManager(t, now, path, http.StatusServiceUnavailable, `{"detail":"offline"}`)
		defer closeServer()
		got, err := manager.Resolve(context.Background())
		if err != nil || got.Source != SourceCache || got.Age != time.Hour || got.Catalog.Models[0].Slug != "stale-cache" || got.LiveError == nil || got.CacheError != nil {
			t.Fatalf("Resolve() = %#v, %v", got, err)
		}
		if calls.Load() != 1 || !strings.Contains(got.LiveError.Error(), "offline") {
			t.Fatalf("calls/error = %d/%v", calls.Load(), got.LiveError)
		}
	})

	t.Run("invalid cache plus invalid live uses fallback and reports both", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		if err := os.WriteFile(path, []byte(`{"models":[`), 0o600); err != nil {
			t.Fatal(err)
		}
		manager, _, closeServer := liveManager(t, now, path, http.StatusOK, `{"models":[{"slug":"invalid"}]}`)
		defer closeServer()
		got, err := manager.Resolve(context.Background())
		if err != nil || got.Source != SourceFallback || len(got.Catalog.Models) != 6 || got.CacheError == nil || got.LiveError == nil {
			t.Fatalf("Resolve() = source %q models %d cacheErr %v liveErr %v finalErr %v", got.Source, len(got.Catalog.Models), got.CacheError, got.LiveError, err)
		}
	})

	t.Run("identity-mismatched valid cache is used when live fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		cached := validCatalog()
		cached.ClientVersion = "0.150.0"
		cached.FetchedAt = now.Add(-time.Hour)
		cached.Models[0].Slug = "stale-mismatch"
		if err := Save(path, cached); err != nil {
			t.Fatal(err)
		}
		manager, calls, closeServer := liveManager(t, now, path, http.StatusServiceUnavailable, `{"detail":"offline"}`)
		defer closeServer()
		got, err := manager.Resolve(context.Background())
		if err != nil || got.Source != SourceCache || got.Age != time.Hour || got.Catalog.Models[0].Slug != "stale-mismatch" || got.LiveError == nil || got.CacheError != nil {
			t.Fatalf("Resolve() = %#v, %v", got, err)
		}
		if calls.Load() != 1 || !strings.Contains(got.LiveError.Error(), "offline") {
			t.Fatalf("calls/error = %d/%v", calls.Load(), got.LiveError)
		}
	})

	t.Run("imported Codex CLI cache without backend is used when live fails", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "models.json")
		imported := `{
			"fetched_at":"2026-07-21T17:00:00Z",
			"etag":"etag-cli",
			"client_version":"0.150.0",
			"models":[{
				"slug":"codex-150-cache",
				"display_name":"Codex CLI Cache",
				"description":"Imported Codex CLI models cache.",
				"default_reasoning_level":"medium",
				"supported_reasoning_levels":[{"effort":"medium"}],
				"context_window":100,
				"max_context_window":200,
				"effective_context_window_percent":95,
				"input_modalities":["text"]
			}]
		}`
		if err := os.WriteFile(path, []byte(imported), 0o600); err != nil {
			t.Fatal(err)
		}
		manager, calls, closeServer := liveManager(t, now, path, http.StatusServiceUnavailable, `{"detail":"offline"}`)
		defer closeServer()
		got, err := manager.Resolve(context.Background())
		if err != nil || got.Source != SourceCache || got.Age != time.Hour || got.Catalog.Models[0].Slug != "codex-150-cache" || got.Catalog.ClientVersion != "0.150.0" || got.Catalog.Backend != "" || got.LiveError == nil || got.CacheError != nil {
			t.Fatalf("Resolve() = %#v, %v", got, err)
		}
		if calls.Load() != 1 {
			t.Fatalf("network calls = %d", calls.Load())
		}
	})

	t.Run("cache write failure does not hide valid live catalog", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "destination")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		manager, _, closeServer := liveManager(t, now, path, http.StatusOK, liveModelsJSON("remote"))
		defer closeServer()
		got, err := manager.Resolve(context.Background())
		if err != nil || got.Source != SourceLive || got.Catalog.Models[0].Slug != "remote" || got.CacheError == nil {
			t.Fatalf("Resolve() = %#v, %v", got, err)
		}
	})
}

func TestManagerConcurrentCallersCoalesceRefreshAndReturnDetachedCatalogs(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "models.json")
	coordinator, _ := discoveryAuth(t, now, "account-test", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		_, _ = io.WriteString(w, liveModelsJSON("coalesced"))
	}))
	defer server.Close()
	manager := &Manager{
		CachePath: path,
		Discovery: &DiscoveryClient{HTTPClient: server.Client(), Auth: coordinator, Endpoint: server.URL + "/models", Now: func() time.Time { return now }},
		Now:       func() time.Time { return now },
	}

	const callers = 32
	start := make(chan struct{})
	results := make(chan Resolution, callers)
	errorsCh := make(chan error, callers)
	var wait sync.WaitGroup
	for range callers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			result, err := manager.Resolve(context.Background())
			results <- result
			errorsCh <- err
		}()
	}
	close(start)
	<-entered
	time.Sleep(25 * time.Millisecond)
	close(release)
	wait.Wait()
	close(results)
	close(errorsCh)
	if calls.Load() != 1 {
		t.Fatalf("network calls = %d, want 1", calls.Load())
	}
	var all []Resolution
	for result := range results {
		if result.Source != SourceLive || result.Catalog.Models[0].Slug != "coalesced" || result.LiveError != nil || result.CacheError != nil {
			t.Errorf("resolution = %#v", result)
		}
		all = append(all, result)
	}
	for err := range errorsCh {
		if err != nil {
			t.Errorf("Resolve() error = %v", err)
		}
	}
	all[0].Catalog.Models[0].Slug = "caller-mutation"
	if all[1].Catalog.Models[0].Slug != "coalesced" {
		t.Fatal("coalesced callers share mutable model storage")
	}
}

func TestManagerCanceledWaiterLeavesSharedRefreshRunning(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "models.json")
	coordinator, _ := discoveryAuth(t, now, "account-test", nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, liveModelsJSON("ok"))
	}))
	defer server.Close()
	manager := &Manager{CachePath: path, Discovery: &DiscoveryClient{HTTPClient: server.Client(), Auth: coordinator, Endpoint: server.URL + "/models", Now: func() time.Time { return now }}, Now: func() time.Time { return now }}
	leader := make(chan error, 1)
	go func() {
		_, err := manager.Resolve(context.Background())
		leader <- err
	}()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	waiter := make(chan error, 1)
	go func() {
		_, err := manager.Resolve(ctx)
		waiter <- err
	}()
	cancel()
	if err := <-waiter; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter error = %v", err)
	}
	close(release)
	if err := <-leader; err != nil {
		t.Fatalf("leader error = %v", err)
	}
}

func TestDiscoveryDoesNotFollowRedirects(t *testing.T) {
	now := time.Date(2026, 7, 21, 18, 0, 0, 123, time.UTC)
	coordinator, _ := discoveryAuth(t, now, "account-test", nil)
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var mu sync.Mutex
			destHits := 0
			dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				destHits++
				mu.Unlock()
				t.Errorf("followed redirect to %s with Authorization %q", r.URL.Path, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, liveModelsJSON("hijacked"))
			}))
			t.Cleanup(dest.Close)
			src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, dest.URL+"/models", status)
			}))
			t.Cleanup(src.Close)

			client := &DiscoveryClient{Auth: coordinator, Endpoint: src.URL + "/models", Now: func() time.Time { return now }}
			_, err := client.Fetch(context.Background())
			var httpErr *DiscoveryHTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != status {
				t.Fatalf("Fetch error = %v, want DiscoveryHTTPError %d", err, status)
			}
			mu.Lock()
			hits := destHits
			mu.Unlock()
			if hits != 0 {
				t.Fatalf("destination hits = %d, want 0 (Authorization must not follow redirects)", hits)
			}
		})
	}
}

func liveManager(t *testing.T, now time.Time, cachePath string, status int, body string) (*Manager, *atomic.Int64, func()) {
	t.Helper()
	coordinator, _ := discoveryAuth(t, now, "account-test", nil)
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = io.WriteString(w, body)
	}))
	manager := &Manager{
		CachePath: cachePath,
		Discovery: &DiscoveryClient{HTTPClient: server.Client(), Auth: coordinator, Endpoint: server.URL + "/models", Now: func() time.Time { return now }},
		Now:       func() time.Time { return now },
	}
	return manager, &calls, server.Close
}

func discoveryAuth(t *testing.T, now time.Time, accountID string, oauthClient *oauth.Client) (*auth.Coordinator, string) {
	t.Helper()
	access := discoveryJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "generation": "old"})
	id := discoveryJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": accountID}})
	store := &auth.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	if _, err := store.SaveLogin(oauth.TokenSet{IDToken: id, AccessToken: access, RefreshToken: "refresh-secret", AccountID: accountID}, now); err != nil {
		t.Fatalf("SaveLogin() error = %v", err)
	}
	return &auth.Coordinator{Store: store, OAuth: oauthClient, Now: func() time.Time { return now }}, access
}

func discoveryJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body) + ".signature"
}

func liveModelsJSON(slug string) string {
	return fmt.Sprintf(`{"models":[{
		"slug":%q,"display_name":"Live","description":"Live model.",
		"default_reasoning_level":"medium",
		"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"}],
		"context_window":272000,"max_context_window":272000,
		"effective_context_window_percent":95,
		"service_tiers":[{"id":"priority","name":"Fast"}],
		"default_service_tier":"priority","additional_speed_tiers":["fast"],
		"supports_parallel_tool_calls":true,"supports_image_detail_original":true,
		"input_modalities":["text","image"],"use_responses_lite":false
	}]}`, slug)
}
