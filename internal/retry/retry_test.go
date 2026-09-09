package retry

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestDelayUsesCappedEqualJitter(t *testing.T) {
	clock := newFakeClock(time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))
	low := newController(t, clock, func() float64 { return 0 })
	high := newController(t, clock, func() float64 { return 1 })
	for _, tc := range []struct {
		attempt           int
		wantLow, wantHigh time.Duration
	}{
		{1, 50 * time.Millisecond, 100 * time.Millisecond},
		{2, 100 * time.Millisecond, 200 * time.Millisecond},
		{3, 200 * time.Millisecond, 400 * time.Millisecond},
		{4, 400 * time.Millisecond, 800 * time.Millisecond},
		{8, 500 * time.Millisecond, time.Second},
	} {
		if got, err := low.Delay(tc.attempt, ""); err != nil || got != tc.wantLow {
			t.Errorf("low Delay(%d) = %s, %v; want %s", tc.attempt, got, err, tc.wantLow)
		}
		if got, err := high.Delay(tc.attempt, ""); err != nil || got != tc.wantHigh {
			t.Errorf("high Delay(%d) = %s, %v; want %s", tc.attempt, got, err, tc.wantHigh)
		}
	}
}

func TestDelayHonorsRetryAfterSecondsAndDate(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(now)
	controller := newController(t, clock, func() float64 { return 0 })
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"3", 3 * time.Second},
		{"1.5", 1500 * time.Millisecond},
		{now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second},
		{"invalid", 50 * time.Millisecond},
		{now.Add(-time.Second).Format(http.TimeFormat), 0},
	} {
		got, err := controller.Delay(1, tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("Delay(%q) = %s, %v; want %s", tc.raw, got, err, tc.want)
		}
	}
	if _, err := controller.Delay(1, "60"); !errors.Is(err, ErrRetryAfterTooLong) {
		t.Fatalf("long Retry-After error = %v", err)
	}
}

func TestWaitDoesNotSleepAfterCancellationOrOverlongRetryAfter(t *testing.T) {
	clock := newFakeClock(time.Now())
	controller := newController(t, clock, func() float64 { return 0 })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.Wait(ctx, 1, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait canceled error = %v", err)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("canceled sleeps = %v", clock.sleeps)
	}
	if err := controller.Wait(context.Background(), 1, "60"); !errors.Is(err, ErrRetryAfterTooLong) {
		t.Fatalf("Wait long error = %v", err)
	}
	if len(clock.sleeps) != 0 {
		t.Fatalf("overlong sleeps = %v", clock.sleeps)
	}
}

func TestGlobalRetryBudgetRefillsByWindow(t *testing.T) {
	clock := newFakeClock(time.Now())
	controller := newController(t, clock, func() float64 { return 0 })
	if err := controller.ReserveRetry(); err != nil {
		t.Fatal(err)
	}
	if err := controller.ReserveRetry(); err != nil {
		t.Fatal(err)
	}
	if err := controller.ReserveRetry(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("third reserve error = %v", err)
	}
	if got := controller.Snapshot().RemainingBudget; got != 0 {
		t.Fatalf("remaining = %d", got)
	}
	clock.advance(time.Minute)
	if err := controller.ReserveRetry(); err != nil {
		t.Fatalf("reserve after refill = %v", err)
	}
	if got := controller.Snapshot().RemainingBudget; got != 1 {
		t.Fatalf("remaining after refill = %d", got)
	}
}

func TestCircuitBreakerClosedOpenHalfOpen(t *testing.T) {
	clock := newFakeClock(time.Now())
	controller := newController(t, clock, func() float64 { return 0 })
	if err := controller.Allow(); err != nil {
		t.Fatal(err)
	}
	controller.Failed(true)
	controller.Failed(true)
	if err := controller.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("open Allow error = %v", err)
	}
	if snapshot := controller.Snapshot(); snapshot.State != StateOpen || snapshot.Failures != 2 {
		t.Fatalf("open snapshot = %+v", snapshot)
	}

	clock.advance(10 * time.Second)
	if err := controller.Allow(); err != nil {
		t.Fatalf("half-open probe Allow = %v", err)
	}
	if err := controller.Allow(); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("concurrent half-open Allow = %v", err)
	}
	controller.Succeeded()
	if snapshot := controller.Snapshot(); snapshot.State != StateClosed || snapshot.Failures != 0 {
		t.Fatalf("closed snapshot = %+v", snapshot)
	}

	controller.Failed(true)
	controller.Failed(true)
	clock.advance(10 * time.Second)
	if err := controller.Allow(); err != nil {
		t.Fatal(err)
	}
	controller.Failed(true)
	if snapshot := controller.Snapshot(); snapshot.State != StateOpen || snapshot.OpenUntil == nil || !snapshot.OpenUntil.Equal(clock.Now().Add(10*time.Second)) {
		t.Fatalf("reopened snapshot = %+v", snapshot)
	}
}

func TestFailedTwiceWhileOpenKeepsFirstOpenUntil(t *testing.T) {
	clock := newFakeClock(time.Now())
	controller := newController(t, clock, func() float64 { return 0 })
	controller.Failed(true)
	controller.Failed(true)
	first := controller.Snapshot()
	if first.State != StateOpen || first.OpenUntil == nil {
		t.Fatalf("open snapshot = %+v", first)
	}
	deadline := *first.OpenUntil

	clock.advance(time.Second)
	controller.Failed(true)
	second := controller.Snapshot()
	if second.State != StateOpen || second.OpenUntil == nil || !second.OpenUntil.Equal(deadline) {
		t.Fatalf("openUntil after Failed while Open = %v, want %s", second.OpenUntil, deadline)
	}
}

func TestPermanentFailureDoesNotTripCircuit(t *testing.T) {
	clock := newFakeClock(time.Now())
	controller := newController(t, clock, func() float64 { return 0 })
	for range 10 {
		controller.Failed(false)
	}
	if snapshot := controller.Snapshot(); snapshot.State != StateClosed || snapshot.Failures != 0 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestRetryBudgetConcurrentBound(t *testing.T) {
	clock := newFakeClock(time.Now())
	controller := newController(t, clock, func() float64 { return 0 })
	var wait sync.WaitGroup
	var mutex sync.Mutex
	successes := 0
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if controller.ReserveRetry() == nil {
				mutex.Lock()
				successes++
				mutex.Unlock()
			}
		}()
	}
	wait.Wait()
	if successes != 2 {
		t.Fatalf("successful reserves = %d, want 2", successes)
	}
}

func newController(t *testing.T, clock *fakeClock, random func() float64) *Controller {
	t.Helper()
	controller, err := New(Config{
		BaseDelay:        100 * time.Millisecond,
		MaxDelay:         time.Second,
		RetryAfterLimit:  5 * time.Second,
		Budget:           2,
		BudgetWindow:     time.Minute,
		FailureThreshold: 2,
		CircuitCooldown:  10 * time.Second,
	}, clock, random)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

type fakeClock struct {
	mutex  sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (clock *fakeClock) Now() time.Time {
	clock.mutex.Lock()
	defer clock.mutex.Unlock()
	return clock.now
}

func (clock *fakeClock) Sleep(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	clock.mutex.Lock()
	clock.sleeps = append(clock.sleeps, delay)
	clock.now = clock.now.Add(delay)
	clock.mutex.Unlock()
	return nil
}

func (clock *fakeClock) advance(delay time.Duration) {
	clock.mutex.Lock()
	clock.now = clock.now.Add(delay)
	clock.mutex.Unlock()
}
