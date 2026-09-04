package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/config"
)

func TestRunServeLoadsEnvironmentAndFlags(t *testing.T) {
	var got config.Config
	deps := testCLIDependencies()
	deps.lookupEnv = func(key string) (string, bool) {
		values := map[string]string{"CLODEX_MODEL": "gpt-5.5:xhigh", "CLODEX_PORT": "9001"}
		value, ok := values[key]
		return value, ok
	}
	deps.serve = func(_ context.Context, cfg config.Config) error {
		got = cfg
		return nil
	}
	if err := run(context.Background(), []string{"serve", "--port", "9002", "--debug-wire"}, deps); err != nil {
		t.Fatal(err)
	}
	if got.Port != 9002 || got.Model != "gpt-5.5:xhigh" || !got.DebugWire {
		t.Fatalf("serve config = %#v", got)
	}
}

func TestRunAuthCommands(t *testing.T) {
	tests := []struct {
		command string
		want    string
	}{
		{command: "login", want: "login"},
		{command: "device", want: "device"},
		{command: "status", want: "status"},
	}
	for _, test := range tests {
		t.Run(test.command, func(t *testing.T) {
			var called string
			deps := testCLIDependencies()
			deps.authLogin = func(context.Context, io.Writer) error { called = "login"; return nil }
			deps.authDevice = func(context.Context, io.Writer) error { called = "device"; return nil }
			deps.authStatus = func(context.Context, io.Writer) error { called = "status"; return nil }
			if err := run(context.Background(), []string{"auth", test.command}, deps); err != nil {
				t.Fatal(err)
			}
			if called != test.want {
				t.Fatalf("called = %q, want %q", called, test.want)
			}
		})
	}
}

func TestRunClaudePreservesArgumentsAndConfiguredDefaults(t *testing.T) {
	var gotArgs []string
	var gotConfig config.Config
	deps := testCLIDependencies()
	deps.lookupEnv = func(key string) (string, bool) {
		if key == "CLODEX_MODEL" {
			return "gpt-5.6-terra:high", true
		}
		return "", false
	}
	deps.claude = func(_ context.Context, args []string, cfg config.Config) error {
		gotArgs = append([]string(nil), args...)
		gotConfig = cfg
		return nil
	}
	wantArgs := []string{"--model", "gpt-5.4:high", "--", "-p", "say hi"}
	if err := run(context.Background(), append([]string{"claude"}, wantArgs...), deps); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) || gotConfig.Model != "gpt-5.6-terra:high" {
		t.Fatalf("claude args=%q config=%#v", gotArgs, gotConfig)
	}
}

func TestRunMetadataAndErrors(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		contains string
		wantErr  string
	}{
		{name: "version", args: []string{"version"}, contains: "test-version"},
		{name: "licenses", args: []string{"licenses"}, contains: "Third-Party Notices"},
		{name: "help", args: []string{"help"}, contains: "clodex serve"},
		{name: "no command", wantErr: "command is required"},
		{name: "unknown", args: []string{"wat"}, wantErr: "unknown command"},
		{name: "extra version arg", args: []string{"version", "extra"}, wantErr: "version takes no arguments"},
		{name: "unknown auth", args: []string{"auth", "wat"}, wantErr: "unknown auth command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps := testCLIDependencies()
			var output bytes.Buffer
			deps.stdout = &output
			err := run(context.Background(), test.args, deps)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("run() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil || !strings.Contains(output.String(), test.contains) {
				t.Fatalf("run() error=%v output=%q, want containing %q", err, output.String(), test.contains)
			}
		})
	}
}

func TestRunServeHelpPrintsUsageToStdout(t *testing.T) {
	deps := testCLIDependencies()
	var output bytes.Buffer
	deps.stdout = &output
	deps.serve = func(context.Context, config.Config) error {
		t.Fatal("serve started despite --help")
		return nil
	}
	if err := run(context.Background(), []string{"serve", "--help"}, deps); err != nil {
		t.Fatalf("run(serve --help) error = %v, want nil", err)
	}
	if !strings.Contains(output.String(), "--port") {
		t.Fatalf("stdout = %q, want containing --port", output.String())
	}
}

func TestRequireCodexAuthMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "auth.json")
	err := requireCodexAuth(&auth.Store{Path: path})
	if err == nil {
		t.Fatal("requireCodexAuth() error = nil, want missing credentials error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("requireCodexAuth() error = %v, want wrapping os.ErrNotExist", err)
	}
	want := "clodex claude requires credentials in ~/.codex/auth.json; run clodex auth login or clodex auth device first"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("requireCodexAuth() error = %q, want containing %q", err, want)
	}
	if !strings.Contains(err.Error(), "open") && !strings.Contains(err.Error(), path) {
		t.Fatalf("requireCodexAuth() error = %q, want wrapped open/path from Read", err)
	}
}

func TestRequireCodexAuthAcceptsValidChatGPTFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	writeMinimalChatGPTAuth(t, path)
	if err := requireCodexAuth(&auth.Store{Path: path}); err != nil {
		t.Fatalf("requireCodexAuth() error = %v", err)
	}
}

func TestRequireCodexAuthRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"auth_mode":"chatgpt","tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := requireCodexAuth(&auth.Store{Path: path})
	if err == nil {
		t.Fatal("requireCodexAuth() error = nil, want fail-closed Read error")
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatalf("requireCodexAuth() treated corrupt file as missing: %v", err)
	}
}

func TestRunPropagatesSubcommandFailure(t *testing.T) {
	want := errors.New("serve failed")
	deps := testCLIDependencies()
	deps.serve = func(context.Context, config.Config) error { return want }
	if err := run(context.Background(), []string{"serve"}, deps); !errors.Is(err, want) {
		t.Fatalf("run() error = %v, want %v", err, want)
	}
}

func testCLIDependencies() cliDependencies {
	return cliDependencies{
		version: "test-version", stdout: io.Discard, stderr: io.Discard,
		lookupEnv:  func(string) (string, bool) { return "", false },
		serve:      func(context.Context, config.Config) error { return nil },
		authLogin:  func(context.Context, io.Writer) error { return nil },
		authDevice: func(context.Context, io.Writer) error { return nil },
		authStatus: func(context.Context, io.Writer) error { return nil },
		claude:     func(context.Context, []string, config.Config) error { return nil },
	}
}

func writeMinimalChatGPTAuth(t *testing.T, path string) {
	t.Helper()
	token := cliTestJWT(t, map[string]any{
		"email": "claude-auth@example.test",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type": "plus",
		},
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	data := fmt.Sprintf(`{"auth_mode":"chatgpt","tokens":{"id_token":%q,"access_token":%q,"refresh_token":"refresh-token","account_id":null},"last_refresh":%q}`, token, token, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cliTestJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(encoded) + ".sig"
}
