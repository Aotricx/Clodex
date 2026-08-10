// Package auth reads and updates Codex CLI ChatGPT OAuth state.
package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	CodexCLISource  = "codex-cli"
	accessTokenSkew = 5 * time.Minute
	refreshMaxAge   = 8 * 24 * time.Hour
)

// Claims is the unverified subset of Codex JWT claims needed by Clodex.
// Parsing checks JWT structure and payload encoding only; it does not verify a
// signature, issuer, audience, or trust chain.
type Claims struct {
	ExpiresAt             *time.Time
	Email                 string
	Plan                  string
	ChatGPTAccountID      string
	ChatGPTUserID         string
	ChatGPTAccountFedRAMP bool
}

// Summary contains only non-credential, redacted auth status.
type Summary struct {
	Source  string     `json:"source"`
	Account string     `json:"account"`
	Plan    string     `json:"plan"`
	Expiry  *time.Time `json:"expiry"`
}

type jwtPayload struct {
	ExpiresAt *int64 `json:"exp"`
	Email     string `json:"email"`
	Profile   *struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
	Auth *authClaims `json:"https://api.openai.com/auth"`

	ChatGPTPlanType         string `json:"chatgpt_plan_type"`
	ChatGPTAccountID        string `json:"chatgpt_account_id"`
	ChatGPTUserID           string `json:"chatgpt_user_id"`
	UserID                  string `json:"user_id"`
	ChatGPTAccountIsFedRAMP bool   `json:"chatgpt_account_is_fedramp"`
}

type authClaims struct {
	Plan                  string `json:"chatgpt_plan_type"`
	ChatGPTAccountID      string `json:"chatgpt_account_id"`
	ChatGPTUserID         string `json:"chatgpt_user_id"`
	UserID                string `json:"user_id"`
	ChatGPTAccountFedRAMP bool   `json:"chatgpt_account_is_fedramp"`
}

// ParseJWTClaims decodes an unverified JWT payload using unpadded base64url,
// matching Codex token-data parsing. Token material is never included in an
// error.
func ParseJWTClaims(token string) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Claims{}, errors.New("invalid JWT format")
	}
	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, errors.New("invalid JWT payload encoding")
	}
	trimmedPayload := bytes.TrimSpace(payloadJSON)
	if len(trimmedPayload) == 0 || trimmedPayload[0] != '{' {
		return Claims{}, errors.New("invalid JWT payload JSON object")
	}

	var payload jwtPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return Claims{}, errors.New("invalid JWT payload JSON")
	}

	direct := authClaims{
		Plan:                  payload.ChatGPTPlanType,
		ChatGPTAccountID:      payload.ChatGPTAccountID,
		ChatGPTUserID:         payload.ChatGPTUserID,
		UserID:                payload.UserID,
		ChatGPTAccountFedRAMP: payload.ChatGPTAccountIsFedRAMP,
	}
	selected := direct
	if payload.Auth != nil {
		selected = mergeAuthClaims(*payload.Auth, direct)
	}

	claims := Claims{
		Email:                 payload.Email,
		Plan:                  selected.Plan,
		ChatGPTAccountID:      selected.ChatGPTAccountID,
		ChatGPTUserID:         firstNonEmpty(selected.ChatGPTUserID, selected.UserID),
		ChatGPTAccountFedRAMP: selected.ChatGPTAccountFedRAMP,
	}
	if claims.Email == "" && payload.Profile != nil {
		claims.Email = payload.Profile.Email
	}
	if payload.ExpiresAt != nil {
		expiresAt := time.Unix(*payload.ExpiresAt, 0).UTC()
		if expiresAt.Year() < 1 || expiresAt.Year() > 9999 {
			return Claims{}, errors.New("JWT expiration is outside RFC3339 range")
		}
		claims.ExpiresAt = &expiresAt
	}
	return claims, nil
}

