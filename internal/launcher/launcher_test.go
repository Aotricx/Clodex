package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/model"
)

func TestParseClaudeArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    Arguments
		wantErr string
	}{
		{
			name: "defaults and passthrough",
			args: []string{"--", "-p", "write a file", "--model", "belongs-to-claude"},
			want: Arguments{Model: "gpt-main:medium", SmallFastModel: "gpt-small:low", ClaudeArgs: []string{"-p", "write a file", "--model", "belongs-to-claude"}},
		},
		{
			name: "launcher overrides",
			args: []string{"--model", "gpt-main:xhigh", "--small-fast-model=gpt-small:medium", "--", "--verbose"},
			want: Arguments{Model: "gpt-main:xhigh", SmallFastModel: "gpt-small:medium", ClaudeArgs: []string{"--verbose"}},
		},
		{name: "no Claude arguments", want: Arguments{Model: "gpt-main:medium", SmallFastModel: "gpt-small:low"}},
		{name: "missing model", args: []string{"--model"}, wantErr: "--model requires"},
		{name: "empty model", args: []string{"--model="}, wantErr: "--model must not be empty"},
		{name: "missing small model", args: []string{"--small-fast-model"}, wantErr: "--small-fast-model requires"},
		{name: "unknown launcher flag", args: []string{"--verbose"}, wantErr: "Claude arguments must follow --"},
		{name: "positional before delimiter", args: []string{"hello"}, wantErr: "Claude arguments must follow --"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(test.args, "gpt-main:medium", "gpt-small:low")
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Parse() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Parse() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestProbeHealth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		want    Health
		wantErr error
	}{
		{name: "healthy", status: 200, body: `{"service":"clodex","status":"ok","version":"v0.1.0"}`, want: Health{Service: "clodex", Status: "ok", Version: "v0.1.0"}},
		{name: "foreign service", status: 200, body: `{"service":"other","status":"ok","version":"v"}`, wantErr: ErrForeignListener},
		{name: "empty version", status: 200, body: `{"service":"clodex","status":"ok","version":" "}`, wantErr: ErrForeignListener},
		{name: "wrong status", status: 200, body: `{"service":"clodex","status":"starting","version":"v"}`, wantErr: ErrForeignListener},
		{name: "HTTP status", status: 404, body: `{}`, wantErr: ErrForeignListener},
		{name: "malformed", status: 200, body: `{`, wantErr: ErrForeignListener},
		{name: "trailing JSON", status: 200, body: `{"service":"clodex","status":"ok","version":"v"}{}`, wantErr: ErrForeignListener},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.Path != "/healthz" {
					t.Errorf("probe request = %s %s", request.Method, request.URL.Path)
				}
				writer.WriteHeader(test.status)
				_, _ = io.WriteString(writer, test.body)
			}))
			defer server.Close()

			got, err := ProbeHealth(context.Background(), server.URL+"/healthz")
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("ProbeHealth() error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("ProbeHealth() = %#v, %v; want %#v, nil", got, err, test.want)
			}
		})
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ProbeHealth(context.Background(), "http://"+address+"/healthz"); !errors.Is(err, ErrProxyUnavailable) {
		t.Fatalf("unavailable ProbeHealth() error = %v, want ErrProxyUnavailable", err)
	}
}

func TestProbeHealthDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	healthyServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(healthy())
	}))
	t.Cleanup(healthyServer.Close)

	redirectServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, healthyServer.URL+"/healthz", http.StatusFound)
	}))
	t.Cleanup(redirectServer.Close)

	_, err := ProbeHealth(context.Background(), redirectServer.URL+"/healthz")
	if !errors.Is(err, ErrForeignListener) {
		t.Fatalf("ProbeHealth() error = %v, want ErrForeignListener", err)
	}
}

func TestProbeHealthHangIsForeignListener(t *testing.T) {
	t.Parallel()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		<-t.Context().Done()
	}()

	_, err = ProbeHealth(context.Background(), "http://"+listener.Addr().String()+"/healthz")
	if !errors.Is(err, ErrForeignListener) {
		t.Fatalf("ProbeHealth() error = %v, want ErrForeignListener", err)
	}
}

