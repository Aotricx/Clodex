// Package ratelimit parses Codex telemetry without treating snapshots as
// terminal errors. Only an actual terminal upstream failure decides status.
package ratelimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Aotricx/Clodex/internal/redact"
	"github.com/Aotricx/Clodex/internal/status"
)

type eventEnvelope struct {
	Type             string        `json:"type"`
	MeteredLimitName string        `json:"metered_limit_name"`
	LimitName        string        `json:"limit_name"`
	RateLimits       *eventLimits  `json:"rate_limits"`
	Credits          *eventCredits `json:"credits"`
}

type eventLimits struct {
	LimitReached bool         `json:"limit_reached"`
	Primary      *eventWindow `json:"primary"`
	Secondary    *eventWindow `json:"secondary"`
}

type eventWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	WindowMinutes     *int64   `json:"window_minutes"`
	ResetAt           *int64   `json:"reset_at"`
	ResetAfterSeconds *int64   `json:"reset_after_seconds"`
}

type eventCredits struct {
	HasCredits *bool   `json:"has_credits"`
	Unlimited  *bool   `json:"unlimited"`
	Balance    *string `json:"balance"`
}

// ParseEvent converts one codex.rate_limits event into status telemetry.
func ParseEvent(raw []byte, capturedAt time.Time) (status.RateLimitSnapshot, error) {
	var event eventEnvelope
	if err := json.Unmarshal(raw, &event); err != nil {
		return status.RateLimitSnapshot{}, fmt.Errorf("parse Codex rate-limit event: %w", err)
	}
	if event.Type != "codex.rate_limits" {
		return status.RateLimitSnapshot{}, errors.New("parse Codex rate-limit event: unexpected event type")
	}

	limitID := event.MeteredLimitName
	if limitID == "" {
		limitID = event.LimitName
	}
	if limitID == "" {
		limitID = "codex"
	}
	snapshot := status.RateLimitSnapshot{CapturedAt: capturedAt, LimitID: normalizeLimitID(limitID)}
	if event.RateLimits != nil {
		snapshot.LimitReached = event.RateLimits.LimitReached
		var err error
		if snapshot.Primary, err = convertWindow(event.RateLimits.Primary); err != nil {
			return status.RateLimitSnapshot{}, err
		}
		if snapshot.Secondary, err = convertWindow(event.RateLimits.Secondary); err != nil {
			return status.RateLimitSnapshot{}, err
		}
	}
	if event.Credits != nil {
		if event.Credits.HasCredits == nil || event.Credits.Unlimited == nil {
			return status.RateLimitSnapshot{}, errors.New("parse Codex rate-limit event: credits fields are incomplete")
		}
		snapshot.Credits = &status.CreditsSummary{
			HasCredits: *event.Credits.HasCredits,
			Unlimited:  *event.Credits.Unlimited,
			Balance:    clone(event.Credits.Balance),
		}
	}
	redacted, err := redact.JSON(raw)
	if err != nil {
		return status.RateLimitSnapshot{}, fmt.Errorf("sanitize Codex rate-limit event: %w", err)
	}
	snapshot.SanitizedRaw = redacted
	return snapshot, nil
}

// ParseHeaders parses the default Codex rate-limit header family. A partial or
// malformed family is ignored instead of fabricated into zero values.
func ParseHeaders(headers http.Header, capturedAt time.Time) (status.RateLimitSnapshot, bool) {
	primary := parseHeaderWindow(headers, "x-codex-primary")
	secondary := parseHeaderWindow(headers, "x-codex-secondary")
	credits := parseHeaderCredits(headers)
	if primary == nil && secondary == nil && credits == nil {
		return status.RateLimitSnapshot{}, false
	}
	return status.RateLimitSnapshot{
		CapturedAt: capturedAt,
		LimitID:    "codex",
		Primary:    primary,
		Secondary:  secondary,
		Credits:    credits,
	}, true
}

// CreditsExhausted reports explicit credit absence only. It never makes a
// snapshot terminal; the caller still requires a real terminal failure.
func CreditsExhausted(snapshot status.RateLimitSnapshot) bool {
	return snapshot.Credits != nil && !snapshot.Credits.HasCredits && !snapshot.Credits.Unlimited
}

// ApplyResponseHeaders exposes informational telemetry without fabricating an
// HTTP 429 or Retry-After header.
func ApplyResponseHeaders(headers http.Header, snapshot status.RateLimitSnapshot) {
	if headers == nil {
		return
	}
	headers.Set("X-Clodex-Rate-Limit-Reached", strconv.FormatBool(snapshot.LimitReached))
	if snapshot.Credits != nil {
		headers.Set("X-Clodex-Credits-Has-Credits", strconv.FormatBool(snapshot.Credits.HasCredits))
		headers.Set("X-Clodex-Credits-Unlimited", strconv.FormatBool(snapshot.Credits.Unlimited))
	}
	if snapshot.Primary != nil && snapshot.Primary.ResetAfterSeconds != nil {
		headers.Set("X-Clodex-Rate-Limit-Reset-After", strconv.FormatInt(*snapshot.Primary.ResetAfterSeconds, 10))
	}
}

func convertWindow(source *eventWindow) (*status.RateLimitWindow, error) {
	if source == nil {
		return nil, nil
	}
	if source.UsedPercent == nil || math.IsNaN(*source.UsedPercent) || math.IsInf(*source.UsedPercent, 0) || *source.UsedPercent < 0 {
		return nil, errors.New("parse Codex rate-limit event: invalid used_percent")
	}
	if negative(source.WindowMinutes) || negative(source.ResetAt) || negative(source.ResetAfterSeconds) {
		return nil, errors.New("parse Codex rate-limit event: negative window/reset value")
	}
	return &status.RateLimitWindow{
		UsedPercent:       *source.UsedPercent,
		WindowMinutes:     clone(source.WindowMinutes),
		ResetAt:           clone(source.ResetAt),
		ResetAfterSeconds: clone(source.ResetAfterSeconds),
	}, nil
}

func parseHeaderWindow(headers http.Header, prefix string) *status.RateLimitWindow {
	used, ok := parseFloat(headers.Get(prefix + "-used-percent"))
	if !ok || used < 0 {
		return nil
	}
	window, windowOK := parseOptionalInt(headers.Get(prefix + "-window-minutes"))
	reset, resetOK := parseOptionalInt(headers.Get(prefix + "-reset-at"))
	if !windowOK || !resetOK {
		return nil
	}
	if used == 0 && window == nil && reset == nil {
		return nil
	}
	return &status.RateLimitWindow{UsedPercent: used, WindowMinutes: window, ResetAt: reset}
}

func parseHeaderCredits(headers http.Header) *status.CreditsSummary {
	hasCredits, hasOK := parseBool(headers.Get("x-codex-credits-has-credits"))
	unlimited, unlimitedOK := parseBool(headers.Get("x-codex-credits-unlimited"))
	if !hasOK || !unlimitedOK {
		return nil
	}
	balanceValue := strings.TrimSpace(headers.Get("x-codex-credits-balance"))
	var balance *string
	if balanceValue != "" {
		balance = &balanceValue
	}
	return &status.CreditsSummary{HasCredits: hasCredits, Unlimited: unlimited, Balance: balance}
}

func parseFloat(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	return value, err == nil && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func parseOptionalInt(raw string) (*int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return nil, false
	}
	return &value, true
}

func parseBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "1":
		return true, true
	case "false", "0":
		return false, true
	default:
		return false, false
	}
}

func normalizeLimitID(value string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(value)), "-", "_")
}

func negative(value *int64) bool {
	return value != nil && *value < 0
}

func clone[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
