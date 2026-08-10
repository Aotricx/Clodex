package auth

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Aotricx/Clodex/internal/oauth"
)

var (
	errRefreshAborted    = errors.New("auth refresh aborted")
	errRefreshSuperseded = errors.New("auth refresh superseded by disk state")
)

// Credentials carries backend credentials without exposing fields to default
// formatting or JSON serialization.
type Credentials struct {
	accessToken string
	accountID   string
}

// AccessToken returns the bearer token for a concrete backend request.
func (c Credentials) AccessToken() string { return c.accessToken }

// AccountID returns the optional ChatGPT account/workspace identifier.
func (c Credentials) AccountID() string { return c.accountID }

// String intentionally never renders credential values.
func (Credentials) String() string {
	return "Credentials{access_token:<redacted>, account_id:<redacted>}"
}

// GoString intentionally never renders credential values for %#v formatting.
func (Credentials) GoString() string {
	return "Credentials{access_token:<redacted>, account_id:<redacted>}"
}

// Attempt performs one backend HTTP operation with current credentials. It
// must construct a fresh request each time because Do may invoke it twice.
type Attempt func(context.Context, Credentials) (*http.Response, error)

// Coordinator cooperates with Codex CLI auth state and one concrete OAuth
// authority. Its zero value is invalid until Store is assigned.
type Coordinator struct {
	Store *Store
	OAuth *oauth.Client
	Now   func() time.Time
}

type refreshSnapshot struct {
	idToken      string
	accessToken  string
	refreshToken string
	lastRefresh  time.Time
	accountID    string
	hasAccountID bool
}

type refreshFlight struct {
	done        chan struct{}
	credentials Credentials
	err         error
}

var processRefreshFlights = struct {
	sync.Mutex
	byPath map[string]*refreshFlight
}{byPath: make(map[string]*refreshFlight)}

// Ensure rereads disk and proactively refreshes only when Codex expiry/age
// rules say refresh is due.
func (c *Coordinator) Ensure(ctx context.Context) (Credentials, error) {
	if err := contextError(ctx); err != nil {
		return Credentials{}, err
	}
	if err := c.validateStore(); err != nil {
		return Credentials{}, err
	}
	current, err := c.Store.Read()
	if err != nil {
		return Credentials{}, err
	}
	due, err := current.RefreshDue(c.now())
	if err != nil {
		return Credentials{}, err
	}
	if !due {
		return credentialsFromFile(current)
	}
	return c.coalesce(ctx, func() (Credentials, error) {
		return c.refreshIf(ctx, func(file *File) (bool, error) {
			return file.RefreshDue(c.now())
		})
	})
}

// Force refreshes after a failed bearer token. Disk is reread first; when
// another process already rotated access state, its winner is returned without
// contacting the authority.
func (c *Coordinator) Force(ctx context.Context, failedAccessToken string) (Credentials, error) {
	if err := contextError(ctx); err != nil {
		return Credentials{}, err
	}
	if err := c.validateStore(); err != nil {
		return Credentials{}, err
	}
	return c.coalesce(ctx, func() (Credentials, error) {
		return c.refreshIf(ctx, func(file *File) (bool, error) {
			if failedAccessToken != "" && file.Tokens.AccessToken != failedAccessToken {
				return false, nil
			}
			return true, nil
		})
	})
}

// Recover401 is the explicit unauthorized-recovery spelling of Force.
func (c *Coordinator) Recover401(ctx context.Context, failedAccessToken string) (Credentials, error) {
	return c.Force(ctx, failedAccessToken)
}

// Do performs one attempt, closes a first 401 body, forces recovery, then
// performs exactly one retry. A second 401 is returned to the caller unchanged.
// Network errors and non-401 responses are never retried here.
func (c *Coordinator) Do(ctx context.Context, attempt Attempt) (*http.Response, error) {
	if attempt == nil {
		return nil, errors.New("auth operation attempt is nil")
	}
	credentials, err := c.Ensure(ctx)
	if err != nil {
		return nil, err
	}
	response, err := attempt(ctx, credentials)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("auth operation returned nil response")
	}
	if response.StatusCode != http.StatusUnauthorized {
		return response, nil
	}
	if response.Body != nil {
		_ = response.Body.Close()
	}

	credentials, err = c.Recover401(ctx, credentials.AccessToken())
	if err != nil {
		return nil, err
	}
	response, err = attempt(ctx, credentials)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return nil, errors.New("auth operation retry returned nil response")
	}
	return response, nil
}

