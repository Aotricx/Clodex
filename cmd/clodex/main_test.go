package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

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
