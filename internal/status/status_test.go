package status

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestSnapshotJSONContract(t *testing.T) {
	now := time.Date(2026, time.July, 21, 14, 30, 0, 0, time.UTC)
	expiry := now.Add(2 * time.Hour)
	resetAt := now.Add(15 * time.Minute).Unix()
	resetAfter := int64(900)
	windowMinutes := int64(60)
	balance := "12.50"
	openUntil := now.Add(time.Minute)

	state := New("v0.1.0")
	state.now = func() time.Time { return now }
	state.SetAuth(AuthSummary{
		Source:  "codex-cli",
		Account: "acct_[REDACTED]",
		Plan:    "pro",
		Expiry:  &expiry,
	})
	state.SetCatalog("live", now.Add(-90*time.Second))
	state.SetRateLimit(RateLimitSnapshot{
		CapturedAt:   now.Add(-time.Second),
		LimitID:      "codex",
		LimitName:    "Codex",
		LimitReached: true,
		Primary: &RateLimitWindow{
			UsedPercent:       100,
			WindowMinutes:     &windowMinutes,
			ResetAt:           &resetAt,
			ResetAfterSeconds: &resetAfter,
		},
		Credits: &CreditsSummary{
			HasCredits: true,
			Unlimited:  false,
			Balance:    &balance,
		},
		SanitizedRaw: json.RawMessage(`{"new_field":{"safe":true}}`),
	})
	state.RecordWarning("unsupported_max_tokens")
	state.RecordRetry("empty_completion")
	state.SetRemainingRetryBudget(7)
	state.SetCircuit(CircuitState{Open: true, Failures: 3, OpenUntil: &openUntil})
	end := state.BeginSession()
	t.Cleanup(end)

	encoded, err := json.Marshal(state.Snapshot())
	if err != nil {
		t.Fatalf("Marshal(Snapshot()) error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal(snapshot JSON) error = %v", err)
	}
	assertKeys(t, document, "active_sessions", "auth", "catalog", "rate_limit", "retry", "timing", "translation_warnings", "version")
	assertKeys(t, objectAt(t, document, "auth"), "account", "expiry", "plan", "source")
	assertKeys(t, objectAt(t, document, "catalog"), "age", "fetched_at", "source")
	assertKeys(t, objectAt(t, document, "translation_warnings"), "by_kind", "total")
	assertKeys(t, objectAt(t, document, "retry"), "by_trigger", "circuit", "remaining_budget", "total")
	assertKeys(t, objectAt(t, objectAt(t, document, "retry"), "circuit"), "failures", "open", "open_until")
	assertKeys(t, objectAt(t, document, "rate_limit"), "captured_at", "credits", "limit_id", "limit_name", "limit_reached", "primary", "raw", "secondary")
	assertKeys(t, objectAt(t, objectAt(t, document, "rate_limit"), "credits"), "balance", "has_credits", "unlimited")
	assertKeys(t, objectAt(t, objectAt(t, document, "rate_limit"), "primary"), "reset_after_seconds", "reset_at", "used_percent", "window_minutes")

	if got, want := document["version"], "v0.1.0"; got != want {
		t.Errorf("version = %v, want %v", got, want)
	}
	if got, want := document["active_sessions"], float64(1); got != want {
		t.Errorf("active_sessions = %v, want %v", got, want)
	}
	if got, want := objectAt(t, document, "catalog")["age"], "1m30s"; got != want {
		t.Errorf("catalog.age = %v, want %v", got, want)
	}
}

