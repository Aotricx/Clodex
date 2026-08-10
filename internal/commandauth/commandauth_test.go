package commandauth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/oauth"
)

type fakeOAuth struct {
	login       func(context.Context, oauth.Opener) (oauth.TokenSet, error)
	startDevice func(context.Context) (oauth.DeviceCode, error)
	pollDevice  func(context.Context, oauth.DeviceCode) (oauth.TokenSet, error)
}

func (f *fakeOAuth) Login(ctx context.Context, opener oauth.Opener) (oauth.TokenSet, error) {
	return f.login(ctx, opener)
}

func (f *fakeOAuth) StartDevice(ctx context.Context) (oauth.DeviceCode, error) {
	if f.startDevice == nil {
		panic("unexpected StartDevice call")
	}
	return f.startDevice(ctx)
}

func (f *fakeOAuth) PollDevice(ctx context.Context, device oauth.DeviceCode) (oauth.TokenSet, error) {
	if f.pollDevice == nil {
		panic("unexpected PollDevice call")
	}
	return f.pollDevice(ctx, device)
}

func TestDeviceLoginPrintsInstructionsBeforePollingAndPersists(t *testing.T) {
	now := time.Date(2026, 7, 21, 17, 18, 19, 0, time.UTC)
	tokens := testTokens(t, now.Add(time.Hour).Unix())
	device := oauth.DeviceCode{
		VerificationURI: "https://auth.openai.com/codex/device",
		UserCode:        "ABCD-EFGH",
		DeviceAuthID:    "device-auth-secret",
		Interval:        5 * time.Second,
	}
	var output bytes.Buffer
	store := &auth.Store{Path: filepath.Join(t.TempDir(), ".codex", "auth.json")}
	client := &fakeOAuth{
		startDevice: func(ctx context.Context) (oauth.DeviceCode, error) {
			return device, nil
		},
		pollDevice: func(ctx context.Context, got oauth.DeviceCode) (oauth.TokenSet, error) {
			if got != device {
				t.Fatalf("PollDevice device = %+v, want %+v", got, device)
			}
			printed := output.String()
			if !bytes.Contains(output.Bytes(), []byte(device.VerificationURI)) || !bytes.Contains(output.Bytes(), []byte(device.UserCode)) {
				t.Fatalf("instructions were not printed before poll: %q", printed)
			}
			return tokens, nil
		},
	}
	service := &Service{OAuth: client, Store: store, Now: func() time.Time { return now }, Writer: &output}

	summary, err := service.DeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("DeviceLogin: %v", err)
	}
	if summary.Source != auth.CodexCLISource || summary.Plan != "Team" {
		t.Fatalf("summary = %+v", summary)
	}
	printed := output.String()
	for _, secret := range []string{device.DeviceAuthID, tokens.IDToken, tokens.AccessToken, tokens.RefreshToken} {
		if bytes.Contains(output.Bytes(), []byte(secret)) {
			t.Fatalf("device output leaked secret %q: %q", secret, printed)
		}
	}
	file, err := store.Read()
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	if file.Tokens.AccessToken != tokens.AccessToken {
		t.Fatal("device OAuth tokens were not persisted")
	}
}

func TestDefaultAuthPathUsesOSUserHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve test user home: %v", err)
	}

	got, err := DefaultAuthPath()
	if err != nil {
		t.Fatalf("DefaultAuthPath: %v", err)
	}
	want := filepath.Join(home, ".codex", "auth.json")
	if got != want {
		t.Fatalf("DefaultAuthPath() = %q, want %q", got, want)
	}
}

