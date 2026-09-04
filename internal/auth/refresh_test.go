package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/oauth"
)

func TestCoordinatorEnsureRefreshBoundaries(t *testing.T) {
	t.Parallel()

	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		name          string
		accessPayload map[string]any
		lastRefresh   time.Time
		wantRefresh   bool
	}{
		{"five minute access skew inclusive", map[string]any{"exp": now.Add(5 * time.Minute).Unix()}, now, true},
		{"outside access skew", map[string]any{"exp": now.Add(5*time.Minute + time.Second).Unix()}, now.Add(-9 * 24 * time.Hour), false},
		{"eight day age exact", map[string]any{"sub": "access"}, now.Add(-8 * 24 * time.Hour), false},
		{"older than eight days", map[string]any{"sub": "access"}, now.Add(-8*24*time.Hour - time.Second), true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, initial := writeRefreshAuth(t, now, tt.lastRefresh)
			initial.Tokens.AccessToken = syntheticJWT(t, tt.accessPayload)
			writeFileValue(t, path, initial)
			oldAccess := initial.Tokens.AccessToken
			newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "generation": "new"})
			var calls atomic.Int64
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeOAuthTokens(t, w, oauth.TokenSet{AccessToken: newAccess})
			}))
			defer issuer.Close()

			coordinator := &Coordinator{
				Store: &Store{Path: path},
				OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()},
				Now:   func() time.Time { return now },
			}
			credentials, err := coordinator.Ensure(context.Background())
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			if got := calls.Load(); got != boolCount(tt.wantRefresh) {
				t.Fatalf("authority calls = %d, want %d", got, boolCount(tt.wantRefresh))
			}
			wantAccess := oldAccess
			if tt.wantRefresh {
				wantAccess = newAccess
			}
			if credentials.AccessToken() != wantAccess {
				t.Fatal("Ensure() returned wrong access token")
			}
		})
	}
}

func TestCoordinatorRefreshExactRequestAndOptionalRotation(t *testing.T) {
	t.Parallel()

	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		name         string
		rotatesID    bool
		rotatesToken bool
		wantAccount  string
	}{
		{name: "omitted optional rotations are preserved"},
		{name: "present rotations replace old values", rotatesID: true, rotatesToken: true, wantAccount: "account-new"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, initial := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
			oldID, oldRefresh := initial.Tokens.IDToken, initial.Tokens.RefreshToken
			oldAccount := *initial.Tokens.AccountID
			initial.raw = map[string]json.RawMessage{"future_top": json.RawMessage(`{"keep":true}`)}
			initial.Tokens.raw = map[string]json.RawMessage{"future_token": json.RawMessage(`[1,2,3]`)}
			writeFileValue(t, path, initial)

			newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix()})
			response := oauth.TokenSet{AccessToken: newAccess}
			if tt.rotatesID {
				response.IDToken = syntheticJWT(t, map[string]any{
					"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "account-new"},
				})
			}
			if tt.rotatesToken {
				response.RefreshToken = "refresh-new"
				response.AccountID = "account-new"
			}

			var requestBody map[string]string
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/oauth/token" {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if got := r.Header.Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q", got)
				}
				if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
					t.Error(err)
				}
				writeOAuthTokens(t, w, response)
			}))
			defer issuer.Close()

			coordinator := &Coordinator{
				Store: &Store{Path: path},
				OAuth: &oauth.Client{Issuer: issuer.URL, ClientID: "client", HTTPClient: issuer.Client()},
				Now:   func() time.Time { return now },
			}
			if _, err := coordinator.Ensure(context.Background()); err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			wantRequest := map[string]string{
				"client_id": "client", "grant_type": "refresh_token", "refresh_token": oldRefresh,
			}
			if !reflect.DeepEqual(requestBody, wantRequest) {
				t.Fatalf("refresh JSON = %#v, want %#v", requestBody, wantRequest)
			}

			got, err := coordinator.Store.Read()
			if err != nil {
				t.Fatal(err)
			}
			wantID, wantRefresh, wantAccount := oldID, oldRefresh, oldAccount
			if tt.rotatesID {
				wantID = response.IDToken
			}
			if tt.rotatesToken {
				wantRefresh = response.RefreshToken
				wantAccount = response.AccountID
			}
			if got.Tokens.IDToken != wantID || got.Tokens.AccessToken != newAccess ||
				got.Tokens.RefreshToken != wantRefresh || got.Tokens.AccountID == nil || *got.Tokens.AccountID != wantAccount {
				t.Fatalf("persisted tokens do not match optional rotation semantics: %#v", got.Tokens)
			}
			if !got.LastRefresh.Equal(now) {
				t.Fatalf("LastRefresh = %v, want %v", got.LastRefresh, now)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range []string{`"future_top":{"keep":true}`, `"future_token":[1,2,3]`} {
				if !bytes.Contains(encoded, []byte(fragment)) {
					t.Fatalf("unknown JSON lost after refresh: %s", encoded)
				}
			}
		})
	}
}

