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
	"strings"
	"sync"
	"time"
)

const maxUpdateAttempts = 256

var errAuthChanged = errors.New("auth file changed during update")

// processUpdateMu closes the compare/rename gap between Store instances in this
// process. User mutators always run before this lock is acquired.
var processUpdateMu sync.RWMutex

// File is the Codex CLI auth.json shape used by ChatGPT OAuth. Unknown fields
// remain attached to File and Tokens across read/update/write cycles.
type File struct {
	AuthMode     string
	OpenAIAPIKey *string
	Tokens       Tokens
	LastRefresh  time.Time

	raw map[string]json.RawMessage
}

// Tokens is the Codex ChatGPT token record. AccountID is source-valid when
// null; account identity can also be recovered from JWT claims.
type Tokens struct {
	IDToken      string
	AccessToken  string
	RefreshToken string
	AccountID    *string

	raw map[string]json.RawMessage
}

// Store cooperatively reads and atomically updates one explicit auth.json.
// Do not copy a Store after first use.
type Store struct {
	Path string

	// beforeRename exists for deterministic package tests and refresh-race
	// coordination. Production constructors leave it nil.
	beforeRename func(tempPath, targetPath string) error
}

// CodexAuthPath resolves ~/.codex/auth.json from an explicit home directory.
// It intentionally never consults HOME, CODEX_HOME, or os.UserHomeDir.
func CodexAuthPath(home string) (string, error) {
	if home == "" {
		return "", errors.New("resolve Codex auth path: home is empty")
	}
	return filepath.Join(home, ".codex", "auth.json"), nil
}

// ResolveCodexAuthPath returns $CODEX_HOME/auth.json when CODEX_HOME is a
// nonempty directory path. Codex treats CODEX_HOME as the config directory
// itself, not as a parent of .codex. Otherwise it falls back to CodexAuthPath.
func ResolveCodexAuthPath(home string) (string, error) {
	return CodexAuthPathFromEnv(home, os.Getenv)
}

// CodexAuthPathFromEnv is the testable form of ResolveCodexAuthPath.
func CodexAuthPathFromEnv(home string, getenv func(string) string) (string, error) {
	if getenv != nil {
		if dir := getenv("CODEX_HOME"); dir != "" {
			return filepath.Join(dir, "auth.json"), nil
		}
	}
	return CodexAuthPath(home)
}

// Read returns a validated ChatGPT OAuth file with unknown JSON fields retained.
func (s *Store) Read() (*File, error) {
	file, _, err := s.readSnapshot()
	return file, err
}

// Summary reads current disk state and returns only redacted status.
func (s *Store) Summary() (Summary, error) {
	file, err := s.Read()
	if err != nil {
		return Summary{}, err
	}
	return file.Summary()
}

// UpdateAtomically re-reads disk, passes a detached clone to mutator, validates
// it, then persists with same-directory temp+fsync+rename. mutator runs without
// Store's mutex or the sibling auth lock and may be called again when disk
// changes; it must therefore be side-effect-free. The compare-and-rename
// critical section takes lockAuth (Clodex-vs-Clodex) and processUpdateMu.
// Retries are bounded by maxUpdateAttempts.
//
// A missing file supplies an empty File, allowing login flows to create initial
// state. API-key auth is never accepted. SaveLogin writes through this path.
func (s *Store) UpdateAtomically(mutator func(current *File) error) (*File, error) {
	if mutator == nil {
		return nil, errors.New("update auth: nil mutator")
	}
	if err := s.validatePath(); err != nil {
		return nil, err
	}

	for attempt := 0; attempt < maxUpdateAttempts; attempt++ {
		base, original, err := s.readSnapshot()
		existed := true
		if errors.Is(err, os.ErrNotExist) {
			base = &File{}
			original = nil
			existed = false
		} else if err != nil {
			return nil, err
		}

		candidate := base.clone()
		if err := mutator(candidate); err != nil {
			return nil, safeWrap("update auth: mutator failed", err)
		}
		if err := candidate.validate(); err != nil {
			return nil, fmt.Errorf("update auth: %w", err)
		}
		encoded, err := json.MarshalIndent(candidate, "", "  ")
		if err != nil {
			return nil, safeWrap("update auth: serialize failed", err)
		}

		err = s.persistAtomic(encoded, original, existed)
		if errors.Is(err, errAuthChanged) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return candidate.clone(), nil
	}
	return nil, errors.New("update auth: contention limit exceeded")
}