func TestBrowserLoginPersistsCodexAuthAndReturnsRedactedSummary(t *testing.T) {
	now := time.Date(2026, 7, 21, 13, 14, 15, 0, time.FixedZone("test", -4*60*60))
	expires := now.Add(time.Hour).Unix()
	tokens := testTokens(t, expires)
	store := &auth.Store{Path: filepath.Join(t.TempDir(), ".codex", "auth.json")}
	opener := oauth.Opener(func(string) error { return nil })
	loginCalled := false
	client := &fakeOAuth{login: func(ctx context.Context, gotOpener oauth.Opener) (oauth.TokenSet, error) {
		loginCalled = true
		if ctx == nil {
			t.Fatal("Login received nil context")
		}
		if gotOpener == nil {
			t.Fatal("Login received nil opener")
		}
		return tokens, nil
	}}
	service := &Service{
		OAuth:  client,
		Store:  store,
		Now:    func() time.Time { return now },
		Opener: opener,
	}

	summary, err := service.BrowserLogin(context.Background())
	if err != nil {
		t.Fatalf("BrowserLogin: %v", err)
	}
	if !loginCalled {
		t.Fatal("OAuth Login was not called")
	}
	if summary.Source != auth.CodexCLISource || summary.Plan != "Team" {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Account == "" || summary.Account == "account-secret" {
		t.Fatalf("summary account is not redacted: %q", summary.Account)
	}
	if summary.Expiry == nil || summary.Expiry.Unix() != expires {
		t.Fatalf("summary expiry = %v, want unix %d", summary.Expiry, expires)
	}

	file, err := store.Read()
	if err != nil {
		t.Fatalf("read saved auth: %v", err)
	}
	if file.AuthMode != "chatgpt" || file.Tokens.AccessToken != tokens.AccessToken || file.Tokens.RefreshToken != tokens.RefreshToken || file.Tokens.IDToken != tokens.IDToken {
		t.Fatal("saved auth does not match OAuth tokens")
	}
	if !file.LastRefresh.Equal(now.UTC()) {
		t.Fatalf("last_refresh = %s, want %s", file.LastRefresh, now.UTC())
	}
}

func TestStatusWritesRedactedJSON(t *testing.T) {
	now := time.Date(2026, 7, 21, 21, 22, 23, 0, time.UTC)
	tokens := testTokens(t, now.Add(time.Hour).Unix())
	store := &auth.Store{Path: filepath.Join(t.TempDir(), ".codex", "auth.json")}
	if _, err := store.SaveLogin(tokens, now); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	var output bytes.Buffer
	service := &Service{Store: store, Writer: &output}

	summary, err := service.Status(context.Background(), StatusJSON)
	if err != nil {
		t.Fatalf("Status JSON: %v", err)
	}
	var decoded auth.Summary
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("decode status JSON %q: %v", output.String(), err)
	}
	if decoded.Source != auth.CodexCLISource || decoded.Account != summary.Account || decoded.Plan != "Team" {
		t.Fatalf("decoded status = %+v, summary = %+v", decoded, summary)
	}
	assertNoTokenLeak(t, output.String(), tokens)
}

func TestStatusWritesConciseRedactedText(t *testing.T) {
	now := time.Date(2026, 7, 21, 21, 22, 23, 0, time.UTC)
	tokens := testTokens(t, now.Add(time.Hour).Unix())
	store := &auth.Store{Path: filepath.Join(t.TempDir(), ".codex", "auth.json")}
	file, err := store.SaveLogin(tokens, now)
	if err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	want, err := file.Summary()
	if err != nil {
		t.Fatalf("summarize seeded auth: %v", err)
	}
	var output bytes.Buffer
	service := &Service{Store: store, Writer: &output}

	summary, err := service.Status(context.Background(), StatusText)
	if err != nil {
		t.Fatalf("Status text: %v", err)
	}
	if summary.Source != want.Source || summary.Account != want.Account || summary.Plan != want.Plan || summary.Expiry == nil || want.Expiry == nil || !summary.Expiry.Equal(*want.Expiry) {
		t.Fatalf("summary = %+v, want %+v", summary, want)
	}
	for _, fragment := range []string{
		"source=" + auth.CodexCLISource,
		"account=" + want.Account,
		"plan=Team",
		"expiry=" + want.Expiry.Format(time.RFC3339),
	} {
		if !bytes.Contains(output.Bytes(), []byte(fragment)) {
			t.Fatalf("text status %q missing %q", output.String(), fragment)
		}
	}
	assertNoTokenLeak(t, output.String(), tokens)
}

func TestLoginErrorsAreRedactedAndRemainInspectable(t *testing.T) {
	tokens := testTokens(t, time.Now().Add(time.Hour).Unix())
	secretError := errors.New("upstream rejected Authorization: Bearer " + tokens.AccessToken + " refresh_token=" + tokens.RefreshToken)
	service := &Service{
		OAuth: &fakeOAuth{login: func(context.Context, oauth.Opener) (oauth.TokenSet, error) {
			return oauth.TokenSet{}, secretError
		}},
		Store: &auth.Store{Path: filepath.Join(t.TempDir(), "auth.json")},
	}

	_, err := service.BrowserLogin(context.Background())
	if err == nil {
		t.Fatal("BrowserLogin unexpectedly succeeded")
	}
	if !errors.Is(err, secretError) {
		t.Fatalf("BrowserLogin error does not preserve cause: %v", err)
	}
	assertNoTokenLeak(t, err.Error(), tokens)
	if !strings.Contains(err.Error(), "browser login") || !strings.Contains(err.Error(), "<redacted>") {
		t.Fatalf("BrowserLogin error is not useful and redacted: %q", err)
	}
}