func TestCoordinatorFailedRefreshLeavesFileIdenticalAndPassesOAuthError(t *testing.T) {
	t.Parallel()

	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{"permanent unauthorized", http.StatusUnauthorized, `{"error":{"code":"refresh_token_invalidated","refresh_token":"refresh-token-secret-marker"}}`},
		{"transient server error", http.StatusServiceUnavailable, `{"error":{"message":"retry","refresh_token":"refresh-token-secret-marker"}}`},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, _ := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer issuer.Close()
			coordinator := &Coordinator{
				Store: &Store{Path: path},
				OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()},
				Now:   func() time.Time { return now },
			}

			_, err = coordinator.Ensure(context.Background())
			var httpErr *oauth.HTTPError
			if !errors.As(err, &httpErr) || httpErr.StatusCode != tt.statusCode {
				t.Fatalf("Ensure() error = %#v, want OAuth HTTP %d", err, tt.statusCode)
			}
			if strings.Contains(err.Error(), "refresh-token-secret-marker") {
				t.Fatalf("error leaked refresh token: %v", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("auth file changed on failed refresh\n got: %s\nwant: %s", after, before)
			}
		})
	}
}

func TestCoordinatorOAuthTransportErrorIsUnchangedAndDoesNotWrite(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, _ := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("transport unavailable")
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, sentinel
	})}
	coordinator := &Coordinator{
		Store: &Store{Path: path},
		OAuth: &oauth.Client{Issuer: "https://issuer.invalid", HTTPClient: httpClient},
		Now:   func() time.Time { return now },
	}
	_, err = coordinator.Ensure(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("Ensure() error = %v, want wrapped transport sentinel", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("auth file changed on transport failure")
	}
}

func TestCoordinatorRecover401UsesDiskRotationWithoutAuthority(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, failed := writeRefreshAuth(t, now, now)
	failedAccess := failed.Tokens.AccessToken
	winner := failed.clone()
	winner.Tokens.AccessToken = syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "winner": true})
	winner.Tokens.RefreshToken = "winner-refresh"
	winner.LastRefresh = now.Add(time.Second)
	writeFileValue(t, path, winner)

	coordinator := &Coordinator{Store: &Store{Path: path}}

	credentials, err := coordinator.Recover401(context.Background(), failedAccess)
	if err != nil {
		t.Fatalf("Recover401() error = %v", err)
	}
	if credentials.AccessToken() != winner.Tokens.AccessToken {
		t.Fatal("Recover401() did not return disk winner")
	}
}

