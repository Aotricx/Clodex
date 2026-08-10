package oauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProtocolConstants(t *testing.T) {
	if DefaultClientID != "app_EMoamEEZ73f0CkXaXp7hrann" {
		t.Fatalf("DefaultClientID = %q", DefaultClientID)
	}
	if DefaultIssuer != "https://auth.openai.com" {
		t.Fatalf("DefaultIssuer = %q", DefaultIssuer)
	}
	if Scopes != "openid profile email offline_access api.connectors.read api.connectors.invoke" {
		t.Fatalf("Scopes = %q", Scopes)
	}
	if DeviceVerificationURI != "https://auth.openai.com/codex/device" {
		t.Fatalf("DeviceVerificationURI = %q", DeviceVerificationURI)
	}
}

func TestGeneratePKCEUses64BytesAndS256(t *testing.T) {
	entropy := make([]byte, 64)
	for i := range entropy {
		entropy[i] = byte(i)
	}
	pkce, err := GeneratePKCE(bytes.NewReader(entropy))
	if err != nil {
		t.Fatal(err)
	}
	wantVerifier := base64.RawURLEncoding.EncodeToString(entropy)
	wantDigest := sha256.Sum256([]byte(wantVerifier))
	wantChallenge := base64.RawURLEncoding.EncodeToString(wantDigest[:])
	if pkce.Verifier != wantVerifier || pkce.Challenge != wantChallenge {
		t.Fatalf("PKCE = %+v, want verifier %q challenge %q", pkce, wantVerifier, wantChallenge)
	}
	if strings.ContainsAny(pkce.Verifier+pkce.Challenge, "=+/\n") {
		t.Fatalf("PKCE is not unpadded base64url: %+v", pkce)
	}
}

func TestGeneratePKCEPropagatesEntropyFailure(t *testing.T) {
	_, err := GeneratePKCE(io.LimitReader(bytes.NewReader(make([]byte, 63)), 63))
	if err == nil {
		t.Fatal("GeneratePKCE returned nil error for short entropy")
	}
}

func TestAuthorizeURLExactQuery(t *testing.T) {
	c := Client{Issuer: "https://issuer.example/base/", ClientID: "client"}
	got, err := c.AuthorizeURL("http://localhost:1455/auth/callback", PKCE{Verifier: "verifier", Challenge: "challenge"}, "state")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "issuer.example" || u.Path != "/base/oauth/authorize" {
		t.Fatalf("authorize endpoint = %s", u)
	}
	want := url.Values{
		"response_type":              {"code"},
		"client_id":                  {"client"},
		"redirect_uri":               {"http://localhost:1455/auth/callback"},
		"scope":                      {Scopes},
		"code_challenge":             {"challenge"},
		"code_challenge_method":      {"S256"},
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
		"state":                      {"state"},
		"originator":                 {"codex_cli_rs"},
	}
	if !reflect.DeepEqual(u.Query(), want) {
		t.Fatalf("query = %#v, want %#v", u.Query(), want)
	}
}

func TestExchangeCodeExactFormAndTokens(t *testing.T) {
	var gotForm url.Values
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		gotForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id_token":"id","access_token":"access","refresh_token":"refresh","account_id":"acct","expires_in":3600}`)
	}))
	defer issuer.Close()

	c := Client{Issuer: issuer.URL, ClientID: "client", HTTPClient: issuer.Client()}
	got, err := c.ExchangeCode(context.Background(), "auth code", "http://localhost:1455/auth/callback", PKCE{Verifier: "verify", Challenge: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	wantForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"auth code"},
		"redirect_uri":  {"http://localhost:1455/auth/callback"},
		"client_id":     {"client"},
		"code_verifier": {"verify"},
	}
	if !reflect.DeepEqual(gotForm, wantForm) {
		t.Fatalf("form = %#v, want %#v", gotForm, wantForm)
	}
	if got != (TokenSet{IDToken: "id", AccessToken: "access", RefreshToken: "refresh", AccountID: "acct", ExpiresIn: 3600}) {
		t.Fatalf("tokens = %+v", got)
	}
}

