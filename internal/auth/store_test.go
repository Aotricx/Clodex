package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCodexAuthPathUsesExplicitHome(t *testing.T) {
	t.Parallel()

	home := filepath.Join(t.TempDir(), "chosen-home")
	got, err := CodexAuthPath(home)
	if err != nil {
		t.Fatalf("CodexAuthPath() error = %v", err)
	}
	want := filepath.Join(home, ".codex", "auth.json")
	if got != want {
		t.Fatalf("CodexAuthPath() = %q, want %q", got, want)
	}
	if _, err := CodexAuthPath(""); err == nil {
		t.Fatal("CodexAuthPath(empty) error = nil")
	}
}

func TestResolveCodexAuthPathUsesCODEXHOMEDirectory(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", codexHome)

	got, err := ResolveCodexAuthPath(filepath.Join(t.TempDir(), "ignored-home"))
	if err != nil {
		t.Fatalf("ResolveCodexAuthPath() error = %v", err)
	}
	want := filepath.Join(codexHome, "auth.json")
	if got != want {
		t.Fatalf("ResolveCodexAuthPath() = %q, want %q", got, want)
	}
}

func TestResolveCodexAuthPathFallsBackToHomeWhenCODEXHOMEEmpty(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := filepath.Join(t.TempDir(), "chosen-home")

	got, err := ResolveCodexAuthPath(home)
	if err != nil {
		t.Fatalf("ResolveCodexAuthPath() error = %v", err)
	}
	want := filepath.Join(home, ".codex", "auth.json")
	if got != want {
		t.Fatalf("ResolveCodexAuthPath() = %q, want %q", got, want)
	}
	if _, err := ResolveCodexAuthPath(""); err == nil {
		t.Fatal("ResolveCodexAuthPath(empty home, empty CODEX_HOME) error = nil")
	}
}

func TestStoreReadAndMarshalPreserveExactShapeAndUnknownFields(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	expiry := time.Date(2032, 3, 4, 5, 6, 7, 0, time.UTC)
	idToken := syntheticJWT(t, map[string]any{
		"email": "roundtrip@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": "pro",
		},
	})
	accessToken := syntheticJWT(t, map[string]any{"exp": expiry.Unix()})
	input := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": "preserve-but-do-not-use",
		"tokens": map[string]any{
			"id_token":      idToken,
			"access_token":  accessToken,
			"refresh_token": "opaque-refresh-secret",
			"account_id":    "account-secret",
			"token_future":  map[string]any{"enabled": true, "count": 3},
		},
		"last_refresh": "2032-03-01T01:02:03Z",
		"top_future":   []any{"one", map[string]any{"two": 2}},
	}
	writeJSONFile(t, path, input)

	got, err := (&Store{Path: path}).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.AuthMode != "chatgpt" || got.OpenAIAPIKey == nil || *got.OpenAIAPIKey != "preserve-but-do-not-use" {
		t.Fatalf("known top-level fields = %#v", got)
	}
	if got.Tokens.IDToken != idToken || got.Tokens.AccessToken != accessToken || got.Tokens.RefreshToken != "opaque-refresh-secret" {
		t.Fatal("known token fields changed")
	}
	if got.Tokens.AccountID == nil || *got.Tokens.AccountID != "account-secret" {
		t.Fatalf("AccountID = %v", got.Tokens.AccountID)
	}
	if !got.LastRefresh.Equal(time.Date(2032, 3, 1, 1, 2, 3, 0, time.UTC)) {
		t.Fatalf("LastRefresh = %v", got.LastRefresh)
	}

	roundTrip, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var wantValue, gotValue any
	original, _ := json.Marshal(input)
	if err := json.Unmarshal(original, &wantValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(roundTrip, &gotValue); err != nil {
		t.Fatal(err)
	}
	if !jsonValuesEqual(wantValue, gotValue) {
		t.Fatalf("round trip changed JSON\n got: %s\nwant: %s", roundTrip, original)
	}
}