func TestCoordinatorNeverOverwritesAnotherProcessWinner(t *testing.T) {
	t.Parallel()

	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(testing.TB, *File)
	}{
		{"id token changed", func(tb testing.TB, f *File) {
			f.Tokens.IDToken = syntheticJWT(tb, map[string]any{"sub": "winner-id"})
		}},
		{"access token changed", func(tb testing.TB, f *File) {
			f.Tokens.AccessToken = syntheticJWT(tb, map[string]any{"exp": now.Add(time.Hour).Unix(), "winner": "access"})
		}},
		{"refresh token changed", func(_ testing.TB, f *File) { f.Tokens.RefreshToken = "winner-refresh" }},
		{"last refresh changed", func(_ testing.TB, f *File) { f.LastRefresh = now.Add(time.Minute) }},
		{"account changed", func(_ testing.TB, f *File) { account := "winner-account"; f.Tokens.AccountID = &account }},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path, initial := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
			winner := initial.clone()
			tt.mutate(t, winner)
			winner.LastRefresh = winner.LastRefresh.UTC()
			var winnerBytes []byte
			store := &Store{Path: path}
			store.beforeRename = func(_, targetPath string) error {
				encoded, err := json.Marshal(winner)
				if err != nil {
					return err
				}
				winnerBytes = bytes.Clone(encoded)
				return os.WriteFile(targetPath, encoded, 0o600)
			}

			var calls atomic.Int64
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeOAuthTokens(t, w, oauth.TokenSet{
					IDToken:      syntheticJWT(t, map[string]any{"sub": "authority"}),
					AccessToken:  syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "authority": true}),
					RefreshToken: "authority-refresh",
					AccountID:    "authority-account",
				})
			}))
			defer issuer.Close()
			coordinator := &Coordinator{
				Store: store,
				OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()},
				Now:   func() time.Time { return now },
			}

			credentials, err := coordinator.Ensure(context.Background())
			if err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			if calls.Load() != 1 {
				t.Fatalf("authority calls = %d, want 1", calls.Load())
			}
			if credentials.AccessToken() != winner.Tokens.AccessToken {
				t.Fatal("Ensure() did not return another-process winner")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, winnerBytes) {
				t.Fatalf("winner overwritten\n got: %s\nwant: %s", after, winnerBytes)
			}
		})
	}
}

func TestCoordinatorConcurrentCallersCoalesceAcrossInstances(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, _ := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
	newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "generation": "coalesced"})
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	var calls atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		writeOAuthTokens(t, w, oauth.TokenSet{AccessToken: newAccess})
	}))
	defer issuer.Close()

	const callers = 24
	start := make(chan struct{})
	results := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			coordinator := &Coordinator{
				Store: &Store{Path: path},
				OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()},
				Now:   func() time.Time { return now },
			}
			credentials, err := coordinator.Ensure(context.Background())
			if err == nil && credentials.AccessToken() != newAccess {
				err = errors.New("coalesced caller received wrong access token")
			}
			results <- err
		}()
	}
	close(start)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("authority refresh did not start")
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("authority calls = %d, want 1", calls.Load())
	}
}

func TestCoordinatorWaitingCallerHonorsContext(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, _ := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
	newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix()})
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		writeOAuthTokens(t, w, oauth.TokenSet{AccessToken: newAccess})
	}))
	defer issuer.Close()
	leader := &Coordinator{Store: &Store{Path: path}, OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}, Now: func() time.Time { return now }}
	leaderResult := make(chan error, 1)
	go func() {
		_, err := leader.Ensure(context.Background())
		leaderResult <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("leader refresh did not start")
	}

	waiter := &Coordinator{Store: &Store{Path: path}, OAuth: leader.OAuth, Now: leader.Now}
	ctx := newObservedCancelContext(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := waiter.Ensure(ctx)
		waiterResult <- err
	}()
	<-ctx.doneObserved
	ctx.cancel()
	err := <-waiterResult
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting Ensure() error = %v, want context.Canceled", err)
	}
	close(release)
	if err := <-leaderResult; err != nil {
		t.Fatalf("leader Ensure() error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("authority calls = %d, want 1", calls.Load())
	}
}