func TestSnapshotDeepCopiesMutableInputAndOutput(t *testing.T) {
	now := time.Date(2026, time.July, 21, 15, 0, 0, 0, time.UTC)
	expiry := now.Add(time.Hour)
	windowMinutes := int64(60)
	resetAt := now.Add(10 * time.Minute).Unix()
	resetAfter := int64(600)
	balance := "5.00"
	raw := json.RawMessage(`{"safe":"original"}`)
	openUntil := now.Add(time.Minute)

	state := New("deep-copy")
	state.now = func() time.Time { return now }
	auth := AuthSummary{Source: "file", Account: "redacted", Plan: "plus", Expiry: &expiry}
	rateLimit := RateLimitSnapshot{
		CapturedAt: now,
		LimitID:    "codex",
		Primary: &RateLimitWindow{
			UsedPercent:       25,
			WindowMinutes:     &windowMinutes,
			ResetAt:           &resetAt,
			ResetAfterSeconds: &resetAfter,
		},
		Credits:      &CreditsSummary{HasCredits: true, Balance: &balance},
		SanitizedRaw: raw,
	}
	state.SetAuth(auth)
	state.SetRateLimit(rateLimit)
	state.RecordWarning("stable")
	state.RecordRetry("stable")
	state.SetCircuit(CircuitState{Open: true, Failures: 2, OpenUntil: &openUntil})

	*auth.Expiry = auth.Expiry.Add(24 * time.Hour)
	*rateLimit.Primary.WindowMinutes = 1
	*rateLimit.Primary.ResetAt = 1
	*rateLimit.Primary.ResetAfterSeconds = 1
	*rateLimit.Credits.Balance = "mutated"
	rateLimit.SanitizedRaw[9] = 'X'
	openUntil = openUntil.Add(24 * time.Hour)

	first := state.Snapshot()
	if got, want := *first.Auth.Expiry, expiry.Add(-24*time.Hour); !got.Equal(want) {
		t.Fatalf("snapshot auth expiry = %v, want original %v", got, want)
	}
	if got, want := *first.RateLimit.Primary.WindowMinutes, int64(60); got != want {
		t.Fatalf("snapshot window minutes = %d, want %d", got, want)
	}
	if got, want := *first.RateLimit.Credits.Balance, "5.00"; got != want {
		t.Fatalf("snapshot balance = %q, want %q", got, want)
	}
	if got, want := string(first.RateLimit.SanitizedRaw), `{"safe":"original"}`; got != want {
		t.Fatalf("snapshot raw = %q, want %q", got, want)
	}
	if got, want := *first.Retry.Circuit.OpenUntil, now.Add(time.Minute); !got.Equal(want) {
		t.Fatalf("snapshot circuit open until = %v, want %v", got, want)
	}

	*first.Auth.Expiry = time.Time{}
	*first.RateLimit.Primary.WindowMinutes = 999
	*first.RateLimit.Credits.Balance = "changed"
	first.RateLimit.SanitizedRaw[9] = 'Y'
	first.TranslationWarnings.ByKind["stable"] = 999
	first.Retry.ByTrigger["stable"] = 999
	*first.Retry.Circuit.OpenUntil = time.Time{}

	second := state.Snapshot()
	if got, want := *second.RateLimit.Primary.WindowMinutes, int64(60); got != want {
		t.Errorf("second snapshot window minutes = %d, want %d", got, want)
	}
	if got, want := *second.RateLimit.Credits.Balance, "5.00"; got != want {
		t.Errorf("second snapshot balance = %q, want %q", got, want)
	}
	if got, want := string(second.RateLimit.SanitizedRaw), `{"safe":"original"}`; got != want {
		t.Errorf("second snapshot raw = %q, want %q", got, want)
	}
	if got, want := second.TranslationWarnings.ByKind["stable"], int64(1); got != want {
		t.Errorf("second snapshot warning count = %d, want %d", got, want)
	}
	if got, want := second.Retry.ByTrigger["stable"], int64(1); got != want {
		t.Errorf("second snapshot retry count = %d, want %d", got, want)
	}
	if second.Retry.Circuit.OpenUntil.IsZero() {
		t.Error("mutating snapshot circuit time changed State")
	}
}

func TestSetRateLimitDropsInvalidSanitizedRaw(t *testing.T) {
	state := New("invalid-raw")
	state.SetRateLimit(RateLimitSnapshot{SanitizedRaw: json.RawMessage(`{"broken":`)})

	encoded, err := json.Marshal(state.Snapshot())
	if err != nil {
		t.Fatalf("Marshal(Snapshot()) error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal(snapshot JSON) error = %v", err)
	}
	if got := objectAt(t, document, "rate_limit")["raw"]; got != nil {
		t.Fatalf("invalid raw JSON encoded as %v, want null", got)
	}
}