func TestStoreReadAcceptsSourceValidNullAccountAndAPIKey(t *testing.T) {
	t.Parallel()

	file := validAuthFile(t, time.Now().Add(time.Hour), time.Now())
	file.Tokens.AccountID = nil
	file.OpenAIAPIKey = nil
	path := filepath.Join(t.TempDir(), "auth.json")
	writeFileValue(t, path, file)

	got, err := (&Store{Path: path}).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Tokens.AccountID != nil || got.OpenAIAPIKey != nil {
		t.Fatalf("null fields did not survive: %#v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"OPENAI_API_KEY":null`, `"account_id":null`} {
		if !bytes.Contains(encoded, []byte(fragment)) {
			t.Fatalf("encoded auth missing %s: %s", fragment, encoded)
		}
	}
}

func TestStoreReadRejectsInvalidAuthWithoutLeakingSecrets(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	base := validAuthMap(t, now.Add(time.Hour), now)
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"missing mode", func(v map[string]any) { delete(v, "auth_mode") }},
		{"api key mode", func(v map[string]any) { v["auth_mode"] = "apikey" }},
		{"other mode", func(v map[string]any) { v["auth_mode"] = "headers" }},
		{"missing tokens", func(v map[string]any) { delete(v, "tokens") }},
		{"null tokens", func(v map[string]any) { v["tokens"] = nil }},
		{"missing id token", func(v map[string]any) { delete(v["tokens"].(map[string]any), "id_token") }},
		{"empty id token", func(v map[string]any) { v["tokens"].(map[string]any)["id_token"] = " " }},
		{"malformed id token", func(v map[string]any) { v["tokens"].(map[string]any)["id_token"] = "not-a-jwt" }},
		{"missing access token", func(v map[string]any) { delete(v["tokens"].(map[string]any), "access_token") }},
		{"malformed access token", func(v map[string]any) { v["tokens"].(map[string]any)["access_token"] = "bad.access.token" }},
		{"missing refresh token", func(v map[string]any) { delete(v["tokens"].(map[string]any), "refresh_token") }},
		{"empty refresh token", func(v map[string]any) { v["tokens"].(map[string]any)["refresh_token"] = "" }},
		{"empty optional account", func(v map[string]any) { v["tokens"].(map[string]any)["account_id"] = "" }},
		{"missing timestamp", func(v map[string]any) { delete(v, "last_refresh") }},
		{"null timestamp", func(v map[string]any) { v["last_refresh"] = nil }},
		{"malformed timestamp", func(v map[string]any) { v["last_refresh"] = "yesterday" }},
		{"non RFC3339 timestamp", func(v map[string]any) { v["last_refresh"] = "2030-01-02 03:04:05" }},
		{"wrong API key type", func(v map[string]any) { v["OPENAI_API_KEY"] = 7 }},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value := cloneJSONMap(t, base)
			tt.mutate(value)
			path := filepath.Join(t.TempDir(), "auth.json")
			writeJSONFile(t, path, value)
			_, err := (&Store{Path: path}).Read()
			if err == nil {
				t.Fatal("Read() error = nil")
			}
			for _, secret := range []string{
				"id-token-secret-marker", "access-token-secret-marker", "refresh-token-secret-marker",
				"account-secret-marker", "api-key-secret-marker", "user-secret@example.test",
			} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked secret %q: %v", secret, err)
				}
			}
		})
	}
}

func TestStoreReadRejectsMalformedJSONWithoutEchoingInput(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "auth.json")
	secret := "refresh-token-secret-never-echo"
	if err := os.WriteFile(path, []byte(`{"auth_mode":"chatgpt","tokens":{"refresh_token":"`+secret+`"`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (&Store{Path: path}).Read()
	if err == nil {
		t.Fatal("Read() error = nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked secret: %v", err)
	}
}

func TestStoreReadMissingFilePreservesNotExist(t *testing.T) {
	t.Parallel()

	_, err := (&Store{Path: filepath.Join(t.TempDir(), "missing.json")}).Read()
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Read() error = %v, want os.ErrNotExist", err)
	}
}