func TestCoordinatorCanceledRefreshLeaderDoesNotPoisonHealthyFollower(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, _ := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
	newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "generation": "healthy-follower"})
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var calls atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst
			return
		}
		writeOAuthTokens(t, w, oauth.TokenSet{AccessToken: newAccess})
	}))
	defer issuer.Close()
	defer close(releaseFirst)

	coordinator := &Coordinator{
		Store: &Store{Path: path},
		OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()},
		Now:   func() time.Time { return now },
	}
	leaderContext, cancelLeader := context.WithCancel(context.Background())
	leaderResult := make(chan error, 1)
	go func() {
		_, err := coordinator.Ensure(leaderContext)
		leaderResult <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("leader refresh did not reach authority")
	}

	followerContext := newObservedCancelContext(context.Background())
	t.Cleanup(followerContext.cancel)
	type result struct {
		credentials Credentials
		err         error
	}
	followerResult := make(chan result, 1)
	go func() {
		credentials, err := coordinator.Ensure(followerContext)
		followerResult <- result{credentials: credentials, err: err}
	}()
	select {
	case <-followerContext.doneObserved:
	case <-time.After(3 * time.Second):
		t.Fatal("follower did not join refresh flight")
	}

	cancelLeader()
	select {
	case err := <-leaderResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader Ensure() error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled leader did not return")
	}

	select {
	case got := <-followerResult:
		if got.err != nil {
			t.Fatalf("healthy follower Ensure() error = %v", got.err)
		}
		if got.credentials.AccessToken() != newAccess {
			t.Fatal("healthy follower did not receive refreshed access token")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("healthy follower did not recover from canceled leader")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("authority calls = %d, want canceled leader plus one follower retry", got)
	}
}

func TestCoordinatorCoalesceCleansUpAfterLeaderPanic(t *testing.T) {
	t.Parallel()

	coordinator := &Coordinator{Store: &Store{Path: filepath.Join(t.TempDir(), "auth.json")}}
	key := coalescingPath(coordinator.Store.Path)
	defer deleteRefreshFlightForTest(key)

	operationStarted := make(chan struct{})
	panicLeader := make(chan struct{})
	leaderRecovered := make(chan any, 1)
	go func() {
		defer func() { leaderRecovered <- recover() }()
		_, _ = coordinator.coalesce(context.Background(), func() (Credentials, error) {
			close(operationStarted)
			<-panicLeader
			panic("synthetic refresh panic")
		})
	}()
	<-operationStarted

	waiterContext := newObservedCancelContext(context.Background())
	waiterResult := make(chan error, 1)
	go func() {
		_, err := coordinator.coalesce(waiterContext, func() (Credentials, error) {
			return Credentials{}, errors.New("waiter unexpectedly became leader")
		})
		waiterResult <- err
	}()
	<-waiterContext.doneObserved
	close(panicLeader)
	if recovered := <-leaderRecovered; recovered != "synthetic refresh panic" {
		t.Fatalf("leader recovered = %#v", recovered)
	}

	processRefreshFlights.Lock()
	_, staleFlight := processRefreshFlights.byPath[key]
	processRefreshFlights.Unlock()
	if staleFlight {
		waiterContext.cancel()
	}
	waiterErr := <-waiterResult
	if waiterErr == nil || waiterErr.Error() != "auth refresh aborted" {
		t.Fatalf("waiter error = %v, want auth refresh aborted", waiterErr)
	}

	nextContext := newObservedCancelContext(context.Background())
	nextResult := make(chan error, 1)
	go func() {
		_, err := coordinator.coalesce(nextContext, func() (Credentials, error) {
			return Credentials{}, nil
		})
		nextResult <- err
	}()
	select {
	case err := <-nextResult:
		if err != nil {
			t.Fatalf("next coalesce error = %v", err)
		}
	case <-nextContext.doneObserved:
		nextContext.cancel()
		if err := <-nextResult; err != nil {
			t.Fatalf("next coalesce joined stale flight: %v", err)
		}
	}
}

func TestCoordinatorCredentialsFormattingNeverLeaks(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, file := writeRefreshAuth(t, now, now)
	file.Tokens.AccountID = nil
	file.Tokens.IDToken = syntheticJWT(t, map[string]any{
		"email":                       "never-show@example.test",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "claim-account-secret"},
	})
	writeFileValue(t, path, file)
	coordinator := &Coordinator{Store: &Store{Path: path}}
	credentials, err := coordinator.Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if credentials.AccountID() != "claim-account-secret" {
		t.Fatalf("AccountID() = %q", credentials.AccountID())
	}
	formatted := fmt.Sprintf("%v %+v %#v", credentials, credentials, credentials)
	for _, secret := range []string{file.Tokens.AccessToken, "claim-account-secret", "never-show@example.test"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("formatted Credentials leaked %q: %s", secret, formatted)
		}
	}
	if !strings.Contains(formatted, "<redacted>") {
		t.Fatalf("formatted Credentials missing redaction marker: %s", formatted)
	}
}

