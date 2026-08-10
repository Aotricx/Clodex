package oauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultCallbackPortsExact(t *testing.T) {
	if DefaultCallbackPort != 1455 || FallbackCallbackPort != 1457 {
		t.Fatalf("callback ports = %d, %d", DefaultCallbackPort, FallbackCallbackPort)
	}
	if got := callbackPortCandidates(nil); !reflect.DeepEqual(got, []uint16{1455, 1457}) {
		t.Fatalf("default candidates = %v", got)
	}
}

func TestCallbackListenerBindsIPv4LoopbackOnly(t *testing.T) {
	port := freeCallbackPort(t)
	listener, actualPort, err := listenForCallback(context.Background(), []uint16{port})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) || actualPort != port {
		t.Fatalf("listener = %v, port = %d", listener.Addr(), actualPort)
	}
}

func TestLoginSuccessUsesLiveLoopbackPKCECallback(t *testing.T) {
	port := freeCallbackPort(t)
	entropy := make([]byte, 96)
	for i := range entropy {
		entropy[i] = byte(i)
	}
	wantState := base64.RawURLEncoding.EncodeToString(entropy[64:])
	wantPKCE, err := GeneratePKCE(bytes.NewReader(entropy[:64]))
	if err != nil {
		t.Fatal(err)
	}

	var tokenRequests atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenRequests.Add(1)
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("token request = %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		want := url.Values{
			"grant_type": {"authorization_code"}, "code": {"authorization-secret"},
			"redirect_uri": {fmt.Sprintf("http://localhost:%d/auth/callback", port)},
			"client_id":    {"client"}, "code_verifier": {wantPKCE.Verifier},
		}
		if !reflect.DeepEqual(r.PostForm, want) {
			t.Errorf("token form = %#v, want %#v", r.PostForm, want)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id_token":"id-secret","access_token":"access-secret","refresh_token":"refresh-secret"}`)
	}))
	defer issuer.Close()

	response := make(chan browserResponse, 1)
	c := Client{
		Issuer: issuer.URL, ClientID: "client", HTTPClient: issuer.Client(),
		Random: bytes.NewReader(entropy), CallbackPorts: []uint16{port},
	}
	tokens, err := c.Login(context.Background(), func(authorizeURL string) error {
		u, err := url.Parse(authorizeURL)
		if err != nil {
			return err
		}
		query := u.Query()
		if query.Get("state") != wantState || len(query.Get("state")) != 43 {
			return fmt.Errorf("state = %q", query.Get("state"))
		}
		if query.Get("code_challenge") != wantPKCE.Challenge || query.Get("code_challenge_method") != "S256" {
			return fmt.Errorf("PKCE query = %v", query)
		}
		redirect, err := url.Parse(query.Get("redirect_uri"))
		if err != nil {
			return err
		}
		if redirect.String() != fmt.Sprintf("http://localhost:%d/auth/callback", port) {
			return fmt.Errorf("redirect = %s", redirect)
		}
		connection, err := net.DialTimeout("tcp4", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err != nil {
			return fmt.Errorf("listener not live before opener: %w", err)
		}
		connection.Close()
		go fetchCallback(response, callbackRequestURL(redirect, wantState, "authorization-secret"), http.MethodGet)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if tokens != (TokenSet{IDToken: "id-secret", AccessToken: "access-secret", RefreshToken: "refresh-secret"}) {
		t.Fatalf("tokens = %+v", tokens)
	}
	if tokenRequests.Load() != 1 {
		t.Fatalf("token requests = %d", tokenRequests.Load())
	}
	page := <-response
	if page.err != nil || page.status != http.StatusOK || !strings.Contains(page.body, "Login successful") {
		t.Fatalf("browser response = %+v", page)
	}
	for _, secret := range []string{"authorization-secret", "id-secret", "access-secret", "refresh-secret", wantState, wantPKCE.Verifier} {
		if strings.Contains(page.body, secret) {
			t.Errorf("browser page leaked %q: %s", secret, page.body)
		}
	}
	assertCallbackPortReusable(t, port)
}

func TestLoginFallsBackToSecondConfiguredPort(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	first := uint16(occupied.Addr().(*net.TCPAddr).Port)
	second := freeCallbackPort(t)
	sentinel := errors.New("stop after inspection")
	c := Client{Random: bytes.NewReader(make([]byte, 96)), CallbackPorts: []uint16{first, second}}
	_, err = c.Login(context.Background(), func(authorizeURL string) error {
		u, parseErr := url.Parse(authorizeURL)
		if parseErr != nil {
			return parseErr
		}
		redirect, parseErr := url.Parse(u.Query().Get("redirect_uri"))
		if parseErr != nil {
			return parseErr
		}
		if redirect.Port() != strconv.Itoa(int(second)) {
			return fmt.Errorf("callback port = %s, want %d", redirect.Port(), second)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v", err)
	}
	assertCallbackPortReusable(t, second)
}

func TestLoginStateMismatchDoesNotExchangeOrEndLogin(t *testing.T) {
	port := freeCallbackPort(t)
	var exchanges atomic.Int32
	issuer := tokenIssuer(t, &exchanges)
	defer issuer.Close()
	responses := make(chan browserResponse, 2)
	c := Client{Issuer: issuer.URL, HTTPClient: issuer.Client(), Random: bytes.NewReader(make([]byte, 96)), CallbackPorts: []uint16{port}}
	tokens, err := c.Login(context.Background(), func(authorizeURL string) error {
		redirect, state, err := authorizeCallback(authorizeURL)
		if err != nil {
			return err
		}
		go func() {
			fetchCallback(responses, callbackRequestURL(redirect, "wrong-state", "wrong-code"), http.MethodGet)
			fetchCallback(responses, callbackRequestURL(redirect, state, "authorization-secret"), http.MethodGet)
		}()
		return nil
	})
	if err != nil || tokens.AccessToken != "access-secret" {
		t.Fatalf("tokens/error = %+v / %v", tokens, err)
	}
	bad, good := <-responses, <-responses
	if bad.status != http.StatusBadRequest || !strings.Contains(bad.body, "State mismatch") {
		t.Fatalf("bad-state response = %+v", bad)
	}
	if strings.Contains(bad.body, "wrong-state") || strings.Contains(bad.body, "wrong-code") {
		t.Fatalf("bad-state response leaked callback values: %s", bad.body)
	}
	if good.status != http.StatusOK || exchanges.Load() != 1 {
		t.Fatalf("good response/exchanges = %+v / %d", good, exchanges.Load())
	}
}

func TestLoginHandlesOAuthErrorAndMissingCode(t *testing.T) {
	for _, tc := range []struct {
		name       string
		query      func(string) url.Values
		wantPhrase string
		wantType   bool
	}{
		{
			name: "oauth error",
			query: func(state string) url.Values {
				return url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {"User declined"}}
			},
			wantPhrase: "Login failed",
			wantType:   true,
		},
		{
			name: "missing code",
			query: func(state string) url.Values {
				return url.Values{"state": {state}}
			},
			wantPhrase: "Missing authorization code",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := freeCallbackPort(t)
			response := make(chan browserResponse, 1)
			var expectedState string
			c := Client{Random: bytes.NewReader(make([]byte, 96)), CallbackPorts: []uint16{port}}
			_, err := c.Login(context.Background(), func(authorizeURL string) error {
				redirect, state, parseErr := authorizeCallback(authorizeURL)
				if parseErr != nil {
					return parseErr
				}
				expectedState = state
				redirect.RawQuery = tc.query(state).Encode()
				redirect.Host = "127.0.0.1:" + redirect.Port()
				go fetchCallback(response, redirect.String(), http.MethodGet)
				return nil
			})
			if err == nil {
				t.Fatal("Login returned nil error")
			}
			var callbackErr *OAuthCallbackError
			if tc.wantType && (!errors.As(err, &callbackErr) || callbackErr.Code != "access_denied" || callbackErr.Description != "User declined") {
				t.Fatalf("OAuth error = %T %+v", err, err)
			}
			page := <-response
			if page.status != http.StatusBadRequest || !strings.Contains(page.body, tc.wantPhrase) {
				t.Fatalf("browser response = %+v", page)
			}
			for _, forbidden := range []string{"access_denied", "User declined", expectedState} {
				if forbidden != "" && strings.Contains(page.body, forbidden) {
					t.Errorf("browser response leaked %q: %s", forbidden, page.body)
				}
			}
			assertCallbackPortReusable(t, port)
		})
	}
}

func TestLoginWrongPathAndMethodRemainNonTerminal(t *testing.T) {
	port := freeCallbackPort(t)
	var exchanges atomic.Int32
	issuer := tokenIssuer(t, &exchanges)
	defer issuer.Close()
	responses := make(chan browserResponse, 3)
	c := Client{Issuer: issuer.URL, HTTPClient: issuer.Client(), Random: bytes.NewReader(make([]byte, 96)), CallbackPorts: []uint16{port}}
	_, err := c.Login(context.Background(), func(authorizeURL string) error {
		redirect, state, err := authorizeCallback(authorizeURL)
		if err != nil {
			return err
		}
		go func() {
			wrongPath := *redirect
			wrongPath.Host = "127.0.0.1:" + redirect.Port()
			wrongPath.Path = "/wrong"
			wrongPath.RawQuery = url.Values{"state": {state}, "code": {"wrong-path-code"}}.Encode()
			fetchCallback(responses, wrongPath.String(), http.MethodGet)
			fetchCallback(responses, callbackRequestURL(redirect, state, "wrong-method-code"), http.MethodPost)
			fetchCallback(responses, callbackRequestURL(redirect, state, "authorization-secret"), http.MethodGet)
		}()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongPath, wrongMethod, success := <-responses, <-responses, <-responses
	if wrongPath.status != http.StatusNotFound || wrongMethod.status != http.StatusMethodNotAllowed || success.status != http.StatusOK {
		t.Fatalf("responses = %+v / %+v / %+v", wrongPath, wrongMethod, success)
	}
	if strings.Contains(wrongPath.body+wrongMethod.body, "wrong-path-code") || strings.Contains(wrongPath.body+wrongMethod.body, "wrong-method-code") {
		t.Fatal("invalid-request response leaked code")
	}
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges = %d", exchanges.Load())
	}
}

func TestLoginTokenExchangeErrorPageContainsNoCredentials(t *testing.T) {
	port := freeCallbackPort(t)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_grant","access_token":"token-secret"}`)
	}))
	defer issuer.Close()
	response := make(chan browserResponse, 1)
	c := Client{Issuer: issuer.URL, HTTPClient: issuer.Client(), Random: bytes.NewReader(make([]byte, 96)), CallbackPorts: []uint16{port}}
	_, err := c.Login(context.Background(), func(authorizeURL string) error {
		redirect, state, parseErr := authorizeCallback(authorizeURL)
		if parseErr != nil {
			return parseErr
		}
		go fetchCallback(response, callbackRequestURL(redirect, state, "authorization-secret"), http.MethodGet)
		return nil
	})
	if err == nil {
		t.Fatal("Login returned nil error")
	}
	page := <-response
	if page.status != http.StatusBadGateway || !strings.Contains(page.body, "Login failed") {
		t.Fatalf("browser response = %+v", page)
	}
	for _, secret := range []string{"authorization-secret", "token-secret", "invalid_grant"} {
		if strings.Contains(page.body, secret) {
			t.Errorf("browser response leaked %q: %s", secret, page.body)
		}
	}
}