func TestStoreUpdateAtomicallyPreservesUnknownFields(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	value := validAuthMap(t, now.Add(time.Hour), now)
	value["future_top"] = map[string]any{"keep": true}
	value["tokens"].(map[string]any)["future_token"] = []any{1, 2, 3}
	writeJSONFile(t, path, value)
	store := &Store{Path: path}
	newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(2 * time.Hour).Unix(), "generation": 2})

	updated, err := store.UpdateAtomically(func(current *File) error {
		current.Tokens.AccessToken = newAccess
		current.LastRefresh = now.Add(time.Minute)
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateAtomically() error = %v", err)
	}
	if updated.Tokens.AccessToken != newAccess {
		t.Fatal("returned update missing access token")
	}
	disk, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if disk.Tokens.AccessToken != newAccess || disk.LastRefresh != now.Add(time.Minute) {
		t.Fatalf("disk update = %#v", disk)
	}
	encoded, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"future_top":{"keep":true}`, `"future_token":[1,2,3]`} {
		if !bytes.Contains(encoded, []byte(fragment)) {
			t.Fatalf("unknown field missing after update: %s", encoded)
		}
	}
}

func TestStoreUpdateCreatesSecureDirectoryAndFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes are not meaningful on Windows")
	}
	t.Parallel()

	root := t.TempDir()
	dir := filepath.Join(root, "new", ".codex")
	path := filepath.Join(dir, "auth.json")
	store := &Store{Path: path}
	want := validAuthFile(t, time.Now().Add(time.Hour), time.Now())
	if _, err := store.UpdateAtomically(func(current *File) error {
		*current = *want.clone()
		return nil
	}); err != nil {
		t.Fatalf("UpdateAtomically() error = %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %04o, want 0700", got)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %04o, want 0600", got)
	}
}

func TestStoreUpdateFailureNeverOverwritesAndCleansTemp(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name      string
		configure func(*Store)
		mutate    func(*File) error
	}{
		{
			name: "mutator failure",
			mutate: func(current *File) error {
				current.Tokens.RefreshToken = "replacement-secret"
				return errors.New("declined")
			},
		},
		{
			name: "validation failure",
			mutate: func(current *File) error {
				current.Tokens.AccessToken = "invalid"
				return nil
			},
		},
		{
			name: "serialization failure",
			mutate: func(current *File) error {
				current.raw["invalid_raw"] = json.RawMessage("{")
				return nil
			},
		},
		{
			name: "pre rename write failure",
			configure: func(store *Store) {
				store.beforeRename = func(tempPath, targetPath string) error {
					if filepath.Dir(tempPath) != filepath.Dir(targetPath) {
						return errors.New("temp file not in destination directory")
					}
					if _, err := os.Stat(tempPath); err != nil {
						return fmt.Errorf("stat temp: %w", err)
					}
					return errors.New("synthetic crash before rename")
				}
			},
			mutate: func(current *File) error {
				current.LastRefresh = now.Add(time.Minute)
				return nil
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "auth.json")
			writeFileValue(t, path, validAuthFile(t, now.Add(time.Hour), now))
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			store := &Store{Path: path}
			if tt.configure != nil {
				tt.configure(store)
			}
			if _, err := store.UpdateAtomically(tt.mutate); err == nil {
				t.Fatal("UpdateAtomically() error = nil")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Fatalf("auth file changed on failure\n got: %s\nwant: %s", after, original)
			}
			temps, err := filepath.Glob(filepath.Join(dir, ".auth.json.tmp-*"))
			if err != nil {
				t.Fatal(err)
			}
			if len(temps) != 0 {
				t.Fatalf("temporary files remain: %v", temps)
			}
		})
	}
}

func TestStoreUpdateValidatesAndSerializesBeforeCreatingDirectory(t *testing.T) {
	t.Parallel()

	for name, mutate := range map[string]func(*File) error{
		"validation": func(current *File) error {
			*current = *validAuthFile(t, time.Now().Add(time.Hour), time.Now())
			current.AuthMode = "apikey"
			return nil
		},
		"serialization": func(current *File) error {
			*current = *validAuthFile(t, time.Now().Add(time.Hour), time.Now())
			current.raw = map[string]json.RawMessage{"bad": json.RawMessage("{")}
			return nil
		},
	} {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "must-not-exist", ".codex")
			store := &Store{Path: filepath.Join(dir, "auth.json")}
			if _, err := store.UpdateAtomically(mutate); err == nil {
				t.Fatal("UpdateAtomically() error = nil")
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("directory was created before validation/serialization: %v", err)
			}
		})
	}
}

func TestStoreUpdateRetriesWhenDiskChangesBeforeRename(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	original := validAuthFile(t, now.Add(time.Hour), now)
	original.raw = map[string]json.RawMessage{"revision": json.RawMessage("0")}
	writeFileValue(t, path, original)
	store := &Store{Path: path}
	hookCalls := 0
	store.beforeRename = func(_, targetPath string) error {
		hookCalls++
		if hookCalls != 1 {
			return nil
		}
		external := original.clone()
		external.raw["revision"] = json.RawMessage("100")
		encoded, err := json.Marshal(external)
		if err != nil {
			return err
		}
		return os.WriteFile(targetPath, encoded, 0o600)
	}

	updated, err := store.UpdateAtomically(func(current *File) error {
		revision, err := rawInt(current.raw["revision"])
		if err != nil {
			return err
		}
		current.raw["revision"] = json.RawMessage(strconv.Itoa(revision + 1))
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateAtomically() error = %v", err)
	}
	if hookCalls < 2 {
		t.Fatalf("beforeRename calls = %d, want retry", hookCalls)
	}
	if revision, err := rawInt(updated.raw["revision"]); err != nil || revision != 101 {
		t.Fatalf("revision = %d, %v; want 101", revision, err)
	}
}

func TestStoreAtomicWritesUpdateSymlinkTargetAndLeaveLink(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	targetDir := t.TempDir()
	linkDir := t.TempDir()
	target := filepath.Join(targetDir, "real-auth.json")
	link := filepath.Join(linkDir, "auth.json")
	initial := validAuthFile(t, now.Add(time.Hour), now)
	writeFileValue(t, target, initial)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink not permitted: %v", err)
	}

	assertStillSymlink := func(t *testing.T) {
		t.Helper()
		info, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatal("Store.Path is no longer a symlink")
		}
		got, err := os.Readlink(link)
		if err != nil {
			t.Fatal(err)
		}
		if got != target {
			t.Fatalf("symlink destination = %q, want %q", got, target)
		}
	}

	t.Run("UpdateAtomically", func(t *testing.T) {
		store := &Store{Path: link}
		newAccess := syntheticJWT(t, map[string]any{"exp": now.Add(2 * time.Hour).Unix(), "generation": "symlink"})
		updated, err := store.UpdateAtomically(func(current *File) error {
			current.Tokens.AccessToken = newAccess
			current.LastRefresh = now.Add(time.Minute)
			return nil
		})
		if err != nil {
			t.Fatalf("UpdateAtomically() error = %v", err)
		}
		if updated.Tokens.AccessToken != newAccess {
			t.Fatal("returned update missing access token")
		}
		assertStillSymlink(t)
		disk, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(disk, []byte(newAccess)) {
			t.Fatal("target file was not updated")
		}
		viaLink, err := store.Read()
		if err != nil {
			t.Fatal(err)
		}
		if viaLink.Tokens.AccessToken != newAccess {
			t.Fatal("read through symlink missing updated access token")
		}
	})

	t.Run("SaveLogin", func(t *testing.T) {
		tokens := completeLoginTokens(t)
		store := &Store{Path: link}
		saved, err := store.SaveLogin(tokens, now.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("SaveLogin() error = %v", err)
		}
		if saved.Tokens.AccessToken != tokens.AccessToken {
			t.Fatal("SaveLogin returned wrong access token")
		}
		assertStillSymlink(t)
		disk, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(disk, []byte(tokens.AccessToken)) {
			t.Fatal("SaveLogin did not update symlink target")
		}
		linkInfo, err := os.Lstat(link)
		if err != nil {
			t.Fatal(err)
		}
		if linkInfo.Mode()&os.ModeSymlink == 0 {
			t.Fatal("SaveLogin replaced the symlink inode")
		}
	})
}

func TestStoreReadWaitsForAtomicRenameCriticalSection(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	writeFileValue(t, path, validAuthFile(t, now.Add(time.Hour), now))
	store := &Store{Path: path}

	processUpdateMu.Lock()
	locked := true
	defer func() {
		if locked {
			processUpdateMu.Unlock()
		}
	}()

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, err := store.Read()
		done <- err
	}()
	<-started

	select {
	case err := <-done:
		t.Fatalf("Read() completed inside atomic rename critical section: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	processUpdateMu.Unlock()
	locked = false
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Read() after critical section error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read() remained blocked after atomic rename critical section")
	}
}

func TestStoreUpdateAtomicallyTakesAuthLockAroundRenameNotMutator(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	writeFileValue(t, path, validAuthFile(t, now.Add(time.Hour), now))
	store := &Store{Path: path}

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

	mutatorStarted := make(chan struct{})
	enteredRename := make(chan struct{})
	store.beforeRename = func(_, _ string) error {
		close(enteredRename)
		return nil
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.UpdateAtomically(func(current *File) error {
			close(mutatorStarted)
			current.LastRefresh = now.Add(time.Minute)
			return nil
		})
		done <- err
	}()

	select {
	case <-mutatorStarted:
	case <-time.After(time.Second):
		t.Fatal("mutator did not run while auth lock was held")
	}

	select {
	case <-enteredRename:
		t.Fatal("rename critical section ran while auth lock was held")
	case err := <-done:
		t.Fatalf("UpdateAtomically completed while auth lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := held.Unlock(); err != nil {
		t.Fatalf("release auth lock: %v", err)
	}
	locked = false

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdateAtomically() error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UpdateAtomically remained blocked after auth lock release")
	}
}

func TestStoreUpdateAtomicallyNestedAuthLockDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	writeFileValue(t, path, validAuthFile(t, now.Add(time.Hour), now))
	store := &Store{Path: path}

	done := make(chan error, 1)
	go func() {
		held, err := lockAuth(context.Background(), path)
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = held.Unlock() }()
		_, err = store.UpdateAtomically(func(current *File) error {
			current.LastRefresh = now.Add(time.Minute)
			return nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("nested UpdateAtomically error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UpdateAtomically deadlocked under lockAuth (refresh holds the sibling lock)")
	}
}

func TestStoreConcurrentReadSummaryAndUpdate(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC().Truncate(time.Second)
	path := filepath.Join(t.TempDir(), "auth.json")
	initial := validAuthFile(t, now.Add(24*time.Hour), now)
	initial.raw = map[string]json.RawMessage{"revision": json.RawMessage("0")}
	writeFileValue(t, path, initial)
	store := &Store{Path: path}

	const writers = 8
	const writesPerWriter = 12
	const readers = 4
	start := make(chan struct{})
	stopReaders := make(chan struct{})
	errorsSeen := make(chan error, writers+readers)
	var writersWG, readersWG sync.WaitGroup

	for i := 0; i < readers; i++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			<-start
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				if _, err := store.Read(); err != nil {
					errorsSeen <- fmt.Errorf("read: %w", err)
					return
				}
				summary, err := store.Summary()
				if err != nil {
					errorsSeen <- fmt.Errorf("summary: %w", err)
					return
				}
				if summary.Source != "codex-cli" || summary.Account == "" {
					errorsSeen <- fmt.Errorf("invalid summary: %#v", summary)
					return
				}
			}
		}()
	}

	for i := 0; i < writers; i++ {
		writersWG.Add(1)
		go func() {
			defer writersWG.Done()
			<-start
			writerStore := &Store{Path: path}
			for j := 0; j < writesPerWriter; j++ {
				_, err := writerStore.UpdateAtomically(func(current *File) error {
					revision, err := rawInt(current.raw["revision"])
					if err != nil {
						return err
					}
					current.raw["revision"] = json.RawMessage(strconv.Itoa(revision + 1))
					return nil
				})
				if err != nil {
					errorsSeen <- fmt.Errorf("update: %w", err)
					return
				}
			}
		}()
	}

	close(start)
	writersWG.Wait()
	close(stopReaders)
	readersWG.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}

	final, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	got, err := rawInt(final.raw["revision"])
	if err != nil {
		t.Fatal(err)
	}
	if want := writers * writesPerWriter; got != want {
		t.Fatalf("revision = %d, want %d", got, want)
	}
}

func validAuthFile(t testing.TB, accessExpiry, lastRefresh time.Time) *File {
	t.Helper()
	account := "account-secret-marker"
	apiKey := "api-key-secret-marker"
	return &File{
		AuthMode:     "chatgpt",
		OpenAIAPIKey: &apiKey,
		Tokens: Tokens{
			IDToken: syntheticJWT(t, map[string]any{
				"email": "user-secret@example.test",
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_plan_type":  "pro",
					"chatgpt_account_id": "account-claim-secret-marker",
				},
			}),
			AccessToken:  syntheticJWT(t, map[string]any{"exp": accessExpiry.Unix(), "marker": "access-token-secret-marker"}),
			RefreshToken: "refresh-token-secret-marker",
			AccountID:    &account,
		},
		LastRefresh: lastRefresh.UTC(),
	}
}

func validAuthMap(t testing.TB, accessExpiry, lastRefresh time.Time) map[string]any {
	t.Helper()
	file := validAuthFile(t, accessExpiry, lastRefresh)
	return map[string]any{
		"auth_mode":      file.AuthMode,
		"OPENAI_API_KEY": *file.OpenAIAPIKey,
		"tokens": map[string]any{
			"id_token":      file.Tokens.IDToken,
			"access_token":  file.Tokens.AccessToken,
			"refresh_token": file.Tokens.RefreshToken,
			"account_id":    *file.Tokens.AccountID,
		},
		"last_refresh": file.LastRefresh.Format(time.RFC3339),
	}
}

func writeFileValue(t testing.TB, path string, value *File) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSONFile(t testing.TB, path string, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cloneJSONMap(t testing.TB, value map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func jsonValuesEqual(left, right any) bool {
	leftJSON, err := json.Marshal(left)
	if err != nil {
		return false
	}
	rightJSON, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return bytes.Equal(leftJSON, rightJSON)
}

func rawInt(value json.RawMessage) (int, error) {
	if len(value) == 0 {
		return 0, nil
	}
	return strconv.Atoi(string(value))
}