func (c *Coordinator) refreshIf(
	ctx context.Context,
	shouldRefresh func(*File) (bool, error),
) (Credentials, error) {
	if err := contextError(ctx); err != nil {
		return Credentials{}, err
	}
	base, err := c.Store.Read()
	if err != nil {
		return Credentials{}, err
	}
	due, err := shouldRefresh(base)
	if err != nil {
		return Credentials{}, err
	}
	if !due {
		return credentialsFromFile(base)
	}
	if err := c.validateOAuth(); err != nil {
		return Credentials{}, err
	}
	if err := contextError(ctx); err != nil {
		return Credentials{}, err
	}

	snapshot := snapshotRefreshFields(base)
	refreshed, err := c.OAuth.Refresh(ctx, base.Tokens.RefreshToken)
	if err != nil {
		return Credentials{}, err
	}
	updated, err := c.Store.UpdateAtomically(func(current *File) error {
		if !sameRefreshSnapshot(current, snapshot) {
			return errRefreshSuperseded
		}
		current.Tokens.AccessToken = refreshed.AccessToken
		if refreshed.IDToken != "" {
			current.Tokens.IDToken = refreshed.IDToken
		}
		if refreshed.RefreshToken != "" {
			current.Tokens.RefreshToken = refreshed.RefreshToken
		}
		if refreshed.AccountID != "" {
			accountID := refreshed.AccountID
			current.Tokens.AccountID = &accountID
		}
		current.LastRefresh = c.now().UTC()
		return nil
	})
	if errors.Is(err, errRefreshSuperseded) {
		winner, readErr := c.Store.Read()
		if readErr != nil {
			return Credentials{}, readErr
		}
		return credentialsFromFile(winner)
	}
	if err != nil {
		return Credentials{}, err
	}
	return credentialsFromFile(updated)
}

func (c *Coordinator) coalesce(
	ctx context.Context,
	operation func() (Credentials, error),
) (Credentials, error) {
	key := coalescingPath(c.Store.Path)
	for {
		if err := contextError(ctx); err != nil {
			return Credentials{}, err
		}
		processRefreshFlights.Lock()
		existing := processRefreshFlights.byPath[key]
		if existing == nil {
			break
		}
		processRefreshFlights.Unlock()
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-existing.done:
			if errors.Is(existing.err, context.Canceled) || errors.Is(existing.err, context.DeadlineExceeded) {
				continue
			}
			return existing.credentials, existing.err
		}
	}
	flight := &refreshFlight{done: make(chan struct{}), err: errRefreshAborted}
	processRefreshFlights.byPath[key] = flight
	processRefreshFlights.Unlock()
	defer func() {
		processRefreshFlights.Lock()
		if processRefreshFlights.byPath[key] == flight {
			delete(processRefreshFlights.byPath, key)
		}
		close(flight.done)
		processRefreshFlights.Unlock()
	}()

	flight.credentials, flight.err = operation()
	return flight.credentials, flight.err
}

func credentialsFromFile(file *File) (Credentials, error) {
	if file == nil {
		return Credentials{}, errors.New("derive credentials: nil auth file")
	}
	accountID := ""
	if file.Tokens.AccountID != nil {
		accountID = *file.Tokens.AccountID
	}
	if accountID == "" {
		idClaims, err := ParseJWTClaims(file.Tokens.IDToken)
		if err != nil {
			return Credentials{}, errors.New("derive credentials: invalid id_token")
		}
		accountID = idClaims.ChatGPTAccountID
		if accountID == "" {
			accessClaims, err := ParseJWTClaims(file.Tokens.AccessToken)
			if err != nil {
				return Credentials{}, errors.New("derive credentials: invalid access_token")
			}
			accountID = accessClaims.ChatGPTAccountID
		}
	}
	return Credentials{accessToken: file.Tokens.AccessToken, accountID: accountID}, nil
}

func snapshotRefreshFields(file *File) refreshSnapshot {
	snapshot := refreshSnapshot{
		idToken:      file.Tokens.IDToken,
		accessToken:  file.Tokens.AccessToken,
		refreshToken: file.Tokens.RefreshToken,
		lastRefresh:  file.LastRefresh,
	}
	if file.Tokens.AccountID != nil {
		snapshot.accountID = *file.Tokens.AccountID
		snapshot.hasAccountID = true
	}
	return snapshot
}

func sameRefreshSnapshot(file *File, snapshot refreshSnapshot) bool {
	if file.Tokens.IDToken != snapshot.idToken ||
		file.Tokens.AccessToken != snapshot.accessToken ||
		file.Tokens.RefreshToken != snapshot.refreshToken ||
		!file.LastRefresh.Equal(snapshot.lastRefresh) {
		return false
	}
	if file.Tokens.AccountID == nil {
		return !snapshot.hasAccountID
	}
	return snapshot.hasAccountID && *file.Tokens.AccountID == snapshot.accountID
}

func (c *Coordinator) validateStore() error {
	if c == nil {
		return errors.New("auth coordinator is nil")
	}
	if c.Store == nil {
		return errors.New("auth coordinator Store is nil")
	}
	return nil
}

func (c *Coordinator) validateOAuth() error {
	if c.OAuth == nil {
		return errors.New("auth coordinator OAuth client is nil")
	}
	return nil
}

func (c *Coordinator) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("auth context is nil")
	}
	return ctx.Err()
}

func coalescingPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}