func TestBoundedCountersNormalizeOverflowAndSaturate(t *testing.T) {
	state := New("bounds")
	state.RecordWarnings(" \t\n", 2)
	state.RecordWarnings("hot", math.MaxInt64)
	state.RecordWarning("hot")
	for i := 0; i < MaxWarningKinds*2; i++ {
		state.RecordWarning(fmt.Sprintf("warning-%03d", i))
	}

	state.RecordRetries(" \t\n", 3)
	state.RecordRetries("hot", math.MaxInt64)
	state.RecordRetry("hot")
	for i := 0; i < MaxRetryTriggers*2; i++ {
		state.RecordRetry(fmt.Sprintf("retry-%03d", i))
	}

	snapshot := state.Snapshot()
	if got := len(snapshot.TranslationWarnings.ByKind); got != MaxWarningKinds {
		t.Fatalf("warning kinds = %d, want hard limit %d", got, MaxWarningKinds)
	}
	if got := len(snapshot.Retry.ByTrigger); got != MaxRetryTriggers {
		t.Fatalf("retry triggers = %d, want hard limit %d", got, MaxRetryTriggers)
	}
	if got, want := snapshot.TranslationWarnings.ByKind[UnknownCounterKey], int64(2); got != want {
		t.Errorf("normalized warning count = %d, want %d", got, want)
	}
	if got, want := snapshot.Retry.ByTrigger[UnknownCounterKey], int64(3); got != want {
		t.Errorf("normalized retry count = %d, want %d", got, want)
	}
	if got, want := snapshot.TranslationWarnings.ByKind[OverflowCounterKey], int64(MaxWarningKinds+3); got != want {
		t.Errorf("warning overflow = %d, want %d", got, want)
	}
	if got, want := snapshot.Retry.ByTrigger[OverflowCounterKey], int64(MaxRetryTriggers+3); got != want {
		t.Errorf("retry overflow = %d, want %d", got, want)
	}
	if got, want := snapshot.TranslationWarnings.ByKind["hot"], int64(math.MaxInt64); got != want {
		t.Errorf("hot warning = %d, want saturation %d", got, want)
	}
	if got, want := snapshot.Retry.ByTrigger["hot"], int64(math.MaxInt64); got != want {
		t.Errorf("hot retry = %d, want saturation %d", got, want)
	}
	if got, want := snapshot.TranslationWarnings.Total, int64(math.MaxInt64); got != want {
		t.Errorf("warning total = %d, want saturation %d", got, want)
	}
	if got, want := snapshot.Retry.Total, int64(math.MaxInt64); got != want {
		t.Errorf("retry total = %d, want saturation %d", got, want)
	}
}

func TestCatalogAgeUsesSnapshotClockAndNeverGoesNegative(t *testing.T) {
	now := time.Date(2026, time.July, 21, 16, 0, 0, 0, time.UTC)
	state := New("clock")
	state.now = func() time.Time { return now }

	state.SetCatalog("cache", now.Add(-1500*time.Millisecond))
	if got, want := state.Snapshot().Catalog.Age, "1.5s"; got != want {
		t.Fatalf("catalog age = %q, want %q", got, want)
	}
	state.SetCatalog("live", now.Add(time.Minute))
	if got, want := state.Snapshot().Catalog.Age, "0s"; got != want {
		t.Fatalf("future catalog age = %q, want clamped %q", got, want)
	}

	var zero State
	zeroSnapshot := zero.Snapshot()
	if zeroSnapshot.Catalog.Age != "" {
		t.Errorf("unset catalog age = %q, want empty", zeroSnapshot.Catalog.Age)
	}
	if zeroSnapshot.TranslationWarnings.ByKind == nil || zeroSnapshot.Retry.ByTrigger == nil {
		t.Error("zero-value State snapshot maps must be non-nil")
	}
	if _, err := json.Marshal(zeroSnapshot); err != nil {
		t.Fatalf("Marshal(zero State snapshot) error = %v", err)
	}
}

func TestBeginSessionEndIsIdempotentAndNeverNegative(t *testing.T) {
	var state State
	end := state.BeginSession()
	if got, want := state.Snapshot().ActiveSessions, int64(1); got != want {
		t.Fatalf("active sessions after begin = %d, want %d", got, want)
	}
	end()
	end()
	if got := state.Snapshot().ActiveSessions; got != 0 {
		t.Fatalf("active sessions after repeated end = %d, want 0", got)
	}
}

func TestStateConcurrentRecordSnapshotAndSessions(t *testing.T) {
	const (
		workers    = 64
		iterations = 100
	)
	state := New("concurrent")
	state.SetRemainingRetryBudget(-1)
	state.SetCircuit(CircuitState{Open: false, Failures: -1})

	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				end := state.BeginSession()
				state.RecordWarning(fmt.Sprintf("warning-%d-%d", worker, iteration))
				state.RecordRetry(fmt.Sprintf("retry-%d-%d", worker, iteration))
				snapshot := state.Snapshot()
				if snapshot.ActiveSessions < 0 {
					t.Errorf("active sessions = %d, want non-negative", snapshot.ActiveSessions)
				}
				if len(snapshot.TranslationWarnings.ByKind) > MaxWarningKinds {
					t.Errorf("warning kinds = %d, exceeds %d", len(snapshot.TranslationWarnings.ByKind), MaxWarningKinds)
				}
				if len(snapshot.Retry.ByTrigger) > MaxRetryTriggers {
					t.Errorf("retry triggers = %d, exceeds %d", len(snapshot.Retry.ByTrigger), MaxRetryTriggers)
				}
				end()
				end()
			}
		}()
	}
	wg.Wait()

	snapshot := state.Snapshot()
	if got, want := snapshot.TranslationWarnings.Total, int64(workers*iterations); got != want {
		t.Errorf("warning total = %d, want %d", got, want)
	}
	if got, want := snapshot.Retry.Total, int64(workers*iterations); got != want {
		t.Errorf("retry total = %d, want %d", got, want)
	}
	if snapshot.ActiveSessions != 0 {
		t.Errorf("active sessions = %d, want 0", snapshot.ActiveSessions)
	}
	if snapshot.Retry.RemainingBudget != 0 {
		t.Errorf("negative remaining budget normalized to %d, want 0", snapshot.Retry.RemainingBudget)
	}
	if snapshot.Retry.Circuit.Failures != 0 {
		t.Errorf("negative circuit failures normalized to %d, want 0", snapshot.Retry.Circuit.Failures)
	}
}