func TestDeviceLoginStopsWhenInstructionsCannotBeWritten(t *testing.T) {
	tokens := testTokens(t, time.Now().Add(time.Hour).Unix())
	writeErr := errors.New("writer failed id_token=" + tokens.IDToken)
	pollCalls := 0
	service := &Service{
		OAuth: &fakeOAuth{
			startDevice: func(context.Context) (oauth.DeviceCode, error) {
				return oauth.DeviceCode{VerificationURI: "https://auth.openai.com/codex/device", UserCode: "ABCD-EFGH", DeviceAuthID: "device-secret", Interval: time.Second}, nil
			},
			pollDevice: func(context.Context, oauth.DeviceCode) (oauth.TokenSet, error) {
				pollCalls++
				return tokens, nil
			},
		},
		Store:  &auth.Store{Path: filepath.Join(t.TempDir(), "auth.json")},
		Writer: failingWriter{err: writeErr},
	}

	_, err := service.DeviceLogin(context.Background())
	if err == nil {
		t.Fatal("DeviceLogin unexpectedly succeeded")
	}
	if pollCalls != 0 {
		t.Fatalf("PollDevice calls = %d, want 0", pollCalls)
	}
	if !errors.Is(err, writeErr) {
		t.Fatalf("DeviceLogin error does not preserve writer cause: %v", err)
	}
	assertNoTokenLeak(t, err.Error(), tokens)
}

func TestBrowserLoginHonorsCancellationBeforeOAuth(t *testing.T) {
	loginCalls := 0
	service := &Service{
		OAuth: &fakeOAuth{login: func(context.Context, oauth.Opener) (oauth.TokenSet, error) {
			loginCalls++
			return oauth.TokenSet{}, nil
		}},
		Store: &auth.Store{Path: filepath.Join(t.TempDir(), "auth.json")},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := service.BrowserLogin(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BrowserLogin error = %v, want context.Canceled", err)
	}
	if loginCalls != 0 {
		t.Fatalf("Login calls = %d, want 0", loginCalls)
	}
}

func TestBrowserLoginReturnsPersistenceFailureWithoutTokens(t *testing.T) {
	tokens := testTokens(t, time.Now().Add(time.Hour).Unix())
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("block"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}
	service := &Service{
		OAuth: &fakeOAuth{login: func(context.Context, oauth.Opener) (oauth.TokenSet, error) {
			return tokens, nil
		}},
		Store: &auth.Store{Path: filepath.Join(blockedParent, "auth.json")},
	}

	_, err := service.BrowserLogin(context.Background())
	if err == nil {
		t.Fatal("BrowserLogin unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "persist browser login") {
		t.Fatalf("persistence error lacks operation: %v", err)
	}
	assertNoTokenLeak(t, err.Error(), tokens)
}

func TestStatusRejectsUnknownFormatWithoutWriting(t *testing.T) {
	now := time.Now().UTC()
	store := &auth.Store{Path: filepath.Join(t.TempDir(), "auth.json")}
	if _, err := store.SaveLogin(testTokens(t, now.Add(time.Hour).Unix()), now); err != nil {
		t.Fatalf("seed auth: %v", err)
	}
	var output bytes.Buffer
	service := &Service{Store: store, Writer: &output}

	_, err := service.Status(context.Background(), StatusFormat("yaml"))
	if err == nil || !strings.Contains(err.Error(), "unsupported auth status format") {
		t.Fatalf("Status error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("Status wrote output for unknown format: %q", output.String())
	}
}

type failingWriter struct {
	err error
}

func (w failingWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func testTokens(t *testing.T, expiry int64) oauth.TokenSet {
	t.Helper()
	return oauth.TokenSet{
		IDToken: testJWT(t, map[string]any{
			"exp": expiry,
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "account-secret",
				"chatgpt_plan_type":  "team",
			},
		}),
		AccessToken:  testJWT(t, map[string]any{"exp": expiry}),
		RefreshToken: "refresh-secret",
		AccountID:    "account-secret",
	}
}

func testJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal JWT payload: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(encoded) + ".signature"
}

func assertNoTokenLeak(t *testing.T, text string, tokens oauth.TokenSet) {
	t.Helper()
	for _, secret := range []string{tokens.IDToken, tokens.AccessToken, tokens.RefreshToken, tokens.AccountID, "account-secret"} {
		if secret != "" && bytes.Contains([]byte(text), []byte(secret)) {
			t.Fatalf("output leaked secret %q: %q", secret, text)
		}
	}
}
