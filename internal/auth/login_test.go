package auth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/oauth"
)

func TestStoreSaveLoginCreatesExactSecureCodexSchema(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, "new-home", ".codex")
	path := filepath.Join(dir, "auth.json")
	zone := time.FixedZone("test-offset", -5*60*60)
	now := time.Date(2032, 4, 5, 6, 7, 8, 123_000_000, zone)
	tokens := completeLoginTokens(t)
	tokens.ExpiresIn = 3600

	got, err := (&Store{Path: path}).SaveLogin(tokens, now)
	if err != nil {
		t.Fatalf("SaveLogin() error = %v", err)
	}
	if got.AuthMode != "chatgpt" || got.OpenAIAPIKey != nil || !got.LastRefresh.Equal(now.UTC()) {
		t.Fatalf("saved top-level fields = %#v", got)
	}
	if got.Tokens.IDToken != tokens.IDToken || got.Tokens.AccessToken != tokens.AccessToken ||
		got.Tokens.RefreshToken != tokens.RefreshToken || got.Tokens.AccountID == nil || *got.Tokens.AccountID != tokens.AccountID {
		t.Fatalf("saved tokens do not match login response: %#v", got.Tokens)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if keys := sortedKeys(raw); !reflect.DeepEqual(keys, []string{"OPENAI_API_KEY", "auth_mode", "last_refresh", "tokens"}) {
		t.Fatalf("top-level keys = %v", keys)
	}
	if raw["auth_mode"] != "chatgpt" || raw["OPENAI_API_KEY"] != nil || raw["last_refresh"] != now.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("exact top-level JSON = %#v", raw)
	}
	rawTokens, ok := raw["tokens"].(map[string]any)
	if !ok {
		t.Fatalf("tokens JSON = %#v", raw["tokens"])
	}
	if keys := sortedKeys(rawTokens); !reflect.DeepEqual(keys, []string{"access_token", "account_id", "id_token", "refresh_token"}) {
		t.Fatalf("token keys = %v", keys)
	}
	if _, exists := rawTokens["expires_in"]; exists {
		t.Fatal("OAuth expires_in leaked into Codex auth schema")
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("directory mode = %04o, want 0700", got)
		}
		fileInfo, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("file mode = %04o, want 0600", got)
		}
	}
}

func TestStoreSaveLoginDerivesOptionalAccountID(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name        string
		responseID  string
		idPayload   map[string]any
		accessClaim string
		wantAccount *string
	}{
		{
			name:        "response account wins",
			responseID:  "response-account",
			idPayload:   map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "id-account"}},
			accessClaim: "access-account",
			wantAccount: stringPtr("response-account"),
		},
		{
			name:        "ID claim fallback",
			idPayload:   map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "id-account"}},
			accessClaim: "access-account",
			wantAccount: stringPtr("id-account"),
		},
		{
			name:        "access claim fallback",
			idPayload:   map[string]any{"sub": "id"},
			accessClaim: "access-account",
			wantAccount: stringPtr("access-account"),
		},
		{
			name:      "source-valid absent account stays null",
			idPayload: map[string]any{"sub": "id"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tokens := completeLoginTokens(t)
			tokens.AccountID = tt.responseID
			tokens.IDToken = syntheticJWT(t, tt.idPayload)
			accessPayload := map[string]any{"exp": now.Add(time.Hour).Unix()}
			if tt.accessClaim != "" {
				accessPayload["chatgpt_account_id"] = tt.accessClaim
			}
			tokens.AccessToken = syntheticJWT(t, accessPayload)
			path := filepath.Join(t.TempDir(), "auth.json")

			got, err := (&Store{Path: path}).SaveLogin(tokens, now)
			if err != nil {
				t.Fatalf("SaveLogin() error = %v", err)
			}
			if !equalOptionalString(got.Tokens.AccountID, tt.wantAccount) {
				t.Fatalf("AccountID = %v, want %v", got.Tokens.AccountID, tt.wantAccount)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantAccount == nil && !bytes.Contains(data, []byte(`"account_id": null`)) {
				t.Fatalf("null account_id missing from schema: %s", data)
			}
		})
	}
}