func TestExchangeCodeRejectsMissingRequiredToken(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"id_token":"id","access_token":"access"}`)
	}))
	defer issuer.Close()
	_, err := (&Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}).ExchangeCode(context.Background(), "code", "redirect", PKCE{Verifier: "verify"})
	if err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("error = %v", err)
	}
}

func TestRefreshExactJSONAndOptionalRotation(t *testing.T) {
	var got map[string]string
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if gotType := r.Header.Get("Content-Type"); gotType != "application/json" {
			t.Errorf("Content-Type = %q", gotType)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id_token":"new-id","access_token":"new-access"}`)
	}))
	defer issuer.Close()

	c := Client{Issuer: issuer.URL, ClientID: "client", HTTPClient: issuer.Client()}
	tokens, err := c.Refresh(context.Background(), "old-refresh")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"client_id": "client", "grant_type": "refresh_token", "refresh_token": "old-refresh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON = %#v, want %#v", got, want)
	}
	if tokens.IDToken != "new-id" || tokens.AccessToken != "new-access" || tokens.RefreshToken != "" {
		t.Fatalf("tokens = %+v", tokens)
	}
}

func TestRefreshRejectsMissingAccessToken(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{}`)
	}))
	defer issuer.Close()

	_, err := (&Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}).Refresh(context.Background(), "refresh")
	if err == nil || err.Error() != "token response missing access_token" {
		t.Fatalf("error = %v", err)
	}
}

func TestStartDeviceExactRequest(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/accounts/deviceauth/usercode" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(body, map[string]string{"client_id": "client"}) {
			t.Errorf("body = %#v", body)
		}
		io.WriteString(w, `{"device_auth_id":"device-id","usercode":"ABCD-EFGH","interval":"2"}`)
	}))
	defer issuer.Close()

	c := Client{Issuer: issuer.URL, ClientID: "client", HTTPClient: issuer.Client()}
	got, err := c.StartDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := DeviceCode{VerificationURI: issuer.URL + "/codex/device", UserCode: "ABCD-EFGH", DeviceAuthID: "device-id", Interval: 2 * time.Second}
	if got != want {
		t.Fatalf("device code = %+v, want %+v", got, want)
	}
}

func TestStartDeviceRejectsNonPositiveInterval(t *testing.T) {
	for _, interval := range []string{"0", "-1"} {
		t.Run(interval, func(t *testing.T) {
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, `{"device_auth_id":"device-id","user_code":"ABCD-EFGH","interval":"`+interval+`"}`)
			}))
			defer issuer.Close()

			_, err := (&Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}).StartDevice(context.Background())
			if err == nil || !strings.Contains(err.Error(), "interval") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPollDevicePendingThenExchangesCode(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	var pollBodies []map[string]string
	var issuer *httptest.Server
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			mu.Lock()
			pollBodies = append(pollBodies, body)
			polls++
			poll := polls
			mu.Unlock()
			if poll == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if poll == 2 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"authorization_code":"authorization","code_challenge":"challenge","code_verifier":"verifier"}`)
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			want := url.Values{
				"grant_type": {"authorization_code"}, "code": {"authorization"},
				"redirect_uri": {issuer.URL + "/deviceauth/callback"}, "client_id": {"client"}, "code_verifier": {"verifier"},
			}
			if !reflect.DeepEqual(r.PostForm, want) {
				t.Errorf("exchange form = %#v, want %#v", r.PostForm, want)
			}
			io.WriteString(w, `{"id_token":"id","access_token":"access","refresh_token":"refresh"}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer issuer.Close()

	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	var sleeps []time.Duration
	c := Client{
		Issuer: issuer.URL, ClientID: "client", HTTPClient: issuer.Client(),
		Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps = append(sleeps, d)
			now = now.Add(d)
			return nil
		},
	}
	tokens, err := c.PollDevice(context.Background(), DeviceCode{DeviceAuthID: "device-id", UserCode: "ABCD-EFGH", Interval: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != "access" || !reflect.DeepEqual(sleeps, []time.Duration{2 * time.Second, 2 * time.Second}) {
		t.Fatalf("tokens = %+v, sleeps = %v", tokens, sleeps)
	}
	wantBody := map[string]string{"device_auth_id": "device-id", "user_code": "ABCD-EFGH"}
	for _, body := range pollBodies {
		if !reflect.DeepEqual(body, wantBody) {
			t.Fatalf("poll body = %#v", body)
		}
	}
}

func TestPollDeviceExpiresAt15Minutes(t *testing.T) {
	polls := 0
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		polls++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer issuer.Close()
	now := time.Unix(0, 0)
	c := Client{
		Issuer: issuer.URL, HTTPClient: issuer.Client(), Now: func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
	}
	_, err := c.PollDevice(context.Background(), DeviceCode{DeviceAuthID: "id", UserCode: "code", Interval: 10 * time.Minute})
	if err == nil || !strings.Contains(err.Error(), "15 minutes") {
		t.Fatalf("error = %v", err)
	}
	if polls != 2 {
		t.Fatalf("polls = %d, want 2", polls)
	}
}

func TestPollDeviceCancellation(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer issuer.Close()
	c := Client{
		Issuer: issuer.URL, HTTPClient: issuer.Client(),
		Sleep: func(ctx context.Context, _ time.Duration) error { <-ctx.Done(); return ctx.Err() },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.PollDevice(ctx, DeviceCode{DeviceAuthID: "id", UserCode: "code", Interval: time.Second})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestPollDeviceRejectsNonPositiveIntervalBeforeRequest(t *testing.T) {
	requests := 0
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusForbidden)
	}))
	defer issuer.Close()
	now := time.Unix(0, 0)
	c := Client{
		Issuer: issuer.URL, HTTPClient: issuer.Client(), Now: func() time.Time { return now },
		Sleep: func(_ context.Context, _ time.Duration) error { now = now.Add(15 * time.Minute); return nil },
	}
	for _, interval := range []time.Duration{0, -time.Second} {
		now = time.Unix(0, 0)
		_, err := c.PollDevice(context.Background(), DeviceCode{DeviceAuthID: "id", UserCode: "code", Interval: interval})
		if err == nil || !strings.Contains(err.Error(), "interval") {
			t.Errorf("interval %s: error = %v", interval, err)
		}
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}

func TestHTTPErrorPreservesStatusRetryAfterAndRedacts(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"rate_limited","message":"wait","access_token":"must-not-leak"}`)
	}))
	defer issuer.Close()
	_, err := (&Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}).StartDevice(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error type = %T: %v", err, err)
	}
	if httpErr.StatusCode != http.StatusTooManyRequests || httpErr.RetryAfter != "17" {
		t.Fatalf("HTTP error = %+v", httpErr)
	}
	if strings.Contains(httpErr.Error(), "must-not-leak") || !strings.Contains(httpErr.Error(), "rate_limited") {
		t.Fatalf("unsafe/unhelpful error = %q", httpErr.Error())
	}
}