func (s *Store) persistAtomic(encoded, original []byte, existed bool) error {
	writePath, err := resolveAuthWritePath(s.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(writePath), 0o700); err != nil {
		return fmt.Errorf("prepare auth directory: %w", err)
	}
	// Skip lockAuth when refreshIf already holds it. Darwin/Windows locks are
	// per-handle; nesting would livelock. Other goroutines still wait.
	if !authLockHeldByCurrentGoroutine(s.Path) {
		held, err := lockAuth(context.Background(), s.Path)
		if err != nil {
			return err
		}
		defer func() { _ = held.Unlock() }()
	}
	processUpdateMu.Lock()
	defer processUpdateMu.Unlock()
	return s.writeAtomic(encoded, original, existed)
}

func (s *Store) readSnapshot() (*File, []byte, error) {
	if err := s.validatePath(); err != nil {
		return nil, nil, err
	}
	processUpdateMu.RLock()
	defer processUpdateMu.RUnlock()
	data, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("read auth file: %w", err)
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, safeWrap("decode auth file failed", err)
	}
	if err := file.validate(); err != nil {
		return nil, nil, fmt.Errorf("read auth file: %w", err)
	}
	return file.clone(), bytes.Clone(data), nil
}

func (s *Store) validatePath() error {
	if s == nil {
		return errors.New("auth store is nil")
	}
	if s.Path == "" {
		return errors.New("auth store path is empty")
	}
	return nil
}

func (s *Store) writeAtomic(encoded, expected []byte, expectedExists bool) (result error) {
	writePath, err := resolveAuthWritePath(s.Path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(writePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("prepare auth directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(writePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create auth temporary file: %w", err)
	}
	tempPath := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempPath)
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure auth temporary file: %w", err)
	}
	if _, err := temp.Write(encoded); err != nil {
		return fmt.Errorf("write auth temporary file: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync auth temporary file: %w", err)
	}
	if err := temp.Close(); err != nil {
		closed = true
		return fmt.Errorf("close auth temporary file: %w", err)
	}
	closed = true

	if s.beforeRename != nil {
		if err := s.beforeRename(tempPath, writePath); err != nil {
			return safeWrap("prepare auth rename failed", err)
		}
	}
	changed, err := targetChanged(writePath, expected, expectedExists)
	if err != nil {
		return err
	}
	if changed {
		return errAuthChanged
	}
	if err := os.Rename(tempPath, writePath); err != nil {
		return fmt.Errorf("rename auth temporary file: %w", err)
	}
	if err := syncDirectory(dir); err != nil {
		return fmt.Errorf("sync auth directory: %w", err)
	}
	return nil
}

// resolveAuthWritePath returns the path that atomic temp+rename must replace.
// A symlink is resolved so the write lands on the target file and leaves the
// link inode in place. A missing path is created at path. A regular file is
// used as-is, including when an ancestor directory is itself a symlink.
func resolveAuthWritePath(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return path, nil
	}
	if err != nil {
		return "", fmt.Errorf("stat auth path: %w", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve auth symlink: %w", err)
	}
	return resolved, nil
}

func targetChanged(path string, expected []byte, expectedExists bool) (bool, error) {
	actual, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return expectedExists, nil
	}
	if err != nil {
		return false, fmt.Errorf("compare auth file before rename: %w", err)
	}
	if !expectedExists {
		return true, nil
	}
	return !bytes.Equal(actual, expected), nil
}

func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (f *File) validate() error {
	if f == nil {
		return errors.New("auth file is nil")
	}
	if f.AuthMode != "chatgpt" {
		return errors.New("auth_mode must be chatgpt")
	}
	if strings.TrimSpace(f.Tokens.IDToken) == "" {
		return errors.New("tokens.id_token is required")
	}
	if _, err := ParseJWTClaims(f.Tokens.IDToken); err != nil {
		return fmt.Errorf("tokens.id_token is invalid: %w", err)
	}
	if strings.TrimSpace(f.Tokens.AccessToken) == "" {
		return errors.New("tokens.access_token is required")
	}
	if _, err := ParseJWTClaims(f.Tokens.AccessToken); err != nil {
		return fmt.Errorf("tokens.access_token is invalid: %w", err)
	}
	if strings.TrimSpace(f.Tokens.RefreshToken) == "" {
		return errors.New("tokens.refresh_token is required")
	}
	if f.Tokens.AccountID != nil && strings.TrimSpace(*f.Tokens.AccountID) == "" {
		return errors.New("tokens.account_id must be nonempty when present")
	}
	if f.LastRefresh.IsZero() {
		return errors.New("last_refresh is required")
	}
	if _, err := f.LastRefresh.MarshalText(); err != nil {
		return errors.New("last_refresh is outside RFC3339 range")
	}
	return nil
}