func TestStoreSaveLoginPreservesUnknownFieldsAndReplacesCredentials(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	existing := validAuthFile(t, now.Add(time.Hour), now.Add(-time.Hour))
	existing.raw = map[string]json.RawMessage{"future_top": json.RawMessage(`{"keep":true}`)}
	existing.Tokens.raw = map[string]json.RawMessage{"future_token": json.RawMessage(`["keep"]`)}
	writeFileValue(t, path, existing)
	tokens := completeLoginTokens(t)

	got, err := (&Store{Path: path}).SaveLogin(tokens, now)
	if err != nil {
		t.Fatalf("SaveLogin() error = %v", err)
	}
	if got.OpenAIAPIKey != nil {
		t.Fatal("OPENAI_API_KEY was not forced to null")
	}
	if got.Tokens.IDToken != tokens.IDToken || got.Tokens.AccessToken != tokens.AccessToken || got.Tokens.RefreshToken != tokens.RefreshToken {
		t.Fatal("existing credentials were not replaced")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"future_top":{"keep":true}`, `"future_token":["keep"]`} {
		if !bytes.Contains(encoded, []byte(fragment)) {
			t.Fatalf("unknown field lost: %s", encoded)
		}
	}
}

func TestStoreSaveLoginRetriesOnConcurrentExistingChange(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	existing := validAuthFile(t, now.Add(time.Hour), now.Add(-time.Hour))
	writeFileValue(t, path, existing)
	store := &Store{Path: path}
	hookCalls := 0
	store.beforeRename = func(_, targetPath string) error {
		hookCalls++
		if hookCalls != 1 {
			return nil
		}
		external := existing.clone()
		external.raw = map[string]json.RawMessage{"external_future": json.RawMessage(`{"won":true}`)}
		encoded, err := json.Marshal(external)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, encoded, 0o600)
	}
	tokens := completeLoginTokens(t)

	got, err := store.SaveLogin(tokens, now)
	if err != nil {
		t.Fatalf("SaveLogin() error = %v", err)
	}
	if hookCalls < 2 {
		t.Fatalf("beforeRename calls = %d, want retry", hookCalls)
	}
	if got.Tokens.AccessToken != tokens.AccessToken || got.OpenAIAPIKey != nil {
		t.Fatal("login credentials did not win after retry")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"external_future":{"won":true}`)) {
		t.Fatalf("concurrent unknown field lost: %s", encoded)
	}
}

func TestStoreSaveLoginRejectsInvalidInputWithoutChangingDiskOrLeakingSecrets(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	valid := completeLoginTokens(t)
	tests := []struct {
		name   string
		tokens oauth.TokenSet
		now    time.Time
	}{
		{"missing ID token", replaceLoginToken(valid, "id", ""), now},
		{"whitespace ID token", replaceLoginToken(valid, "id", " "), now},
		{"malformed ID token", replaceLoginToken(valid, "id", "malformed-id-token-secret"), now},
		{"missing access token", replaceLoginToken(valid, "access", ""), now},
		{"malformed access token", replaceLoginToken(valid, "access", "malformed-access-token-secret"), now},
		{"missing refresh token", replaceLoginToken(valid, "refresh", ""), now},
		{"whitespace refresh token", replaceLoginToken(valid, "refresh", " "), now},
		{"whitespace optional account", replaceLoginToken(valid, "account", " "), now},
		{"zero timestamp", valid, time.Time{}},
		{"out of range timestamp", valid, time.Date(10_000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "auth.json")
			writeFileValue(t, path, validAuthFile(t, now.Add(time.Hour), now.Add(-time.Hour)))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			_, err = (&Store{Path: path}).SaveLogin(tt.tokens, tt.now)
			if err == nil {
				t.Fatal("SaveLogin() error = nil")
			}
			for _, secret := range []string{
				valid.IDToken, valid.AccessToken, valid.RefreshToken, valid.AccountID,
				"malformed-id-token-secret", "malformed-access-token-secret",
			} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked secret %q: %v", secret, err)
				}
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("auth file changed on invalid login\n got: %s\nwant: %s", after, before)
			}
		})
	}
}

func TestStoreSaveLoginNilStoreFailsSafely(t *testing.T) {
	t.Parallel()

	var store *Store
	_, err := store.SaveLogin(completeLoginTokens(t), time.Now())
	if err == nil || !strings.Contains(err.Error(), "store") {
		t.Fatalf("SaveLogin() error = %v", err)
	}
}

func completeLoginTokens(t testing.TB) oauth.TokenSet {
	t.Helper()
	return oauth.TokenSet{
		IDToken: syntheticJWT(t, map[string]any{
			"email": "login-secret@example.test",
			"https://api.openai.com/auth": map[string]any{
				"chatgpt_account_id": "login-account-secret",
			},
		}),
		AccessToken:  syntheticJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "marker": "login-access-secret"}),
		RefreshToken: "login-refresh-secret",
		AccountID:    "login-account-secret",
	}
}

func replaceLoginToken(tokens oauth.TokenSet, field, value string) oauth.TokenSet {
	switch field {
	case "id":
		tokens.IDToken = value
	case "access":
		tokens.AccessToken = value
	case "refresh":
		tokens.RefreshToken = value
	case "account":
		tokens.AccountID = value
	}
	return tokens
}

func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func stringPtr(value string) *string { return &value }

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
