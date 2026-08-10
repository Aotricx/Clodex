// Package retry bounds transient retries and protects the shared ChatGPT account.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrBudgetExhausted   = errors.New("global retry budget exhausted")
	ErrCircuitOpen       = errors.New("retry circuit is open")
	ErrRetryAfterTooLong = errors.New("Retry-After exceeds configured limit")
)

// State is the circuit-breaker state.
type State string

const (
	StateClosed   State = "closed"
	StateOpen     State = "open"
	StateHalfOpen State = "half_open"
)

// Config defines bounded retry and breaker behavior.
type Config struct {
	BaseDelay        time.Duration
	MaxDelay         time.Duration
	RetryAfterLimit  time.Duration
	Budget           int
	BudgetWindow     time.Duration
	FailureThreshold int
	CircuitCooldown  time.Duration
}

// Clock makes waiting and time-window behavior deterministic under test.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// Snapshot is a detached view of retry protection state.
type Snapshot struct {
	RemainingBudget int
	State           State
	Failures        int
	OpenUntil       *time.Time
}

// Controller owns one process-wide retry budget and circuit breaker.
type Controller struct {
	mu     sync.Mutex
	config Config
	clock  Clock
	random func() float64

	remaining   int
	windowStart time.Time
	state       State
	failures    int
	openUntil   time.Time
	halfProbe   bool
}

// New validates configuration and creates a closed controller.
func New(config Config, clock Clock, random func() float64) (*Controller, error) {
	if config.BaseDelay <= 0 || config.MaxDelay < config.BaseDelay {
		return nil, errors.New("retry delays must be positive and max must not be below base")
	}
	if config.RetryAfterLimit <= 0 {
		return nil, errors.New("Retry-After limit must be positive")
	}
	if config.Budget < 1 || config.BudgetWindow <= 0 {
		return nil, errors.New("retry budget and window must be positive")
	}
	if config.FailureThreshold < 1 || config.CircuitCooldown <= 0 {
		return nil, errors.New("circuit threshold and cooldown must be positive")
	}
	if clock == nil || random == nil {
		return nil, errors.New("retry clock and random source are required")
	}
	now := clock.Now()
	return &Controller{
		config:      config,
		clock:       clock,
		random:      random,
		remaining:   config.Budget,
		windowStart: now,
		state:       StateClosed,
	}, nil
}

// Allow permits a normal attempt or the sole half-open probe.
func (controller *Controller) Allow() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	now := controller.clock.Now()
	switch controller.state {
	case StateOpen:
		if now.Before(controller.openUntil) {
			return ErrCircuitOpen
		}
		controller.state = StateHalfOpen
		controller.halfProbe = true
		return nil
	case StateHalfOpen:
		if controller.halfProbe {
			return ErrCircuitOpen
		}
		controller.halfProbe = true
	}
	return nil
}

// ReserveRetry consumes one retry token. Initial attempts do not call it.
func (controller *Controller) ReserveRetry() error {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.refillLocked(controller.clock.Now())
	if controller.remaining == 0 {
		return ErrBudgetExhausted
	}
	controller.remaining--
	return nil
}

// Delay calculates one retry delay. attempt is one for the first retry.
// A valid Retry-After takes precedence over local exponential jitter.
func (controller *Controller) Delay(attempt int, retryAfter string) (time.Duration, error) {
	if delay, valid := parseRetryAfter(retryAfter, controller.clock.Now()); valid {
		if delay > controller.config.RetryAfterLimit {
			return 0, fmt.Errorf("%w: %s", ErrRetryAfterTooLong, delay)
		}
		return delay, nil
	}
	if attempt < 1 {
		attempt = 1
	}
	ceiling := controller.config.BaseDelay
	for step := 1; step < attempt && ceiling < controller.config.MaxDelay; step++ {
		if ceiling > controller.config.MaxDelay/2 {
			ceiling = controller.config.MaxDelay
			break
		}
		ceiling *= 2
	}
	if ceiling > controller.config.MaxDelay {
		ceiling = controller.config.MaxDelay
	}
	controller.mu.Lock()
	random := controller.random()
	controller.mu.Unlock()
	if math.IsNaN(random) || random < 0 {
		random = 0
	} else if random > 1 {
		random = 1
	}
	half := ceiling / 2
	return half + time.Duration(random*float64(ceiling-half)), nil
}

// Wait sleeps for one calculated delay, respecting cancellation before and
// during the sleep.
func (controller *Controller) Wait(ctx context.Context, attempt int, retryAfter string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	delay, err := controller.Delay(attempt, retryAfter)
	if err != nil {
		return err
	}
	if delay == 0 {
		return ctx.Err()
	}
	return controller.clock.Sleep(ctx, delay)
}

// Failed records a retryable transport/upstream failure. Permanent failures
// never contribute to the breaker and release a half-open probe.
func (controller *Controller) Failed(retryable bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if !retryable {
		if controller.state == StateHalfOpen {
			controller.resetLocked()
		}
		return
	}
	controller.failures++
	if controller.state == StateHalfOpen || controller.failures >= controller.config.FailureThreshold {
		controller.state = StateOpen
		controller.openUntil = controller.clock.Now().Add(controller.config.CircuitCooldown)
		controller.halfProbe = false
	}
}

// Succeeded closes the breaker and resets its consecutive failure count.
func (controller *Controller) Succeeded() {
	controller.mu.Lock()
	controller.resetLocked()
	controller.mu.Unlock()
}

// Snapshot returns race-safe retry state.
func (controller *Controller) Snapshot() Snapshot {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.refillLocked(controller.clock.Now())
	snapshot := Snapshot{
		RemainingBudget: controller.remaining,
		State:           controller.state,
		Failures:        controller.failures,
	}
	if controller.state == StateOpen {
		openUntil := controller.openUntil
		snapshot.OpenUntil = &openUntil
	}
	return snapshot
}

func (controller *Controller) refillLocked(now time.Time) {
	if now.Before(controller.windowStart) || now.Sub(controller.windowStart) >= controller.config.BudgetWindow {
		controller.remaining = controller.config.Budget
		controller.windowStart = now
	}
}

func (controller *Controller) resetLocked() {
	controller.state = StateClosed
	controller.failures = 0
	controller.openUntil = time.Time{}
	controller.halfProbe = false
}

func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil && seconds >= 0 && !math.IsInf(seconds, 0) && !math.IsNaN(seconds) {
		return time.Duration(seconds * float64(time.Second)), true
	}
	date, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	delay := date.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// NewReal builds a controller using wall time and the supplied race-safe
// random function.
func NewReal(config Config, random func() float64) (*Controller, error) {
	return New(config, realClock{}, random)
}
