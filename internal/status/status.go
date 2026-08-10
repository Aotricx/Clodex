// Package status maintains bounded, race-safe process telemetry for /status.
package status

import (
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	// MaxWarningKinds is the maximum number of warning counter keys, including
	// the overflow bucket, retained by one State.
	MaxWarningKinds = 128
	// MaxRetryTriggers is the maximum number of retry counter keys, including
	// the overflow bucket, retained by one State.
	MaxRetryTriggers = 128
	// UnknownCounterKey receives empty or whitespace-only counter keys.
	UnknownCounterKey = "unknown"
	// OverflowCounterKey receives counters beyond a bounded key limit.
	OverflowCounterKey = "_other"

	// MaxTimingSamples bounds the per-call timing ring retained by one State:
	// appending past the cap evicts the oldest sample.
	MaxTimingSamples = 64

	maxCounterKeyBytes = 128
)

// AuthSummary is deliberately limited to redacted, non-credential auth data.
type AuthSummary struct {
	Source  string     `json:"source"`
	Account string     `json:"account"`
	Plan    string     `json:"plan"`
	Expiry  *time.Time `json:"expiry"`
}

// CatalogSummary reports model-catalog provenance and age at snapshot time.
type CatalogSummary struct {
	Source    string     `json:"source"`
	FetchedAt *time.Time `json:"fetched_at"`
	Age       string     `json:"age"`
}

// RateLimitWindow is one backend rate-limit time window. ResetAt is a Unix
// timestamp; ResetAfterSeconds preserves event snapshots that report a
// relative reset instead.
type RateLimitWindow struct {
	UsedPercent       float64 `json:"used_percent"`
	WindowMinutes     *int64  `json:"window_minutes"`
	ResetAt           *int64  `json:"reset_at"`
	ResetAfterSeconds *int64  `json:"reset_after_seconds"`
}

