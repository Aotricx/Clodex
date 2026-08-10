package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestParseJWTClaimsExtractsCodexClaims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payload map[string]any
		want    Claims
	}{
		{
			name: "nested auth and direct email",
			payload: map[string]any{
				"exp":   int64(1_900_000_000),
				"email": "direct@example.test",
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_plan_type":          "pro",
					"chatgpt_account_id":         "acct-nested",
					"chatgpt_user_id":            "user-chatgpt",
					"user_id":                    "user-fallback",
					"chatgpt_account_is_fedramp": true,
				},
			},
			want: Claims{
				ExpiresAt:             timePtr(time.Unix(1_900_000_000, 0).UTC()),
				Email:                 "direct@example.test",
				Plan:                  "pro",
				ChatGPTAccountID:      "acct-nested",
				ChatGPTUserID:         "user-chatgpt",
				ChatGPTAccountFedRAMP: true,
			},
		},
		{
			name: "profile email and nested user fallback",
			payload: map[string]any{
				"https://api.openai.com/profile": map[string]any{"email": "profile@example.test"},
				"https://api.openai.com/auth":    map[string]any{"user_id": "user-nested"},
			},
			want: Claims{Email: "profile@example.test", ChatGPTUserID: "user-nested"},
		},
		{
			name: "direct variants",
			payload: map[string]any{
				"chatgpt_plan_type":          "business",
				"chatgpt_account_id":         "acct-direct",
				"chatgpt_user_id":            "user-direct",
				"chatgpt_account_is_fedramp": true,
			},
			want: Claims{
				Plan:                  "business",
				ChatGPTAccountID:      "acct-direct",
				ChatGPTUserID:         "user-direct",
				ChatGPTAccountFedRAMP: true,
			},
		},
		{
			name: "nested values win over direct variants",
			payload: map[string]any{
				"chatgpt_plan_type":  "free",
				"chatgpt_account_id": "acct-direct",
				"user_id":            "user-direct",
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_plan_type":  "team",
					"chatgpt_account_id": "acct-nested",
					"user_id":            "user-nested",
				},
			},
			want: Claims{Plan: "team", ChatGPTAccountID: "acct-nested", ChatGPTUserID: "user-nested"},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseJWTClaims(syntheticJWT(t, tt.payload))
			if err != nil {
				t.Fatalf("ParseJWTClaims() error = %v", err)
			}
			if !claimsEqual(got, tt.want) {
				t.Fatalf("ParseJWTClaims() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseJWTClaimsRequiresExactUnpaddedPayload(t *testing.T) {
	t.Parallel()

	valid := syntheticJWT(t, map[string]any{"sub": "synthetic"})
	parts := strings.Split(valid, ".")
	tests := map[string]string{
		"one segment":       "not-a-jwt",
		"empty header":      "." + parts[1] + ".sig",
		"empty payload":     parts[0] + "..sig",
		"empty signature":   parts[0] + "." + parts[1] + ".",
		"four segments":     valid + ".extra",
		"invalid base64url": parts[0] + ".%%%25.sig",
		"padded payload":    parts[0] + "." + parts[1] + "=.sig",
		"invalid json":      parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("{")) + ".sig",
		"null payload":      parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte("null")) + ".sig",
	}
	for name, token := range tests {
		name, token := name, token
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseJWTClaims(token); err == nil {
				t.Fatal("ParseJWTClaims() error = nil")
			} else if strings.Contains(err.Error(), token) {
				t.Fatalf("error leaked JWT: %v", err)
			}
		})
	}
}

func TestParseJWTClaimsDoesNotVerifySignature(t *testing.T) {
	t.Parallel()

	token := syntheticJWT(t, map[string]any{"email": "synthetic@example.test"})
	parts := strings.Split(token, ".")
	token = parts[0] + "." + parts[1] + ".deliberately-not-a-signature"
	claims, err := ParseJWTClaims(token)
	if err != nil {
		t.Fatalf("ParseJWTClaims() error = %v", err)
	}
	if claims.Email != "synthetic@example.test" {
		t.Fatalf("Email = %q", claims.Email)
	}
}

func TestParseJWTClaimsRejectsInvalidExpiration(t *testing.T) {
	t.Parallel()

	for name, exp := range map[string]any{
		"string":       "1900000000",
		"fractional":   1.5,
		"out of range": int64(1<<63 - 1),
	} {
		name, exp := name, exp
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseJWTClaims(syntheticJWT(t, map[string]any{"exp": exp})); err == nil {
				t.Fatal("ParseJWTClaims() error = nil")
			}
		})
	}
}

