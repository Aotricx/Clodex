package ratelimit

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseEventPreservesCreditedLimitSnapshotAsInformational(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	raw := []byte(`{"type":"codex.rate_limits","rate_limits":{"allowed":false,"limit_reached":true,"primary":{"used_percent":100,"window_minutes":10080,"reset_after_seconds":509821}},"credits":{"has_credits":true,"unlimited":false,"balance":null},"account_id":"secret-account"}`)
	snapshot, err := ParseEvent(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CapturedAt != now || snapshot.LimitID != "codex" || !snapshot.LimitReached {
		t.Fatalf("identity = %#v", snapshot)
	}
	if snapshot.Primary == nil || snapshot.Primary.UsedPercent != 100 || snapshot.Primary.WindowMinutes == nil ||
		*snapshot.Primary.WindowMinutes != 10080 || snapshot.Primary.ResetAfterSeconds == nil || *snapshot.Primary.ResetAfterSeconds != 509821 {
		t.Fatalf("primary = %#v", snapshot.Primary)
	}
	if snapshot.Credits == nil || !snapshot.Credits.HasCredits || snapshot.Credits.Unlimited || CreditsExhausted(snapshot) {
		t.Fatalf("credits = %#v, exhausted=%v", snapshot.Credits, CreditsExhausted(snapshot))
	}
	if !json.Valid(snapshot.SanitizedRaw) || strings.Contains(string(snapshot.SanitizedRaw), "secret-account") || !strings.Contains(string(snapshot.SanitizedRaw), "<redacted>") {
		t.Fatalf("sanitized raw = %s", snapshot.SanitizedRaw)
	}

	headers := make(http.Header)
	ApplyResponseHeaders(headers, snapshot)
	for name, want := range map[string]string{
		"X-Clodex-Rate-Limit-Reached":     "true",
		"X-Clodex-Credits-Has-Credits":    "true",
		"X-Clodex-Credits-Unlimited":      "false",
		"X-Clodex-Rate-Limit-Reset-After": "509821",
	} {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if headers.Get("Retry-After") != "" {
		t.Fatalf("informational snapshot fabricated Retry-After: %q", headers.Get("Retry-After"))
	}
}

func TestParseEventNormalizesLimitNameAndExplicitExhaustion(t *testing.T) {
	raw := []byte(`{"type":"codex.rate_limits","metered_limit_name":"Codex-Sonic","rate_limits":{"limit_reached":true},"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`)
	snapshot, err := ParseEvent(raw, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LimitID != "codex_sonic" || !CreditsExhausted(snapshot) || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != "0" {
		t.Fatalf("snapshot = %#v", snapshot)
	}

	unlimited, err := ParseEvent([]byte(`{"type":"codex.rate_limits","credits":{"has_credits":false,"unlimited":true}}`), time.Time{})
	if err != nil || CreditsExhausted(unlimited) {
		t.Fatalf("unlimited = %#v, %v", unlimited, err)
	}
}

func TestParseEventRejectsWrongTypeMalformedAndNonfiniteData(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.completed"}`,
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":"NaN"}}}`,
		`{"type":"codex.rate_limits","credits":{"has_credits":true}}`,
		`not-json`,
	} {
		if _, err := ParseEvent([]byte(raw), time.Time{}); err == nil {
			t.Errorf("ParseEvent(%s) succeeded", raw)
		}
	}
}

func TestParseHeadersUsesPinnedCodexFamilies(t *testing.T) {
	now := time.Date(2026, time.July, 21, 12, 0, 0, 0, time.UTC)
	headers := http.Header{
		"X-Codex-Primary-Used-Percent":     {"12.5"},
		"X-Codex-Primary-Window-Minutes":   {"60"},
		"X-Codex-Primary-Reset-At":         {"1704069000"},
		"X-Codex-Secondary-Used-Percent":   {"80"},
		"X-Codex-Secondary-Window-Minutes": {"1440"},
		"X-Codex-Secondary-Reset-At":       {"1704074400"},
		"X-Codex-Credits-Has-Credits":      {"1"},
		"X-Codex-Credits-Unlimited":        {"false"},
		"X-Codex-Credits-Balance":          {"12.50"},
	}
	snapshot, ok := ParseHeaders(headers, now)
	if !ok || snapshot.Primary == nil || snapshot.Secondary == nil || snapshot.Credits == nil {
		t.Fatalf("snapshot = %#v, ok=%v", snapshot, ok)
	}
	if snapshot.Primary.UsedPercent != 12.5 || *snapshot.Primary.ResetAt != 1704069000 ||
		snapshot.Secondary.UsedPercent != 80 || *snapshot.Secondary.WindowMinutes != 1440 ||
		!snapshot.Credits.HasCredits || snapshot.Credits.Balance == nil || *snapshot.Credits.Balance != "12.50" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestParseHeadersReturnsFalseWithoutCompleteRateOrCreditFields(t *testing.T) {
	for _, headers := range []http.Header{
		nil,
		{"X-Codex-Primary-Used-Percent": {"not-number"}},
		{"X-Codex-Credits-Has-Credits": {"true"}},
	} {
		if snapshot, ok := ParseHeaders(headers, time.Time{}); ok {
			t.Errorf("ParseHeaders(%v) = %#v, true", headers, snapshot)
		}
	}
}