func TestRunReusesHealthyProxyAndBuildsClaudeEnvironment(t *testing.T) {
	t.Parallel()

	var commands []Command
	options := testOptions(8484)
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(_ context.Context, target string) (Health, error) {
		if target != "http://127.0.0.1:8484/healthz" {
			t.Fatalf("probe target = %q", target)
		}
		return healthy(), nil
	}
	options.Dependencies.Executable = func() (string, error) {
		t.Fatal("current executable resolved while reusing proxy")
		return "", errors.New("unreachable")
	}
	options.Dependencies.Environ = func() []string {
		return []string{
			"KEEP=present",
			"ANTHROPIC_MODEL=stale",
			"anthropic_model=duplicate",
			"ANTHROPIC_BASE_URL=http://wrong",
			"CLAUDE_CODE_AUTO_COMPACT_WINDOW=100000",
		}
	}
	options.Dependencies.Spawn = func(_ context.Context, command Command) (Process, error) {
		commands = append(commands, command)
		process := newProcessStub()
		process.complete(nil)
		return process, nil
	}

	err := Run(context.Background(), []string{"--", "-p", "task with spaces"}, options)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(commands) != 1 {
		t.Fatalf("spawned %d commands, want Claude only", len(commands))
	}
	command := commands[0]
	if command.Path != "/test/claude" || !reflect.DeepEqual(command.Args, []string{"-p", "task with spaces"}) {
		t.Fatalf("Claude command = %#v", command)
	}
	assertEnv(t, command.Env, map[string]string{
		"KEEP":                            "present",
		"ANTHROPIC_BASE_URL":              "http://127.0.0.1:8484",
		"ANTHROPIC_AUTH_TOKEN":            DummyAuthToken,
		"ANTHROPIC_MODEL":                 "anthropic-clodex-gpt-main:medium[1m]",
		"ANTHROPIC_SMALL_FAST_MODEL":      "anthropic-clodex-gpt-small:low[1m]",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "272000",
	})
	if _, ok := environmentValue(command.Env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS"); ok {
		t.Fatal("obsolete MAX_CONTEXT override survived launcher environment")
	}
	for _, key := range targetEnvironmentKeys {
		if count := envKeyCount(command.Env, key); count != 1 {
			t.Errorf("environment key %s count = %d, want 1 (%q)", key, count, command.Env)
		}
	}
}

func TestRunUsesCatalogFallbackContextForConfiguredAndUnknownModels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		args        []string
		wantModel   string
		wantSmall   string
		wantContext string
		wantErr     string
	}{
		{name: "known override", args: []string{"--model", "gpt-small:low"}, wantModel: "gpt-small:low", wantSmall: "gpt-small:low", wantContext: "128000"},
		{name: "unknown aliases fallback", args: []string{"--model", "future-model:xhigh", "--small-fast-model", "claude-haiku-4"}, wantModel: "gpt-main:xhigh", wantSmall: "gpt-small:low", wantContext: "272000"},
		{name: "invalid known effort", args: []string{"--model", "gpt-small:ultra"}, wantErr: "does not support effort"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var command Command
			options := testOptions(8484)
			options.Dependencies = testDependencies()
			options.Dependencies.Probe = func(context.Context, string) (Health, error) { return healthy(), nil }
			options.Dependencies.Spawn = func(_ context.Context, got Command) (Process, error) {
				command = got
				process := newProcessStub()
				process.complete(nil)
				return process, nil
			}
			err := Run(context.Background(), test.args, options)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Run() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			assertEnv(t, command.Env, map[string]string{
				"ANTHROPIC_MODEL":                 model.ClaudeCarrierID(test.wantModel),
				"ANTHROPIC_SMALL_FAST_MODEL":      model.ClaudeCarrierID(test.wantSmall),
				"CLAUDE_CODE_AUTO_COMPACT_WINDOW": test.wantContext,
			})
		})
	}
}