func (f *File) clone() *File {
	if f == nil {
		return nil
	}
	clone := *f
	clone.OpenAIAPIKey = cloneString(f.OpenAIAPIKey)
	clone.Tokens = *f.Tokens.clone()
	clone.raw = cloneRawMap(f.raw)
	return &clone
}

func (t *Tokens) clone() *Tokens {
	if t == nil {
		return nil
	}
	clone := *t
	clone.AccountID = cloneString(t.AccountID)
	clone.raw = cloneRawMap(t.raw)
	return &clone
}

func cloneString(source *string) *string {
	if source == nil {
		return nil
	}
	clone := *source
	return &clone
}

func cloneRawMap(source map[string]json.RawMessage) map[string]json.RawMessage {
	if source == nil {
		return nil
	}
	clone := make(map[string]json.RawMessage, len(source))
	for key, value := range source {
		clone[key] = bytes.Clone(value)
	}
	return clone
}

func (f *File) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("auth file must be a JSON object")
	}

	var decoded File
	if raw, ok := fields["auth_mode"]; ok {
		if err := json.Unmarshal(raw, &decoded.AuthMode); err != nil {
			return fmt.Errorf("decode auth_mode: %w", err)
		}
	}
	if raw, ok := fields["OPENAI_API_KEY"]; ok {
		if err := json.Unmarshal(raw, &decoded.OpenAIAPIKey); err != nil {
			return fmt.Errorf("decode OPENAI_API_KEY: %w", err)
		}
	}
	if raw, ok := fields["tokens"]; ok {
		if err := json.Unmarshal(raw, &decoded.Tokens); err != nil {
			return fmt.Errorf("decode tokens: %w", err)
		}
	}
	if raw, ok := fields["last_refresh"]; ok {
		if err := json.Unmarshal(raw, &decoded.LastRefresh); err != nil {
			return fmt.Errorf("decode last_refresh: %w", err)
		}
	}
	for _, known := range []string{"auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh"} {
		delete(fields, known)
	}
	decoded.raw = cloneRawMap(fields)
	*f = decoded
	return nil
}

func (f File) MarshalJSON() ([]byte, error) {
	fields := cloneRawMap(f.raw)
	if fields == nil {
		fields = make(map[string]json.RawMessage, 4)
	}
	known := map[string]any{
		"auth_mode":      f.AuthMode,
		"OPENAI_API_KEY": f.OpenAIAPIKey,
		"tokens":         f.Tokens,
		"last_refresh":   f.LastRefresh,
	}
	for name, value := range known {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[name] = encoded
	}
	return json.Marshal(fields)
}

func (t *Tokens) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("tokens must be a JSON object")
	}

	var decoded Tokens
	known := []struct {
		name string
		into any
	}{
		{"id_token", &decoded.IDToken},
		{"access_token", &decoded.AccessToken},
		{"refresh_token", &decoded.RefreshToken},
		{"account_id", &decoded.AccountID},
	}
	for _, field := range known {
		if raw, ok := fields[field.name]; ok {
			if err := json.Unmarshal(raw, field.into); err != nil {
				return fmt.Errorf("decode %s: %w", field.name, err)
			}
		}
		delete(fields, field.name)
	}
	decoded.raw = cloneRawMap(fields)
	*t = decoded
	return nil
}

func (t Tokens) MarshalJSON() ([]byte, error) {
	fields := cloneRawMap(t.raw)
	if fields == nil {
		fields = make(map[string]json.RawMessage, 4)
	}
	known := map[string]any{
		"id_token":      t.IDToken,
		"access_token":  t.AccessToken,
		"refresh_token": t.RefreshToken,
		"account_id":    t.AccountID,
	}
	for name, value := range known {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[name] = encoded
	}
	return json.Marshal(fields)
}

type safeError struct {
	message string
	cause   error
}

func safeWrap(message string, cause error) error {
	return &safeError{message: message, cause: cause}
}

func (e *safeError) Error() string { return e.message }
func (e *safeError) Unwrap() error { return e.cause }