func TestCoordinatorDoRetriesOne401AfterClosingBody(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, initial := writeRefreshAuth(t, now, now)
	oldAccess := initial.Tokens.AccessToken
	newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "generation": "retry"})
	var authorityCalls atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		authorityCalls.Add(1)
		writeOAuthTokens(t, w, oauth.TokenSet{AccessToken: newAccess})
	}))
	defer issuer.Close()
	coordinator := &Coordinator{
		Store: &Store{Path: path},
		OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()},
		Now:   func() time.Time { return now },
	}

	firstBody := &trackingBody{}
	secondBody := &trackingBody{}
	var attempts atomic.Int64
	var tokens []string
	var tokensMu sync.Mutex
	response, err := coordinator.Do(context.Background(), func(_ context.Context, credentials Credentials) (*http.Response, error) {
		call := attempts.Add(1)
		tokensMu.Lock()
		tokens = append(tokens, credentials.AccessToken())
		tokensMu.Unlock()
		body := io.ReadCloser(firstBody)
		if call == 2 {
			body = secondBody
		}
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: body}, nil
	})
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if attempts.Load() != 2 || authorityCalls.Load() != 1 {
		t.Fatalf("attempts = %d, authority calls = %d; want 2, 1", attempts.Load(), authorityCalls.Load())
	}
	if !firstBody.closed.Load() {
		t.Fatal("first 401 body was not closed before retry")
	}
	if secondBody.closed.Load() {
		t.Fatal("surfaced second 401 body was closed")
	}
	if !reflect.DeepEqual(tokens, []string{oldAccess, newAccess}) {
		t.Fatal("Do() did not retry with rotated credential")
	}
}

func TestCoordinatorDoDoesNotRetryNetworkOrNon401Response(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path, _ := writeRefreshAuth(t, now, now)
	var authorityCalls atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		authorityCalls.Add(1)
		writeOAuthTokens(t, w, oauth.TokenSet{})
	}))
	defer issuer.Close()
	coordinator := &Coordinator{Store: &Store{Path: path}, OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()}, Now: func() time.Time { return now }}

	t.Run("network error", func(t *testing.T) {
		sentinel := errors.New("upstream network failure")
		var attempts atomic.Int64
		response, err := coordinator.Do(context.Background(), func(context.Context, Credentials) (*http.Response, error) {
			attempts.Add(1)
			return nil, sentinel
		})
		if response != nil || !errors.Is(err, sentinel) || attempts.Load() != 1 {
			t.Fatalf("Do() = %#v, %v; attempts %d", response, err, attempts.Load())
		}
	})

	t.Run("non 401", func(t *testing.T) {
		var attempts atomic.Int64
		body := &trackingBody{}
		response, err := coordinator.Do(context.Background(), func(context.Context, Credentials) (*http.Response, error) {
			attempts.Add(1)
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: body}, nil
		})
		if err != nil || response == nil || response.StatusCode != http.StatusServiceUnavailable || attempts.Load() != 1 {
			t.Fatalf("Do() = %#v, %v; attempts %d", response, err, attempts.Load())
		}
		defer response.Body.Close()
		if body.closed.Load() {
			t.Fatal("surfaced non-401 response body was closed")
		}
	})
	if authorityCalls.Load() != 0 {
		t.Fatalf("authority calls = %d, want 0", authorityCalls.Load())
	}
}

func TestCoordinatorRejectsInvalidConfigurationAndHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Coordinator{}).Ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure(canceled) error = %v", err)
	}
	if _, err := (&Coordinator{}).Ensure(context.Background()); err == nil {
		t.Fatal("Ensure() with nil Store error = nil")
	}
	coordinator := &Coordinator{Store: &Store{Path: filepath.Join(t.TempDir(), "auth.json")}}
	if _, err := coordinator.Force(context.Background(), "failed"); err == nil {
		t.Fatal("Force() with nil OAuth error = nil")
	}
	if _, err := coordinator.Do(context.Background(), nil); err == nil {
		t.Fatal("Do() with nil attempt error = nil")
	}
}