func TestNormalizePlanMatchesPinnedCodexMappings(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"pro":                             "Pro",
		"go":                              "Go",
		"hc":                              "Enterprise",
		"team":                            "Team",
		"business":                        "Business",
		"enterprise_cbp_usage_based":      "Enterprise CBP Usage Based",
		"self_serve_business_usage_based": "Self Serve Business Usage Based",
		"mystery-tier":                    "mystery-tier",
		"":                                "Unknown",
	}
	for raw, want := range tests {
		raw, want := raw, want
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			if got := NormalizePlan(raw); got != want {
				t.Fatalf("NormalizePlan(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestSummaryRedactsAccountAndUsesAccessExpiry(t *testing.T) {
	t.Parallel()

	accessExpiry := time.Unix(1_900_000_000, 0).UTC()
	idExpiry := accessExpiry.Add(time.Hour)
	accountID := "acct-super-secret-123456"
	file := validAuthFile(t, accessExpiry, time.Now().UTC())
	file.Tokens.AccountID = &accountID
	file.Tokens.IDToken = syntheticJWT(t, map[string]any{
		"exp":   idExpiry.Unix(),
		"email": "never-show@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  "hc",
			"chatgpt_account_id": "claim-account-secret",
		},
	})

	got, err := file.Summary()
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}
	wantHash := sha256.Sum256([]byte(accountID))
	wantAccount := "sha256:" + hex.EncodeToString(wantHash[:6])
	if got.Source != "codex-cli" || got.Account != wantAccount || got.Plan != "Enterprise" {
		t.Fatalf("Summary() = %#v", got)
	}
	if got.Expiry == nil || !got.Expiry.Equal(accessExpiry) {
		t.Fatalf("Expiry = %v, want %v", got.Expiry, accessExpiry)
	}
	for _, secret := range []string{accountID, "claim-account-secret", "never-show@example.test"} {
		if strings.Contains(mustJSON(t, got), secret) {
			t.Fatalf("summary leaked %q: %#v", secret, got)
		}
	}

	again, err := file.Summary()
	if err != nil || again.Account != got.Account {
		t.Fatalf("account mask not deterministic: %#v, %v", again, err)
	}
}

func TestSummaryFallsBackToIDExpiryAndClaimIdentity(t *testing.T) {
	t.Parallel()

	expiry := time.Unix(1_900_000_000, 0).UTC()
	file := validAuthFile(t, expiry, time.Now().UTC())
	file.Tokens.AccountID = nil
	file.Tokens.AccessToken = syntheticJWT(t, map[string]any{"sub": "access"})
	file.Tokens.IDToken = syntheticJWT(t, map[string]any{
		"exp": expiry.Unix(),
		"https://api.openai.com/profile": map[string]any{
			"email": "fallback@example.test",
		},
	})

	got, err := file.Summary()
	if err != nil {
		t.Fatalf("Summary() error = %v", err)
	}
	if got.Account == "" || got.Account == "fallback@example.test" || strings.Contains(got.Account, "fallback@example.test") {
		t.Fatalf("Account = %q", got.Account)
	}
	if got.Expiry == nil || !got.Expiry.Equal(expiry) {
		t.Fatalf("Expiry = %v, want %v", got.Expiry, expiry)
	}
}

func TestExpiredAndRefreshDueBoundaries(t *testing.T) {
	t.Parallel()

	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name          string
		accessPayload map[string]any
		idPayload     map[string]any
		lastRefresh   time.Time
		wantExpired   bool
		wantDue       bool
	}{
		{"expires now", map[string]any{"exp": now.Unix()}, map[string]any{"exp": now.Add(time.Hour).Unix()}, now, true, true},
		{"five minute skew inclusive", map[string]any{"exp": now.Add(5 * time.Minute).Unix()}, nil, now, false, true},
		{"outside five minute skew", map[string]any{"exp": now.Add(5*time.Minute + time.Second).Unix()}, nil, now.Add(-9 * 24 * time.Hour), false, false},
		{"id expiry fallback", map[string]any{"sub": "access"}, map[string]any{"exp": now.Unix()}, now, true, false},
		{"eight days exact is fresh", map[string]any{"sub": "access"}, nil, now.Add(-8 * 24 * time.Hour), false, false},
		{"older than eight days", map[string]any{"sub": "access"}, nil, now.Add(-8*24*time.Hour - time.Second), false, true},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			file := validAuthFile(t, now.Add(time.Hour), tt.lastRefresh)
			file.Tokens.AccessToken = syntheticJWT(t, tt.accessPayload)
			if tt.idPayload == nil {
				tt.idPayload = map[string]any{"sub": "id"}
			}
			file.Tokens.IDToken = syntheticJWT(t, tt.idPayload)

			expired, err := file.Expired(now)
			if err != nil {
				t.Fatalf("Expired() error = %v", err)
			}
			if expired != tt.wantExpired {
				t.Fatalf("Expired() = %v, want %v", expired, tt.wantExpired)
			}
			due, err := file.RefreshDue(now)
			if err != nil {
				t.Fatalf("RefreshDue() error = %v", err)
			}
			if due != tt.wantDue {
				t.Fatalf("RefreshDue() = %v, want %v", due, tt.wantDue)
			}
		})
	}
}

func syntheticJWT(t testing.TB, payload map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("synthetic-signature"))
}

func claimsEqual(left, right Claims) bool {
	if left.Email != right.Email || left.Plan != right.Plan || left.ChatGPTAccountID != right.ChatGPTAccountID ||
		left.ChatGPTUserID != right.ChatGPTUserID || left.ChatGPTAccountFedRAMP != right.ChatGPTAccountFedRAMP {
		return false
	}
	if left.ExpiresAt == nil || right.ExpiresAt == nil {
		return left.ExpiresAt == nil && right.ExpiresAt == nil
	}
	return left.ExpiresAt.Equal(*right.ExpiresAt)
}

func timePtr(value time.Time) *time.Time { return &value }

func mustJSON(t testing.TB, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