func TestRunRejectsForeignListenerWithoutSpawning(t *testing.T) {
	t.Parallel()

	options := testOptions(8484)
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(context.Context, string) (Health, error) {
		return Health{Service: "other", Status: "ok", Version: "v"}, nil
	}
	var spawned atomic.Bool
	options.Dependencies.Spawn = func(context.Context, Command) (Process, error) {
		spawned.Store(true)
		return nil, errors.New("unexpected spawn")
	}

	err := Run(context.Background(), nil, options)
	if !errors.Is(err, ErrForeignListener) || !strings.Contains(err.Error(), "127.0.0.1:8484") {
		t.Fatalf("Run() error = %v, want clear foreign-listener error", err)
	}
	if spawned.Load() {
		t.Fatal("foreign listener caused spawn")
	}
}

func TestRunSpawnsCurrentExecutableAndWaitsForReadiness(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var commands []Command
	probeCalls := 0
	proxy := newProcessStub()
	t.Cleanup(func() { proxy.complete(nil) })
	options := testOptions(9753)
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(_ context.Context, target string) (Health, error) {
		if target != "http://127.0.0.1:9753/healthz" {
			t.Fatalf("probe target = %q", target)
		}
		probeCalls++
		if probeCalls < 3 {
			return Health{}, ErrProxyUnavailable
		}
		return healthy(), nil
	}
	options.Dependencies.Spawn = func(_ context.Context, command Command) (Process, error) {
		mu.Lock()
		commands = append(commands, command)
		index := len(commands)
		mu.Unlock()
		if index == 1 {
			return proxy, nil
		}
		claude := newProcessStub()
		claude.complete(nil)
		return claude, nil
	}

	if err := Run(context.Background(), []string{"--", "--print"}, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(commands) != 2 {
		t.Fatalf("commands = %d, want proxy and Claude", len(commands))
	}
	if commands[0].Path != "/test/clodex" || !reflect.DeepEqual(commands[0].Args, []string{"serve", "--port", "9753"}) {
		t.Fatalf("proxy command = %#v", commands[0])
	}
	if commands[1].Path != "/test/claude" || !reflect.DeepEqual(commands[1].Args, []string{"--print"}) {
		t.Fatalf("Claude command = %#v", commands[1])
	}
	if !commands[0].DetachProcessGroup {
		t.Fatal("proxy spawn missing DetachProcessGroup")
	}
	if commands[1].DetachProcessGroup {
		t.Fatal("Claude spawn unexpectedly detached process group")
	}
	if proxy.killed.Load() {
		t.Fatal("healthy spawned proxy was killed")
	}
}

func TestRunKillsAndWaitsForFailedProxyStart(t *testing.T) {
	t.Parallel()

	proxy := newProcessStub()
	options := testOptions(8484)
	options.ReadyTimeout = 20 * time.Millisecond
	options.ProbeInterval = time.Millisecond
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(context.Context, string) (Health, error) {
		return Health{}, ErrProxyUnavailable
	}
	spawnCount := 0
	options.Dependencies.Spawn = func(context.Context, Command) (Process, error) {
		spawnCount++
		return proxy, nil
	}

	err := Run(context.Background(), nil, options)
	if err == nil || !strings.Contains(err.Error(), "did not become healthy") {
		t.Fatalf("Run() error = %v, want readiness timeout", err)
	}
	if spawnCount != 1 {
		t.Fatalf("spawn count = %d, want proxy only", spawnCount)
	}
	if !proxy.killed.Load() || !proxy.waited.Load() {
		t.Fatalf("failed proxy cleanup killed=%v waited=%v", proxy.killed.Load(), proxy.waited.Load())
	}
}

func TestRunReportsProxyExitBeforeReadiness(t *testing.T) {
	t.Parallel()

	proxy := newProcessStub()
	proxy.complete(errors.New("serve exited 23"))
	options := testOptions(8484)
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(context.Context, string) (Health, error) {
		return Health{}, ErrProxyUnavailable
	}
	options.Dependencies.Spawn = func(context.Context, Command) (Process, error) { return proxy, nil }

	err := Run(context.Background(), nil, options)
	if err == nil || !strings.Contains(err.Error(), "exited before becoming healthy") || !strings.Contains(err.Error(), "serve exited 23") {
		t.Fatalf("Run() error = %v", err)
	}
	if proxy.killed.Load() || !proxy.waited.Load() {
		t.Fatalf("exited proxy killed=%v waited=%v", proxy.killed.Load(), proxy.waited.Load())
	}
}

func TestRunReusesHealthyListenerWhenSpawnedProxyExits(t *testing.T) {
	t.Parallel()

	proxy := newProcessStub()
	var probes atomic.Int32
	options := testOptions(8484)
	options.ReadyTimeout = 2 * time.Second
	options.ProbeInterval = time.Hour
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(context.Context, string) (Health, error) {
		switch probes.Add(1) {
		case 1:
			return Health{}, ErrProxyUnavailable
		case 2:
			proxy.complete(errors.New("serve exited 23"))
			return Health{}, ErrProxyUnavailable
		default:
			if proxy.waited.Load() {
				return healthy(), nil
			}
			return Health{}, ErrProxyUnavailable
		}
	}
	options.Dependencies.Spawn = func(_ context.Context, command Command) (Process, error) {
		if command.Path == "/test/clodex" {
			return proxy, nil
		}
		claude := newProcessStub()
		claude.complete(nil)
		return claude, nil
	}

	if err := Run(context.Background(), nil, options); err != nil {
		t.Fatalf("Run() error = %v, want reuse after spawned proxy exit", err)
	}
	if proxy.killed.Load() || !proxy.waited.Load() {
		t.Fatalf("exited proxy killed=%v waited=%v", proxy.killed.Load(), proxy.waited.Load())
	}
}

func TestRunReportsForeignListenerWhenSpawnedProxyExits(t *testing.T) {
	t.Parallel()

	proxy := newProcessStub()
	var probes atomic.Int32
	options := testOptions(8484)
	options.ReadyTimeout = 2 * time.Second
	options.ProbeInterval = time.Hour
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(context.Context, string) (Health, error) {
		switch probes.Add(1) {
		case 1:
			return Health{}, ErrProxyUnavailable
		case 2:
			proxy.complete(errors.New("serve exited 23"))
			return Health{}, ErrProxyUnavailable
		default:
			return Health{}, ErrForeignListener
		}
	}
	options.Dependencies.Spawn = func(context.Context, Command) (Process, error) { return proxy, nil }

	err := Run(context.Background(), nil, options)
	if !errors.Is(err, ErrForeignListener) {
		t.Fatalf("Run() error = %v, want ErrForeignListener", err)
	}
}

func TestRunPreservesClaudeWaitError(t *testing.T) {
	t.Parallel()

	want := errors.New("Claude exit 17")
	options := testOptions(8484)
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(context.Context, string) (Health, error) { return healthy(), nil }
	options.Dependencies.Spawn = func(context.Context, Command) (Process, error) {
		process := newProcessStub()
		process.complete(want)
		return process, nil
	}

	err := Run(context.Background(), nil, options)
	if !errors.Is(err, want) {
		t.Fatalf("Run() error = %v, want wrapping %v", err, want)
	}
}

func TestRunPreservesSubprocessExitCode(t *testing.T) {
	if os.Getenv("CLODEX_LAUNCHER_EXIT_HELPER") == "1" {
		os.Exit(17)
	}

	options := testOptions(8484)
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(context.Context, string) (Health, error) { return healthy(), nil }
	options.Dependencies.LookPath = func(string) (string, error) { return os.Args[0], nil }
	options.Dependencies.Spawn = nil // Exercise the real cross-platform os/exec spawner.
	options.Dependencies.Environ = func() []string {
		return append(os.Environ(), "CLODEX_LAUNCHER_EXIT_HELPER=1")
	}

	err := Run(context.Background(), []string{"--", "-test.run=^TestRunPreservesSubprocessExitCode$"}, options)
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ExitCode() != 17 {
		t.Fatalf("Run() error = %v, want preserved subprocess exit code 17", err)
	}
}

func TestRunUsesShippingModelDefaults(t *testing.T) {
	t.Parallel()

	fallback, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	var command Command
	options := Options{Port: 8484, Dependencies: testDependencies()}
	options.Dependencies.Catalog = staticResolver{resolution: catalog.Resolution{Catalog: fallback, Source: catalog.SourceFallback}}
	options.Dependencies.Probe = func(context.Context, string) (Health, error) { return healthy(), nil }
	options.Dependencies.Spawn = func(_ context.Context, got Command) (Process, error) {
		command = got
		process := newProcessStub()
		process.complete(nil)
		return process, nil
	}

	if err := Run(context.Background(), nil, options); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertEnv(t, command.Env, map[string]string{
		"ANTHROPIC_MODEL":                 "anthropic-clodex-gpt-5.6-sol:medium[1m]",
		"ANTHROPIC_SMALL_FAST_MODEL":      "anthropic-clodex-gpt-5.4-mini:low[1m]",
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "272000",
	})
}

func TestRunSubprocessPassThroughAndSpawnedHealth(t *testing.T) {
	if os.Getenv("CLODEX_LAUNCHER_HELPER") == "1" {
		runLauncherHelper(t)
		return
	}

	port := freePort(t)
	capturePath := filepath.Join(t.TempDir(), "claude.json")
	var proxyCommand *exec.Cmd
	var healthTargets []string
	var spawned []Command
	options := testOptions(port)
	options.ReadyTimeout = 2 * time.Second
	options.ProbeInterval = 10 * time.Millisecond
	options.Dependencies = testDependencies()
	options.Dependencies.Probe = func(ctx context.Context, target string) (Health, error) {
		healthTargets = append(healthTargets, target)
		return ProbeHealth(ctx, target)
	}
	options.Dependencies.Environ = func() []string {
		return []string{"KEEP=subprocess", "ANTHROPIC_MODEL=old", "anthropic_model=duplicate"}
	}
	options.Dependencies.Spawn = func(ctx context.Context, command Command) (Process, error) {
		spawned = append(spawned, command)
		role := "claude"
		if command.Path == "/test/clodex" {
			role = "proxy"
		}
		encodedArgs, err := json.Marshal(command.Args)
		if err != nil {
			return nil, err
		}
		helper := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunSubprocessPassThroughAndSpawnedHealth$")
		helper.Env = append(append([]string{}, command.Env...),
			"CLODEX_LAUNCHER_HELPER=1",
			"CLODEX_LAUNCHER_HELPER_ROLE="+role,
			"CLODEX_LAUNCHER_HELPER_ARGS="+string(encodedArgs),
			"CLODEX_LAUNCHER_HELPER_CAPTURE="+capturePath,
			"CLODEX_LAUNCHER_HELPER_PORT="+strconv.Itoa(port),
		)
		helper.Stdin, helper.Stdout, helper.Stderr = command.Stdin, command.Stdout, command.Stderr
		if err := helper.Start(); err != nil {
			return nil, err
		}
		if role == "proxy" {
			proxyCommand = helper
		}
		return subprocessProcess{command: helper}, nil
	}
	t.Cleanup(func() {
		if proxyCommand != nil && proxyCommand.Process != nil {
			_ = proxyCommand.Process.Kill()
		}
	})

	err := Run(context.Background(), []string{"--model", "future-model:xhigh", "--", "-p", "task with spaces"}, options)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(spawned) != 2 {
		t.Fatalf("spawned %d commands, want proxy and Claude", len(spawned))
	}
	if !reflect.DeepEqual(spawned[0].Args, []string{"serve", "--port", strconv.Itoa(port)}) {
		t.Fatalf("proxy args = %#v", spawned[0].Args)
	}
	wantTarget := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	for _, target := range healthTargets {
		if target != wantTarget {
			t.Errorf("health target = %q, want %q", target, wantTarget)
		}
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("read Claude capture: %v", err)
	}
	var capture helperCapture
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatalf("decode Claude capture: %v", err)
	}
	if !reflect.DeepEqual(capture.Args, []string{"-p", "task with spaces"}) {
		t.Fatalf("Claude args = %#v", capture.Args)
	}
	if capture.Env["KEEP"] != "subprocess" || capture.Env["ANTHROPIC_MODEL"] != "anthropic-clodex-gpt-main:xhigh[1m]" ||
		capture.Env["ANTHROPIC_BASE_URL"] != fmt.Sprintf("http://127.0.0.1:%d", port) ||
		capture.Env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"] != "272000" ||
		capture.Env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] != "" {
		t.Fatalf("Claude env = %#v", capture.Env)
	}
	for _, key := range targetEnvironmentKeys {
		if count := envKeyCount(spawned[1].Env, key); count != 1 {
			t.Errorf("spawn environment key %s count = %d", key, count)
		}
	}
}