func assertKeys(t *testing.T, object map[string]any, want ...string) {
	t.Helper()
	got := make([]string, 0, len(object))
	for key := range object {
		got = append(got, key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON keys = %v, want %v", got, want)
	}
}

func objectAt(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("JSON object missing key %q", key)
	}
	nested, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("JSON key %q = %T, want object", key, value)
	}
	return nested
}

func TestRecordTimingBoundedRingAndSnapshot(t *testing.T) {
	state := New("timing")
	for i := 0; i < MaxTimingSamples+3; i++ {
		state.RecordTiming(TimingSample{
			TTFTMillis:       int64(i),
			TotalMillis:      int64(i * 10),
			UpstreamMillis:   int64(i * 2),
			FirstEventMillis: int64(i * 3),
			CommitMillis:     int64(i * 4),
			Attempts:         1 + i%3,
		})
	}
	snapshot := state.Snapshot()
	if got, want := snapshot.Timing.Count, int64(MaxTimingSamples+3); got != want {
		t.Fatalf("Timing.Count = %d, want %d", got, want)
	}
	if got, want := len(snapshot.Timing.Samples), MaxTimingSamples; got != want {
		t.Fatalf("len(Timing.Samples) = %d, want %d", got, want)
	}
	// Oldest evicted: the surviving window is [3, MaxTimingSamples+3).
	if got, want := snapshot.Timing.Samples[0].TTFTMillis, int64(3); got != want {
		t.Errorf("oldest surviving sample ttft = %d, want %d", got, want)
	}
	last := snapshot.Timing.Samples[len(snapshot.Timing.Samples)-1]
	if got, want := last.TTFTMillis, int64(MaxTimingSamples+2); got != want {
		t.Errorf("newest sample ttft = %d, want %d", got, want)
	}
	if got, want := last.Attempts, 1+(MaxTimingSamples+2)%3; got != want {
		t.Errorf("newest sample attempts = %d, want %d", got, want)
	}
	// Snapshot is detached: mutating the returned slice must not corrupt state.
	snapshot.Timing.Samples[0].TTFTMillis = 999999
	if got := state.Snapshot().Timing.Samples[0].TTFTMillis; got == 999999 {
		t.Error("Snapshot timing samples are not deep-copied")
	}
}

func TestSnapshotTimingEmptyMarshalsAsArray(t *testing.T) {
	encoded, err := json.Marshal(New("empty").Snapshot())
	if err != nil {
		t.Fatalf("Marshal(Snapshot()) error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal(snapshot JSON) error = %v", err)
	}
	timing := objectAt(t, document, "timing")
	if _, ok := timing["samples"].([]any); !ok {
		t.Fatalf("timing.samples = %v (%T), want [] — /status collections stay non-null", timing["samples"], timing["samples"])
	}
}

func TestSnapshotJSONContractIncludesTiming(t *testing.T) {
	state := New("timing-json")
	state.RecordTiming(TimingSample{TTFTMillis: 1500, TotalMillis: 12000, UpstreamMillis: 1400, FirstEventMillis: 1450, CommitMillis: 1490, Attempts: 2})
	encoded, err := json.Marshal(state.Snapshot())
	if err != nil {
		t.Fatalf("Marshal(Snapshot()) error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("Unmarshal(snapshot JSON) error = %v", err)
	}
	timing := objectAt(t, document, "timing")
	assertKeys(t, timing, "count", "samples")
	samples, ok := timing["samples"].([]any)
	if !ok || len(samples) != 1 {
		t.Fatalf("timing.samples = %v, want one sample", timing["samples"])
	}
	sample, ok := samples[0].(map[string]any)
	if !ok {
		t.Fatalf("timing.samples[0] = %v, want object", samples[0])
	}
	assertKeys(t, sample, "attempts", "commit_ms", "first_event_ms", "total_ms", "ttft_ms", "upstream_ms")
}