func mergeAuthClaims(primary, fallback authClaims) authClaims {
	return authClaims{
		Plan:                  firstNonEmpty(primary.Plan, fallback.Plan),
		ChatGPTAccountID:      firstNonEmpty(primary.ChatGPTAccountID, fallback.ChatGPTAccountID),
		ChatGPTUserID:         firstNonEmpty(primary.ChatGPTUserID, fallback.ChatGPTUserID),
		UserID:                firstNonEmpty(primary.UserID, fallback.UserID),
		ChatGPTAccountFedRAMP: primary.ChatGPTAccountFedRAMP,
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// NormalizePlan applies pinned Codex plan labels while keeping unknown backend
// values readable.
func NormalizePlan(raw string) string {
	trimmed := strings.TrimSpace(raw)
	switch strings.ToLower(trimmed) {
	case "free":
		return "Free"
	case "go":
		return "Go"
	case "plus":
		return "Plus"
	case "pro":
		return "Pro"
	case "prolite":
		return "Pro Lite"
	case "team":
		return "Team"
	case "self_serve_business_usage_based":
		return "Self Serve Business Usage Based"
	case "business":
		return "Business"
	case "enterprise_cbp_usage_based":
		return "Enterprise CBP Usage Based"
	case "enterprise", "hc":
		return "Enterprise"
	case "education", "edu":
		return "Edu"
	case "":
		return "Unknown"
	default:
		return trimmed
	}
}

// Summary returns redacted status derived from this Codex auth file.
func (f *File) Summary() (Summary, error) {
	if f == nil {
		return Summary{}, errors.New("summarize auth: nil file")
	}
	accessClaims, err := ParseJWTClaims(f.Tokens.AccessToken)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize auth: invalid access_token: %w", err)
	}
	idClaims, err := ParseJWTClaims(f.Tokens.IDToken)
	if err != nil {
		return Summary{}, fmt.Errorf("summarize auth: invalid id_token: %w", err)
	}

	identity := ""
	if f.Tokens.AccountID != nil {
		identity = *f.Tokens.AccountID
	}
	identity = firstNonEmpty(identity, idClaims.ChatGPTAccountID, accessClaims.ChatGPTAccountID,
		idClaims.Email, accessClaims.Email, idClaims.ChatGPTUserID, accessClaims.ChatGPTUserID)
	account := "unknown"
	if identity != "" {
		account = maskIdentity(identity)
	}

	expiry := accessClaims.ExpiresAt
	if expiry == nil {
		expiry = idClaims.ExpiresAt
	}
	return Summary{
		Source:  CodexCLISource,
		Account: account,
		Plan:    NormalizePlan(firstNonEmpty(idClaims.Plan, accessClaims.Plan)),
		Expiry:  cloneTime(expiry),
	}, nil
}

// Expired reports whether the best available access/ID expiry is at or before
// now. A JWT without either expiration is not reported as expired.
func (f *File) Expired(now time.Time) (bool, error) {
	summary, err := f.Summary()
	if err != nil {
		return false, err
	}
	return summary.Expired(now), nil
}

// Expired reports whether Summary expiry is at or before now.
func (s Summary) Expired(now time.Time) bool {
	return s.Expiry != nil && !s.Expiry.After(now)
}

// RefreshDue follows Codex proactive refresh rules: an access-token expiration
// at or within five minutes wins; when access exp is absent, refresh age older
// than eight days is used. ID-token expiry is intentionally not used here.
func (f *File) RefreshDue(now time.Time) (bool, error) {
	if f == nil {
		return false, errors.New("check refresh: nil file")
	}
	accessClaims, err := ParseJWTClaims(f.Tokens.AccessToken)
	if err != nil {
		return false, fmt.Errorf("check refresh: invalid access_token: %w", err)
	}
	if accessClaims.ExpiresAt != nil {
		return !accessClaims.ExpiresAt.After(now.Add(accessTokenSkew)), nil
	}
	return f.LastRefresh.Before(now.Add(-refreshMaxAge)), nil
}

func maskIdentity(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return "sha256:" + hex.EncodeToString(digest[:6])
}

func cloneTime(source *time.Time) *time.Time {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}
