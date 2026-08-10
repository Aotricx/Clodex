// Package commandauth implements Clodex's user-facing OAuth commands.
package commandauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/oauth"
	"github.com/Aotricx/Clodex/internal/redact"
)

// StatusFormat selects auth status output encoding.
type StatusFormat string

const (
	StatusText StatusFormat = "text"
	StatusJSON StatusFormat = "json"
)

// OAuthClient is the login subset used by auth commands.
type OAuthClient interface {
	Login(context.Context, oauth.Opener) (oauth.TokenSet, error)
	StartDevice(context.Context) (oauth.DeviceCode, error)
	PollDevice(context.Context, oauth.DeviceCode) (oauth.TokenSet, error)
}

var _ OAuthClient = (*oauth.Client)(nil)

// Service runs auth commands with explicitly injected dependencies.
type Service struct {
	OAuth  OAuthClient
	Store  *auth.Store
	Now    func() time.Time
	Writer io.Writer
	Opener oauth.Opener
}

// DefaultAuthPath returns Codex CLI's cross-platform ChatGPT OAuth path.
func DefaultAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return auth.CodexAuthPath(home)
}

// BrowserLogin completes browser PKCE login and persists Codex-compatible auth.
func (s *Service) BrowserLogin(ctx context.Context) (auth.Summary, error) {
	if err := s.validateLogin(ctx); err != nil {
		return auth.Summary{}, err
	}
	tokens, err := s.OAuth.Login(ctx, s.Opener)
	if err != nil {
		return auth.Summary{}, safeWrap("browser login", err)
	}
	file, err := s.Store.SaveLogin(tokens, s.now()())
	if err != nil {
		return auth.Summary{}, safeWrap("persist browser login", err)
	}
	summary, err := file.Summary()
	if err != nil {
		return auth.Summary{}, safeWrap("summarize browser login", err)
	}
	return summary, nil
}

// DeviceLogin completes headless device-code login and persists Codex auth.
func (s *Service) DeviceLogin(ctx context.Context) (auth.Summary, error) {
	if err := s.validateLogin(ctx); err != nil {
		return auth.Summary{}, err
	}
	if s.Writer == nil {
		return auth.Summary{}, errors.New("auth output writer is nil")
	}
	device, err := s.OAuth.StartDevice(ctx)
	if err != nil {
		return auth.Summary{}, safeWrap("start device login", err)
	}
	if _, err := fmt.Fprintf(s.Writer, "Open %s and enter code %s\n", device.VerificationURI, device.UserCode); err != nil {
		return auth.Summary{}, safeWrap("write device login instructions", err)
	}
	tokens, err := s.OAuth.PollDevice(ctx, device)
	if err != nil {
		return auth.Summary{}, safeWrap("poll device login", err)
	}
	file, err := s.Store.SaveLogin(tokens, s.now()())
	if err != nil {
		return auth.Summary{}, safeWrap("persist device login", err)
	}
	summary, err := file.Summary()
	if err != nil {
		return auth.Summary{}, safeWrap("summarize device login", err)
	}
	return summary, nil
}

// Status reads Codex auth and writes credential-free account status.
func (s *Service) Status(ctx context.Context, format StatusFormat) (auth.Summary, error) {
	if s == nil {
		return auth.Summary{}, errors.New("auth service is nil")
	}
	if ctx == nil {
		return auth.Summary{}, errors.New("auth context is nil")
	}
	if err := ctx.Err(); err != nil {
		return auth.Summary{}, err
	}
	if s.Store == nil {
		return auth.Summary{}, errors.New("auth store is nil")
	}
	if s.Writer == nil {
		return auth.Summary{}, errors.New("auth output writer is nil")
	}
	file, err := s.Store.Read()
	if err != nil {
		return auth.Summary{}, safeWrap("read auth status", err)
	}
	summary, err := file.Summary()
	if err != nil {
		return auth.Summary{}, safeWrap("summarize auth status", err)
	}

	switch format {
	case StatusJSON:
		if err := json.NewEncoder(s.Writer).Encode(summary); err != nil {
			return auth.Summary{}, safeWrap("write auth status JSON", err)
		}
	case StatusText:
		expiry := "unknown"
		if summary.Expiry != nil {
			expiry = summary.Expiry.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(s.Writer, "source=%s account=%s plan=%s expiry=%s\n", summary.Source, summary.Account, summary.Plan, expiry); err != nil {
			return auth.Summary{}, safeWrap("write auth status text", err)
		}
	default:
		return auth.Summary{}, errors.New("unsupported auth status format")
	}
	return summary, nil
}

func (s *Service) validateLogin(ctx context.Context) error {
	if s == nil {
		return errors.New("auth service is nil")
	}
	if ctx == nil {
		return errors.New("auth context is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.OAuth == nil {
		return errors.New("OAuth client is nil")
	}
	if s.Store == nil {
		return errors.New("auth store is nil")
	}
	return nil
}

func (s *Service) now() func() time.Time {
	if s.Now == nil {
		return time.Now
	}
	return s.Now
}

type sanitizedError struct {
	operation string
	cause     error
}

func (e *sanitizedError) Error() string {
	message := redact.Text(e.cause.Error())
	if message == "" {
		return e.operation
	}
	return e.operation + ": " + message
}

func (e *sanitizedError) Unwrap() error {
	return e.cause
}

func safeWrap(operation string, cause error) error {
	return &sanitizedError{operation: operation, cause: cause}
}