func TestLoginCancellationTimeoutAndOpenerFailureReleasePort(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(Client) error
	}{
		{
			name: "cancel",
			run: func(c Client) error {
				ctx, cancel := context.WithCancel(context.Background())
				_, err := c.Login(ctx, func(string) error { cancel(); return nil })
				return err
			},
		},
		{
			name: "timeout",
			run: func(c Client) error {
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
				defer cancel()
				_, err := c.Login(ctx, func(string) error { return nil })
				return err
			},
		},
		{
			name: "opener",
			run: func(c Client) error {
				sentinel := errors.New("opener failed")
				_, err := c.Login(context.Background(), func(string) error { return sentinel })
				if !errors.Is(err, sentinel) {
					return fmt.Errorf("got %v, want opener sentinel", err)
				}
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			port := freeCallbackPort(t)
			c := Client{Random: bytes.NewReader(make([]byte, 96)), CallbackPorts: []uint16{port}}
			err := tc.run(c)
			if err == nil {
				t.Fatal("Login returned nil error")
			}
			assertCallbackPortReusable(t, port)
		})
	}
}

func TestOpenBrowserCommandMatrixAndValidation(t *testing.T) {
	for _, tc := range []struct {
		goos string
		name string
		args []string
	}{
		{"darwin", "open", []string{"https://example.test/path?q=one"}},
		{"linux", "xdg-open", []string{"https://example.test/path?q=one"}},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", "https://example.test/path?q=one"}},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			var gotName string
			var gotArgs []string
			runner := func(_ context.Context, name string, args ...string) error {
				gotName, gotArgs = name, append([]string(nil), args...)
				return nil
			}
			if err := openBrowser(context.Background(), tc.goos, runner, "https://example.test/path?q=one"); err != nil {
				t.Fatal(err)
			}
			if gotName != tc.name || !reflect.DeepEqual(gotArgs, tc.args) {
				t.Fatalf("command = %q %#v, want %q %#v", gotName, gotArgs, tc.name, tc.args)
			}
		})
	}

	for _, rawURL := range []string{"file:///tmp/secret", "javascript:alert(1)", "//example.test/path", "https://user:pass@example.test/"} {
		t.Run(rawURL, func(t *testing.T) {
			called := false
			err := openBrowser(context.Background(), runtime.GOOS, func(context.Context, string, ...string) error { called = true; return nil }, rawURL)
			if err == nil || called {
				t.Fatalf("error/called = %v / %v", err, called)
			}
		})
	}
	called := false
	err := openBrowser(context.Background(), "plan9", func(context.Context, string, ...string) error { called = true; return nil }, "https://example.test")
	if err == nil || !strings.Contains(err.Error(), "unsupported") || called {
		t.Fatalf("unsupported error/called = %v / %v", err, called)
	}
}

