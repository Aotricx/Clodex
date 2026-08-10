package failure

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestFromHTTPMapsAnthropicErrorTypesAndPreservesReason(t *testing.T) {
	tests := []struct {
		name, body, wantType, wantMessage string
		status                            int
		wantRetryable                     bool
	}{
		{"bad request detail", `{"detail":"unsupported effort"}`, "invalid_request_error", "unsupported effort", 400, false},
		{"authentication", `{"error":{"message":"token expired"}}`, "authentication_error", "token expired", 401, false},
		{"permission", `{"message":"account forbidden"}`, "permission_error", "account forbidden", 403, false},
		{"not found", `{"error":"model absent"}`, "not_found_error", "model absent", 404, false},
		{"timeout", `{"error":{"message":"request timed out"}}`, "api_error", "request timed out", 408, true},
		{"conflict", `{"detail":"try again"}`, "api_error", "try again", 409, true},
		{"too early", `{"detail":"too early"}`, "api_error", "too early", 425, true},
		{"too large", `{"detail":"image too large"}`, "request_too_large", "image too large", 413, false},
		{"unprocessable", `{"detail":"invalid schema"}`, "invalid_request_error", "invalid schema", 422, false},
		{"rate limit", `{"error":{"message":"quota exhausted","type":"rate_limit_error"}}`, "rate_limit_error", "quota exhausted", 429, true},
		{"server", `{"error":{"message":"upstream broke"}}`, "api_error", "upstream broke", 500, true},
		{"overloaded payload", `{"response":{"error":{"type":"overloaded_error","message":"busy"}}}`, "overloaded_error", "busy", 503, true},
		{"overloaded status", `{"detail":"capacity"}`, "overloaded_error", "capacity", 529, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FromHTTP(tc.status, []byte(tc.body), nil)
			if got.StatusCode != tc.status || got.Type != tc.wantType || got.Message != tc.wantMessage || got.Retryable != tc.wantRetryable {
				t.Fatalf("FromHTTP() = %+v", got)
			}
			encoded, err := json.Marshal(got.AnthropicResponse())
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			if response["type"] != "error" {
				t.Fatalf("error envelope = %s", encoded)
			}
		})
	}
}

func TestFromHTTPExtractsCommonBodiesAndRetryAfter(t *testing.T) {
	tests := []struct {
		name, contentType, body, want string
	}{
		{"nested response", "application/json", `{"response":{"error":{"message":"nested"}}}`, "nested"},
		{"error detail", "application/json", `{"error":{"detail":"specific"}}`, "specific"},
		{"error code fallback", "application/json", `{"error":{"code":"account_unavailable"}}`, "account_unavailable"},
		{"plain text", "text/plain", " gateway unavailable \n", "gateway unavailable"},
		{"malformed JSON", "application/json", "<html>bad gateway</html>", "<html>bad gateway</html>"},
		{"empty", "application/json", "", "Codex upstream returned HTTP 502 Bad Gateway"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("Content-Type", tc.contentType)
			headers.Set("Retry-After", "17")
			got := FromHTTP(http.StatusBadGateway, []byte(tc.body), headers)
			if got.Message != tc.want || got.RetryAfter != "17" {
				t.Fatalf("FromHTTP() = %+v, want message %q retry 17", got, tc.want)
			}
		})
	}
}

func TestFromHTTPIgnoresUntrustedErrorTypeForClassification(t *testing.T) {
	got := FromHTTP(400, []byte(`{"error":{"type":"authentication_error","message":"bad input"}}`), nil)
	if got.Type != "invalid_request_error" || got.StatusCode != 400 {
		t.Fatalf("FromHTTP() = %+v", got)
	}
}

func TestFromHTTPBoundsMessage(t *testing.T) {
	body := make([]byte, MaxMessageBytes+100)
	for i := range body {
		body[i] = 'x'
	}
	got := FromHTTP(500, body, nil)
	if len(got.Message) != MaxMessageBytes {
		t.Fatalf("message bytes = %d, want %d", len(got.Message), MaxMessageBytes)
	}
}

func TestFromHTTPRedactsSecretsBeforeBuildingClientError(t *testing.T) {
	const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1cHN0cmVhbSJ9.signature"
	body := []byte(`{"error":{"message":"failed for Bearer bearer-secret; access_token=` + jwt + `"}}`)
	got := FromHTTP(http.StatusBadGateway, body, nil)
	for _, secret := range []string{"bearer-secret", jwt} {
		if strings.Contains(got.Message, secret) {
			t.Fatalf("FromHTTP() leaked %q in client message %q", secret, got.Message)
		}
	}
	if !strings.Contains(got.Message, "<redacted>") {
		t.Fatalf("FromHTTP() message = %q, want redaction marker", got.Message)
	}
}
