package auth

import (
	"errors"
	"strings"
	"time"

	"github.com/Aotricx/Clodex/internal/oauth"
)

// SaveLogin persists one complete browser or device OAuth login in Codex's
// auth.json format. OAuth response-only fields, such as expires_in, are not
// part of that format and are intentionally ignored.
func (s *Store) SaveLogin(tokens oauth.TokenSet, now time.Time) (*File, error) {
	if err := s.validatePath(); err != nil {
		return nil, err
	}

	accountID, lastRefresh, err := validateLogin(tokens, now)
	if err != nil {
		return nil, err
	}

	return s.UpdateAtomically(func(current *File) error {
		current.AuthMode = "chatgpt"
		current.OpenAIAPIKey = nil
		current.Tokens.IDToken = tokens.IDToken
		current.Tokens.AccessToken = tokens.AccessToken
		current.Tokens.RefreshToken = tokens.RefreshToken
		current.Tokens.AccountID = cloneString(accountID)
		current.LastRefresh = lastRefresh
		return nil
	})
}

func validateLogin(tokens oauth.TokenSet, now time.Time) (*string, time.Time, error) {
	if strings.TrimSpace(tokens.IDToken) == "" {
		return nil, time.Time{}, errors.New("save login: id_token is required")
	}
	idClaims, err := ParseJWTClaims(tokens.IDToken)
	if err != nil {
		return nil, time.Time{}, errors.New("save login: id_token is invalid")
	}

	if strings.TrimSpace(tokens.AccessToken) == "" {
		return nil, time.Time{}, errors.New("save login: access_token is required")
	}
	accessClaims, err := ParseJWTClaims(tokens.AccessToken)
	if err != nil {
		return nil, time.Time{}, errors.New("save login: access_token is invalid")
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return nil, time.Time{}, errors.New("save login: refresh_token is required")
	}

	accountID := firstNonEmpty(tokens.AccountID, idClaims.ChatGPTAccountID, accessClaims.ChatGPTAccountID)
	if accountID != "" && strings.TrimSpace(accountID) == "" {
		return nil, time.Time{}, errors.New("save login: account_id is invalid")
	}

	lastRefresh := now.UTC()
	if lastRefresh.IsZero() {
		return nil, time.Time{}, errors.New("save login: timestamp is required")
	}
	if _, err := lastRefresh.MarshalText(); err != nil {
		return nil, time.Time{}, errors.New("save login: timestamp is outside RFC3339 range")
	}

	if accountID == "" {
		return nil, lastRefresh, nil
	}
	return &accountID, lastRefresh, nil
}