func TestLoginDoesNotLeakGoroutinesOnCancellation(t *testing.T) {
	before := runtime.NumGoroutine()
	port := freeCallbackPort(t)
	for i := 0; i < 8; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		c := Client{Random: bytes.NewReader(make([]byte, 96)), CallbackPorts: []uint16{port}}
		_, err := c.Login(ctx, func(string) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("iteration %d: error = %v", i, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines before/after = %d/%d", before, after)
	}
}

type browserResponse struct {
	status int
	body   string
	err    error
}

func fetchCallback(result chan<- browserResponse, callbackURL, method string) {
	req, err := http.NewRequest(method, callbackURL, nil)
	if err != nil {
		result <- browserResponse{err: err}
		return
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		result <- browserResponse{err: err}
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	result <- browserResponse{status: resp.StatusCode, body: string(body), err: err}
}

func authorizeCallback(authorizeURL string) (*url.URL, string, error) {
	u, err := url.Parse(authorizeURL)
	if err != nil {
		return nil, "", err
	}
	redirect, err := url.Parse(u.Query().Get("redirect_uri"))
	if err != nil {
		return nil, "", err
	}
	return redirect, u.Query().Get("state"), nil
}

func callbackRequestURL(redirect *url.URL, state, code string) string {
	callback := *redirect
	callback.Host = "127.0.0.1:" + redirect.Port()
	callback.RawQuery = url.Values{"state": {state}, "code": {code}}.Encode()
	return callback.String()
}

func tokenIssuer(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.PostForm.Get("code") != "authorization-secret" {
			t.Errorf("code = %q", r.PostForm.Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id_token":"id-secret","access_token":"access-secret","refresh_token":"refresh-secret"}`)
	}))
}

func freeCallbackPort(t *testing.T) uint16 {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	return port
}

func assertCallbackPortReusable(t *testing.T, port uint16) {
	t.Helper()
	listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("callback port %d not released: %v", port, err)
	}
	listener.Close()
}