type staticResolver struct {
	resolution catalog.Resolution
	err        error
}

func (resolver staticResolver) Resolve(context.Context) (catalog.Resolution, error) {
	return resolver.resolution, resolver.err
}

type processStub struct {
	wait   chan error
	once   sync.Once
	killed atomic.Bool
	waited atomic.Bool
}

func newProcessStub() *processStub {
	return &processStub{wait: make(chan error, 1)}
}

func (process *processStub) Wait() error {
	process.waited.Store(true)
	return <-process.wait
}

func (process *processStub) Kill() error {
	process.killed.Store(true)
	process.complete(errors.New("killed"))
	return nil
}

func (process *processStub) complete(err error) {
	process.once.Do(func() { process.wait <- err })
}

type subprocessProcess struct {
	command *exec.Cmd
}

func (process subprocessProcess) Wait() error { return process.command.Wait() }
func (process subprocessProcess) Kill() error { return process.command.Process.Kill() }

type helperCapture struct {
	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`
}

func runLauncherHelper(t *testing.T) {
	role := os.Getenv("CLODEX_LAUNCHER_HELPER_ROLE")
	if role == "proxy" {
		port, err := strconv.Atoi(os.Getenv("CLODEX_LAUNCHER_HELPER_PORT"))
		if err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/healthz" {
				http.NotFound(writer, request)
				return
			}
			_ = json.NewEncoder(writer).Encode(healthy())
		})}
		time.AfterFunc(5*time.Second, func() { _ = server.Close() })
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
		return
	}
	if role != "claude" {
		t.Fatalf("unknown helper role %q", role)
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("CLODEX_LAUNCHER_HELPER_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	capture := helperCapture{Args: args, Env: map[string]string{}}
	for _, key := range append([]string{"KEEP", "CLAUDE_CODE_MAX_CONTEXT_TOKENS"}, targetEnvironmentKeys...) {
		capture.Env[key] = os.Getenv(key)
	}
	data, err := json.Marshal(capture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("CLODEX_LAUNCHER_HELPER_CAPTURE"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func testOptions(port int) Options {
	return Options{
		Port:           port,
		Model:          "gpt-main:medium",
		SmallFastModel: "gpt-small:low",
		ReadyTimeout:   200 * time.Millisecond,
		ProbeInterval:  time.Millisecond,
	}
}

func testDependencies() Dependencies {
	return Dependencies{
		Catalog: staticResolver{resolution: catalog.Resolution{Catalog: testCatalog(), Source: catalog.SourceFallback}},
		LookPath: func(name string) (string, error) {
			if name != "claude" {
				return "", fmt.Errorf("unexpected executable %q", name)
			}
			return "/test/claude", nil
		},
		Executable: func() (string, error) { return "/test/clodex", nil },
		Environ:    func() []string { return []string{"KEEP=default"} },
		Stdin:      strings.NewReader(""),
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}
}

func testCatalog() catalog.Catalog {
	return catalog.Catalog{Source: catalog.SourceFallback, Models: []catalog.Model{
		{
			Slug: "gpt-main", DefaultReasoningLevel: "medium",
			SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "low"}, {Effort: "medium"}, {Effort: "xhigh"}},
			ContextWindow:            272000, MaxContextWindow: 400000, EffectiveContextWindowPercent: 95,
			InputModalities: []string{"text"},
		},
		{
			Slug: "gpt-small", DefaultReasoningLevel: "low",
			SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "low"}, {Effort: "medium"}},
			ContextWindow:            128000, MaxContextWindow: 256000, EffectiveContextWindowPercent: 95,
			InputModalities: []string{"text"},
		},
	}}
}

func healthy() Health {
	return Health{Service: "clodex", Status: "ok", Version: "test-version"}
}

func assertEnv(t *testing.T, environment []string, want map[string]string) {
	t.Helper()
	got := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			got[key] = value
		}
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("environment %s = %q, want %q (%q)", key, got[key], value, environment)
		}
	}
}

func envKeyCount(environment []string, target string) int {
	count := 0
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, target) {
			count++
		}
	}
	return count
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

var targetEnvironmentKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
}

func environmentValue(environment []string, target string) (string, bool) {
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, target) {
			return value, true
		}
	}
	return "", false
}