func TestHTTPErrorRedactsEchoedRequestCredential(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"bad refresh old-refresh-credential"}`)
	}))
	defer issuer.Close()
	_, err := (&Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}).Refresh(context.Background(), "old-refresh-credential")
	if err == nil || strings.Contains(err.Error(), "old-refresh-credential") {
		t.Fatalf("credential leaked in error: %v", err)
	}
}

func TestHTTPErrorRedactsMalformedAndTruncatedSecrets(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjMifQ.signature"
	malformed := `{"error":"bad","message":"helpful diagnostic","access_token":"access-secret","client_secret":"client-secret","authorization":"Bearer bearer-secret","jwt":"` + jwt + `","refresh_token":"unterminated-refresh-secret`
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, malformed+strings.Repeat("x", maxErrorBodyBytes))
	}))
	defer issuer.Close()

	_, err := (&Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}).StartDevice(context.Background())
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error type = %T: %v", err, err)
	}
	if !strings.Contains(httpErr.Body, "helpful diagnostic") {
		t.Fatalf("helpful message lost: %q", httpErr.Body)
	}
	for _, secret := range []string{"access-secret", "client-secret", "bearer-secret", jwt, "unterminated-refresh-secret"} {
		if strings.Contains(httpErr.Error(), secret) {
			t.Errorf("secret %q leaked: %q", secret, httpErr.Error())
		}
	}
	if len(httpErr.Body) > maxErrorBodyBytes+64 {
		t.Fatalf("error body length = %d", len(httpErr.Body))
	}
}