func TestCoordinatorEnsureSkipsRefreshWhenDiskRotatedUnderLock(t *testing.T) {
	t.Parallel()

	now := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	path, initial := writeRefreshAuth(t, now, now.Add(-9*24*time.Hour))
	winner := initial.clone()
	winner.Tokens.AccessToken = syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "winner": true})
	winner.LastRefresh = now

	held, err := lockAuth(context.Background(), path)
	if err != nil {
		t.Fatalf("hold auth lock: %v", err)
	}
	locked := true
	defer func() {
		if locked {
			_ = held.Unlock()
		}
	}()

	var calls atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeOAuthTokens(t, w, oauth.TokenSet{
			AccessToken: syntheticJWT(t, map[string]any{"exp": now.Add(time.Hour).Unix(), "authority": true}),
		})
	}))
	defer issuer.Close()

	coordinator := &Coordinator{
		Store: &Store{Path: path},
		OAuth: &oauth.Client{Issuer: issuer.URL, HTTPClient: issuer.Client()},
		Now:   func() time.Time { return now },
	}

	type outcome struct {
		credentials Credentials
		err         error
	}
	done := make(chan outcome, 1)
	go func() {
		credentials, err := coordinator.Ensure(context.Background())
		done <- outcome{credentials: credentials, err: err}
	}()

	select {
	case got := <-done:
		t.Fatalf("Ensure() completed while lock held: %v", got.err)
	case <-time.After(200 * time.Millisecond):
	}
	if calls.Load() != 0 {
		t.Fatalf("authority calls = %d while lock held, want 0", calls.Load())
	}

	writeFileValue(t, path, winner)
	if err := held.Unlock(); err != nil {
		t.Fatalf("release auth lock: %v", err)
	}
	locked = false

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Ensure() error = %v", got.err)
		}
		if got.credentials.AccessToken() != winner.Tokens.AccessToken {
			t.Fatal("Ensure() did not return disk winner after lock")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Ensure() remained blocked after lock release")
	}
	if calls.Load() != 0 {
		t.Fatalf("authority calls = %d, want 0", calls.Load())
	}

	info, err := os.Stat(path + ".lock")
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("lock file mode = %04o, want 0600", got)
		}
	}
}

func writeRefreshAuth(t testing.TB, now, lastRefresh time.Time) (string, *File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	file := validAuthFile(t, now.Add(time.Hour), lastRefresh)
	if lastRefresh.Before(now.Add(-8 * 24 * time.Hour)) {
		file.Tokens.AccessToken = syntheticJWT(t, map[string]any{"sub": "age-based-refresh"})
	}
	writeFileValue(t, path, file)
	return path, file
}

func writeOAuthTokens(t testing.TB, w http.ResponseWriter, tokens oauth.TokenSet) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(tokens); err != nil {
		t.Errorf("encode OAuth response: %v", err)
	}
}

func boolCount(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

type trackingBody struct {
	closed atomic.Bool
}

func (*trackingBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *trackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type observedCancelContext struct {
	parent       context.Context
	done         chan struct{}
	doneObserved chan struct{}
	observeOnce  sync.Once
	cancelOnce   sync.Once
	canceled     atomic.Bool
}

func newObservedCancelContext(parent context.Context) *observedCancelContext {
	return &observedCancelContext{
		parent:       parent,
		done:         make(chan struct{}),
		doneObserved: make(chan struct{}),
	}
}

func (c *observedCancelContext) Deadline() (time.Time, bool) { return c.parent.Deadline() }
func (c *observedCancelContext) Done() <-chan struct{} {
	c.observeOnce.Do(func() { close(c.doneObserved) })
	return c.done
}
func (c *observedCancelContext) Err() error {
	if c.canceled.Load() {
		return context.Canceled
	}
	return nil
}
func (c *observedCancelContext) Value(key any) any { return c.parent.Value(key) }
func (c *observedCancelContext) cancel() {
	c.cancelOnce.Do(func() {
		c.canceled.Store(true)
		close(c.done)
	})
}

func deleteRefreshFlightForTest(key string) {
	processRefreshFlights.Lock()
	delete(processRefreshFlights.byPath, key)
	processRefreshFlights.Unlock()
}