// CreditsSummary reports the backend's non-secret credit state. Balance stays
// a string because the backend representation is decimal text.
type CreditsSummary struct {
	HasCredits bool    `json:"has_credits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

// RateLimitSnapshot is the latest backend rate-limit observation.
// SanitizedRaw may contain redactor-approved forward-compatible JSON only.
type RateLimitSnapshot struct {
	CapturedAt   time.Time        `json:"captured_at"`
	LimitID      string           `json:"limit_id"`
	LimitName    string           `json:"limit_name"`
	LimitReached bool             `json:"limit_reached"`
	Primary      *RateLimitWindow `json:"primary"`
	Secondary    *RateLimitWindow `json:"secondary"`
	Credits      *CreditsSummary  `json:"credits"`
	SanitizedRaw json.RawMessage  `json:"raw"`
}

// WarningSummary contains bounded translation-warning counts.
type WarningSummary struct {
	Total  int64            `json:"total"`
	ByKind map[string]int64 `json:"by_kind"`
}

// CircuitState reports global circuit-breaker state.
type CircuitState struct {
	Open      bool       `json:"open"`
	Failures  int64      `json:"failures"`
	OpenUntil *time.Time `json:"open_until"`
}

// RetrySummary contains bounded retry counts, remaining process-wide retry
// budget, and circuit-breaker state.
type RetrySummary struct {
	Total           int64            `json:"total"`
	ByTrigger       map[string]int64 `json:"by_trigger"`
	RemainingBudget int64            `json:"remaining_budget"`
	Circuit         CircuitState     `json:"circuit"`
}

// TimingSample is one completed call's latency decomposition. Durations and
// counts only — timing telemetry carries no wire content, identifiers, or
// anything the redact rules govern.
type TimingSample struct {
	// TTFTMillis is request received → response committed to the client:
	// streaming = the commit that releases the 200 status (the first SSE
	// frame follows within the same drain), buffered = the start of the
	// body write. It measures when the proxy stops withholding output, not
	// kernel-level wire delivery under backpressure.
	TTFTMillis int64 `json:"ttft_ms"`
	// TotalMillis is request received → turn finished.
	TotalMillis int64 `json:"total_ms"`
	// UpstreamMillis is the last attempt's transport wait: upstream request
	// sent → response headers received.
	UpstreamMillis int64 `json:"upstream_ms"`
	// FirstEventMillis is upstream headers → first SSE event decoded
	// (last attempt).
	FirstEventMillis int64 `json:"first_event_ms"`
	// CommitMillis is upstream headers → commit-gate release (first client-
	// visible frame authorized); zero for buffered turns, which never commit.
	CommitMillis int64 `json:"commit_ms"`
	// Attempts counts transport attempts, so retry/backoff waits are visible
	// when interpreting the per-attempt legs above.
	Attempts int `json:"attempts"`
}

// TimingSummary is the bounded per-call timing telemetry: a saturating total
// count plus the last MaxTimingSamples samples, oldest first.
type TimingSummary struct {
	Count   int64          `json:"count"`
	Samples []TimingSample `json:"samples"`
}

// Snapshot is a detached, JSON-ready view of State. Its maps, raw JSON, and
// pointers never alias State storage.
type Snapshot struct {
	Version             string             `json:"version"`
	Auth                AuthSummary        `json:"auth"`
	Catalog             CatalogSummary     `json:"catalog"`
	RateLimit           *RateLimitSnapshot `json:"rate_limit"`
	TranslationWarnings WarningSummary     `json:"translation_warnings"`
	Retry               RetrySummary       `json:"retry"`
	Timing              TimingSummary      `json:"timing"`
	ActiveSessions      int64              `json:"active_sessions"`
}

type boundedCounters struct {
	total int64
	byKey map[string]int64
}

// State owns process-wide status telemetry. Its zero value is ready for use.
type State struct {
	mu sync.RWMutex

	now     func() time.Time
	version string
	auth    AuthSummary
	catalog CatalogSummary
	rate    *RateLimitSnapshot

	warnings boundedCounters
	retries  boundedCounters
	budget   int64
	circuit  CircuitState
	sessions int64

	timingCount   int64
	timingSamples []TimingSample
}

// New constructs an empty State reporting version.
func New(version string) *State {
	return &State{version: version, now: time.Now}
}

// SetVersion replaces the reported build version.
func (s *State) SetVersion(version string) {
	s.mu.Lock()
	s.version = version
	s.mu.Unlock()
}

// SetAuth replaces the redacted auth summary.
func (s *State) SetAuth(summary AuthSummary) {
	copy := cloneAuth(summary)
	s.mu.Lock()
	s.auth = copy
	s.mu.Unlock()
}

// SetCatalog records catalog provenance and fetch time. Age is derived when a
// snapshot is taken, so it cannot become stale in State.
func (s *State) SetCatalog(source string, fetchedAt time.Time) {
	s.mu.Lock()
	s.catalog = CatalogSummary{Source: source, FetchedAt: clonePtr(&fetchedAt)}
	s.mu.Unlock()
}

// ClearCatalog removes the current catalog observation.
func (s *State) ClearCatalog() {
	s.mu.Lock()
	s.catalog = CatalogSummary{}
	s.mu.Unlock()
}

// SetRateLimit replaces the latest rate-limit observation. All mutable data is
// copied before it enters State.
func (s *State) SetRateLimit(snapshot RateLimitSnapshot) {
	copy := cloneRateLimit(snapshot)
	s.mu.Lock()
	s.rate = &copy
	s.mu.Unlock()
}

// ClearRateLimit removes the latest rate-limit observation.
func (s *State) ClearRateLimit() {
	s.mu.Lock()
	s.rate = nil
	s.mu.Unlock()
}

// RecordWarning increments one normalized translation-warning counter.
func (s *State) RecordWarning(kind string) {
	s.RecordWarnings(kind, 1)
}

// RecordWarnings adds count to one normalized translation-warning counter.
// Non-positive counts do not change State.
func (s *State) RecordWarnings(kind string, count int64) {
	if count <= 0 {
		return
	}
	key := normalizeCounterKey(kind)
	s.mu.Lock()
	s.warnings.add(key, count, MaxWarningKinds)
	s.mu.Unlock()
}

// RecordRetry increments one normalized retry-trigger counter.
func (s *State) RecordRetry(trigger string) {
	s.RecordRetries(trigger, 1)
}

// RecordRetries adds count to one normalized retry-trigger counter.
// Non-positive counts do not change State.
func (s *State) RecordRetries(trigger string, count int64) {
	if count <= 0 {
		return
	}
	key := normalizeCounterKey(trigger)
	s.mu.Lock()
	s.retries.add(key, count, MaxRetryTriggers)
	s.mu.Unlock()
}

// RecordTiming appends one completed call's timing sample to the bounded
// ring, evicting the oldest sample past MaxTimingSamples.
func (s *State) RecordTiming(sample TimingSample) {
	s.mu.Lock()
	s.timingCount = saturatingAdd(s.timingCount, 1)
	s.timingSamples = append(s.timingSamples, sample)
	if len(s.timingSamples) > MaxTimingSamples {
		// Shift-in-place keeps one backing array instead of re-slicing the
		// head, which would pin evicted samples and grow without bound.
		copy(s.timingSamples, s.timingSamples[len(s.timingSamples)-MaxTimingSamples:])
		s.timingSamples = s.timingSamples[:MaxTimingSamples]
	}
	s.mu.Unlock()
}

// SetRemainingRetryBudget replaces the process-wide remaining retry budget.
// Negative inputs are reported as zero.
func (s *State) SetRemainingRetryBudget(remaining int64) {
	if remaining < 0 {
		remaining = 0
	}
	s.mu.Lock()
	s.budget = remaining
	s.mu.Unlock()
}

// SetCircuit replaces the circuit-breaker state. Mutable timestamps are copied
// and negative failure counts are reported as zero.
func (s *State) SetCircuit(circuit CircuitState) {
	copy := cloneCircuit(circuit)
	if copy.Failures < 0 {
		copy.Failures = 0
	}
	s.mu.Lock()
	s.circuit = copy
	s.mu.Unlock()
}

// BeginSession increments the active-session count and returns an idempotent end
// function. Saturated sessions are not counted and therefore are not decremented.
func (s *State) BeginSession() func() {
	s.mu.Lock()
	counted := s.sessions < math.MaxInt64
	if counted {
		s.sessions++
	}
	s.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			if !counted {
				return
			}
			s.mu.Lock()
			if s.sessions > 0 {
				s.sessions--
			}
			s.mu.Unlock()
		})
	}
}

// Snapshot returns a detached point-in-time view of State.
func (s *State) Snapshot() Snapshot {
	s.mu.RLock()
	clock := s.now
	snapshot := Snapshot{
		Version:             s.version,
		Auth:                cloneAuth(s.auth),
		Catalog:             cloneCatalog(s.catalog),
		TranslationWarnings: s.warnings.snapshot(),
		Retry: RetrySummary{
			Total:           s.retries.total,
			ByTrigger:       cloneMap(s.retries.byKey),
			RemainingBudget: s.budget,
			Circuit:         cloneCircuit(s.circuit),
		},
		Timing: TimingSummary{
			Count: s.timingCount,
			// make (not append-to-nil) so an empty ring marshals as [],
			// matching the stable non-null collections /status guarantees.
			Samples: append(make([]TimingSample, 0, len(s.timingSamples)), s.timingSamples...),
		},
		ActiveSessions: s.sessions,
	}
	if s.rate != nil {
		rate := cloneRateLimit(*s.rate)
		snapshot.RateLimit = &rate
	}
	s.mu.RUnlock()

	if clock == nil {
		clock = time.Now
	}
	if snapshot.Catalog.FetchedAt != nil && !snapshot.Catalog.FetchedAt.IsZero() {
		age := clock().Sub(*snapshot.Catalog.FetchedAt)
		if age < 0 {
			age = 0
		}
		snapshot.Catalog.Age = age.String()
	}
	return snapshot
}

func (c *boundedCounters) add(key string, count int64, limit int) {
	if c.byKey == nil {
		c.byKey = make(map[string]int64, limit)
	}
	c.total = saturatingAdd(c.total, count)
	if key == OverflowCounterKey {
		c.byKey[key] = saturatingAdd(c.byKey[key], count)
		return
	}
	if current, exists := c.byKey[key]; exists {
		c.byKey[key] = saturatingAdd(current, count)
		return
	}

	specificKeys := len(c.byKey)
	if _, exists := c.byKey[OverflowCounterKey]; exists {
		specificKeys--
	}
	if specificKeys < limit-1 {
		c.byKey[key] = count
		return
	}
	c.byKey[OverflowCounterKey] = saturatingAdd(c.byKey[OverflowCounterKey], count)
}

func (c boundedCounters) snapshot() WarningSummary {
	return WarningSummary{Total: c.total, ByKind: cloneMap(c.byKey)}
}

func normalizeCounterKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return UnknownCounterKey
	}
	if len(value) > maxCounterKeyBytes {
		value = value[:maxCounterKeyBytes]
	}
	return value
}

func saturatingAdd(current, increment int64) int64 {
	if increment <= 0 {
		return current
	}
	if current >= math.MaxInt64-increment {
		return math.MaxInt64
	}
	return current + increment
}

func cloneAuth(summary AuthSummary) AuthSummary {
	summary.Expiry = clonePtr(summary.Expiry)
	return summary
}

func cloneCatalog(summary CatalogSummary) CatalogSummary {
	summary.FetchedAt = clonePtr(summary.FetchedAt)
	return summary
}

func cloneCircuit(circuit CircuitState) CircuitState {
	circuit.OpenUntil = clonePtr(circuit.OpenUntil)
	return circuit
}

func cloneRateLimit(snapshot RateLimitSnapshot) RateLimitSnapshot {
	if snapshot.Primary != nil {
		primary := cloneRateLimitWindow(*snapshot.Primary)
		snapshot.Primary = &primary
	}
	if snapshot.Secondary != nil {
		secondary := cloneRateLimitWindow(*snapshot.Secondary)
		snapshot.Secondary = &secondary
	}
	if snapshot.Credits != nil {
		credits := *snapshot.Credits
		credits.Balance = clonePtr(credits.Balance)
		snapshot.Credits = &credits
	}
	if !json.Valid(snapshot.SanitizedRaw) {
		snapshot.SanitizedRaw = nil
	} else {
		snapshot.SanitizedRaw = append(json.RawMessage(nil), snapshot.SanitizedRaw...)
	}
	return snapshot
}

func cloneRateLimitWindow(window RateLimitWindow) RateLimitWindow {
	window.WindowMinutes = clonePtr(window.WindowMinutes)
	window.ResetAt = clonePtr(window.ResetAt)
	window.ResetAfterSeconds = clonePtr(window.ResetAfterSeconds)
	return window
}

func cloneMap(source map[string]int64) map[string]int64 {
	copy := make(map[string]int64, len(source))
	for key, value := range source {
		copy[key] = value
	}
	return copy
}

func clonePtr[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
