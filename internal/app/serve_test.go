package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/config"
	clodexstatus "github.com/Aotricx/Clodex/internal/status"
)

func TestRefreshAuthStatusRereadsRotatedCodexFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	store := &auth.Store{Path: path}
	state := clodexstatus.New("test")
	if err := refreshAuthStatus(store, state); err != nil {
		t.Fatal(err)
	}
	if got := state.Snapshot().Auth.Source; got != "none" {
		t.Fatalf("missing auth source = %q", got)
	}

	expiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	token := appTestJWT(t, map[string]any{
		"exp":                         expiry.Unix(),
		"email":                       "owner@example.test",
		"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "pro"},
	})
	data := fmt.Sprintf(`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":%q,"access_token":%q,"refresh_token":"secret","account_id":null},"last_refresh":%q}`, token, token, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refreshAuthStatus(store, state); err != nil {
		t.Fatal(err)
	}
	got := state.Snapshot().Auth
	if got.Source != auth.CodexCLISource || got.Plan != "Pro" || got.Account == "owner@example.test" || got.Expiry == nil || !got.Expiry.Equal(expiry) {
		t.Fatalf("rotated auth status = %#v", got)
	}
}

func appTestJWT(t testing.TB, payload map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(encoded) + ".signature"
}

func TestServeStartsWithoutAuthUsingFallbackCatalog(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Port = 0
	ready := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{
			Config: cfg, Version: "test-version",
			AuthPath: filepath.Join(root, "missing", "auth.json"), CatalogPath: filepath.Join(root, "models.json"), DumpDir: filepath.Join(root, "wire"),
			Ready: func(address net.Addr) { ready <- address }, Stderr: io.Discard,
		})
	}()
	address := <-ready
	response, err := http.Get("http://" + address.String() + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var snapshot clodexstatus.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || snapshot.Version != "test-version" || snapshot.Auth.Source != "none" || snapshot.Catalog.Source != "fallback" || snapshot.Retry.RemainingBudget != int64(cfg.GlobalRetryBudget) {
		t.Fatalf("status = %d %#v", response.StatusCode, snapshot)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestDefaultPathsUseUserHomeAndCodexInteropFiles(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "users", "test")
	paths, err := DefaultPaths(func() (string, error) { return home, nil })
	if err != nil {
		t.Fatal(err)
	}
	if paths.Auth != filepath.Join(home, ".codex", "auth.json") || paths.Catalog != filepath.Join(home, ".codex", "models_cache.json") || !strings.Contains(paths.Dump, "clodex") {
		t.Fatalf("paths = %#v", paths)
	}
	if _, err := DefaultPaths(func() (string, error) { return "", os.ErrNotExist }); err == nil {
		t.Fatal("DefaultPaths() error = nil")
	}
}

func TestServeStartsWithPathOverridesWhenHomeDirFails(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Port = 0
	ready := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{
			Config: cfg, Version: "test",
			AuthPath: filepath.Join(root, "missing", "auth.json"), CatalogPath: filepath.Join(root, "models.json"), DumpDir: filepath.Join(root, "wire"),
			HomeDir: func() (string, error) { return "", os.ErrNotExist },
			Ready:   func(address net.Addr) { ready <- address }, Stderr: io.Discard,
		})
	}()
	select {
	case <-ready:
		cancel()
	case err := <-done:
		t.Fatalf("Serve() error = %v", err)
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Serve did not become ready")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestServeRejectsUnknownSmallFastModelAgainstFallbackCatalog(t *testing.T) {
	root := t.TempDir()
	cfg := config.Defaults()
	cfg.Port = 0
	cfg.SmallFastModel = "not-a-catalog-model:low"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ServeOptions{
			Config: cfg, Version: "test",
			AuthPath: filepath.Join(root, "missing", "auth.json"), CatalogPath: filepath.Join(root, "models.json"), DumpDir: filepath.Join(root, "wire"),
			Ready: func(address net.Addr) { ready <- address }, Stderr: io.Discard,
		})
	}()
	select {
	case address := <-ready:
		cancel()
		t.Fatalf("Serve started on %v with unknown small fast model", address)
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not-a-catalog-model") {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Serve did not return")
	}
}

func TestServeRejectsCorruptExistingAuthInsteadOfMaskingIt(t *testing.T) {
	root := t.TempDir()
	authPath := filepath.Join(root, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt","tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Port = 0
	err := Serve(context.Background(), ServeOptions{Config: cfg, Version: "test", AuthPath: authPath, CatalogPath: filepath.Join(root, "models.json"), DumpDir: filepath.Join(root, "wire"), Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "auth") {
		t.Fatalf("Serve() error = %v", err)
	}
}
