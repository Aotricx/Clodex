// Package launcher starts or reuses Clodex and runs the real Claude Code
// process against its loopback Anthropic-compatible endpoint.
package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/config"
	"github.com/Aotricx/Clodex/internal/model"
)

const (
	// DummyAuthToken is deliberately non-secret. Claude Code requires a token in
	// custom-base mode; Clodex authenticates separately with Codex credentials.
	DummyAuthToken = "clodex-loopback-dummy"

	// catalogReadySlack covers tokenizer/handler construction after a
	// worst-case live catalog Fetch (catalog.HTTPTimeout) so spawned serve
	// can fall back and bind instead of being SIGKILL'd at the Fetch deadline.
	catalogReadySlack    = 15 * time.Second
	defaultProbeInterval = 50 * time.Millisecond
)

var defaultReadyTimeout = catalog.HTTPTimeout + catalogReadySlack

var claudeEnvironmentKeys = []string{
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_SMALL_FAST_MODEL",
	"CLAUDE_CODE_AUTO_COMPACT_WINDOW",
}

var removedClaudeEnvironmentKeys = []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS"}

// Arguments contains launcher overrides and untouched Claude Code arguments.
type Arguments struct {
	Model          string
	SmallFastModel string
	ClaudeArgs     []string
}

// CatalogResolver is implemented by catalog.Manager.
type CatalogResolver interface {
	Resolve(context.Context) (catalog.Resolution, error)
}

// Command is one directly spawned executable. Args excludes argv[0].
type Command struct {
	Path               string
	Args               []string
	Env                []string
	Stdin              io.Reader
	Stdout             io.Writer
	Stderr             io.Writer
	DetachProcessGroup bool
}

// Process is the lifecycle surface required from a spawned child.
type Process interface {
	Wait() error
	Kill() error
}

// SpawnFunc starts one command. Proxy starts receive context.Background so a
// ready proxy may remain reusable; Claude starts receive the caller context.
type SpawnFunc func(context.Context, Command) (Process, error)

// Dependencies contains every external effect used by Run.
type Dependencies struct {
	Catalog    CatalogResolver
	LookPath   func(string) (string, error)
	Executable func() (string, error)
	Spawn      SpawnFunc
	Probe      ProbeFunc
	Environ    func() []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}

// Options supplies configured defaults and bounded proxy-start timing.
type Options struct {
	Port           int
	Model          string
	SmallFastModel string
	ReadyTimeout   time.Duration
	ProbeInterval  time.Duration
	Dependencies   Dependencies
}

// Parse consumes launcher flags only before --. Everything following -- is
// passed byte-for-byte as separate arguments to Claude Code.
func Parse(args []string, defaultModel, defaultSmallFastModel string) (Arguments, error) {
	parsed := Arguments{Model: defaultModel, SmallFastModel: defaultSmallFastModel}
	if strings.TrimSpace(parsed.Model) == "" {
		return Arguments{}, errors.New("parse claude launcher: default model must not be empty")
	}
	if strings.TrimSpace(parsed.SmallFastModel) == "" {
		return Arguments{}, errors.New("parse claude launcher: default small-fast model must not be empty")
	}

	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			parsed.ClaudeArgs = append([]string(nil), args[index+1:]...)
			return parsed, nil
		}
		if value, ok := strings.CutPrefix(argument, "--model="); ok {
			if value == "" {
				return Arguments{}, errors.New("parse claude launcher: --model must not be empty")
			}
			parsed.Model = value
			continue
		}
		if value, ok := strings.CutPrefix(argument, "--small-fast-model="); ok {
			if value == "" {
				return Arguments{}, errors.New("parse claude launcher: --small-fast-model must not be empty")
			}
			parsed.SmallFastModel = value
			continue
		}
		switch argument {
		case "--model":
			index++
			if index >= len(args) {
				return Arguments{}, errors.New("parse claude launcher: --model requires a value")
			}
			if args[index] == "" {
				return Arguments{}, errors.New("parse claude launcher: --model must not be empty")
			}
			parsed.Model = args[index]
		case "--small-fast-model":
			index++
			if index >= len(args) {
				return Arguments{}, errors.New("parse claude launcher: --small-fast-model requires a value")
			}
			if args[index] == "" {
				return Arguments{}, errors.New("parse claude launcher: --small-fast-model must not be empty")
			}
			parsed.SmallFastModel = args[index]
		default:
			return Arguments{}, fmt.Errorf("parse claude launcher: Claude arguments must follow --; unexpected %q", argument)
		}
	}
	return parsed, nil
}

// Run resolves launcher models, ensures a healthy loopback proxy, and waits for
// Claude Code. Child exit errors remain wrapped so callers can preserve exit
// codes and platform-specific signal information.
func Run(ctx context.Context, args []string, options Options) error {
	if ctx == nil {
		return errors.New("run Claude Code: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	options, err := normalizeOptions(options)
	if err != nil {
		return err
	}
	dependencies, err := normalizeDependencies(options.Dependencies)
	if err != nil {
		return err
	}
	parsed, err := Parse(args, options.Model, options.SmallFastModel)
	if err != nil {
		return err
	}

	resolution, err := dependencies.Catalog.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("run Claude Code: resolve model catalog: %w", err)
	}
	mainSelection, err := model.Resolve(resolution.Catalog, parsed.Model, options.Model, 0)
	if err != nil {
		return fmt.Errorf("run Claude Code: resolve main model: %w", err)
	}
	smallSelection, err := model.Resolve(resolution.Catalog, parsed.SmallFastModel, options.SmallFastModel, 0)
	if err != nil {
		return fmt.Errorf("run Claude Code: resolve small-fast model: %w", err)
	}
	if mainSelection.Model.ContextWindow <= 0 {
		return fmt.Errorf("run Claude Code: model %q has invalid context window %d", mainSelection.Model.Slug, mainSelection.Model.ContextWindow)
	}

	claudePath, err := dependencies.LookPath("claude")
	if err != nil {
		return fmt.Errorf("run Claude Code: find claude executable: %w", err)
	}
	if strings.TrimSpace(claudePath) == "" {
		return errors.New("run Claude Code: claude executable lookup returned an empty path")
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", options.Port)
	healthURL := baseURL + "/healthz"
	baseEnvironment := append([]string(nil), dependencies.Environ()...)
	// Claude Code caps auto-compaction to its inferred model window. Gateway
	// discovery only accepts claude-/anthropic- IDs, so a reversible [1m]
	// carrier avoids its unknown-model 200k fallback. AUTO_COMPACT then applies
	// the catalog's real context window; the obsolete MAX_CONTEXT override works
	// only when compaction is disabled and is deliberately removed.
	contextWindow := strconv.Itoa(mainSelection.Model.ContextWindow)
	claudeEnvironment := replaceEnvironment(baseEnvironment, map[string]string{
		"ANTHROPIC_BASE_URL":              baseURL,
		"ANTHROPIC_AUTH_TOKEN":            DummyAuthToken,
		"ANTHROPIC_MODEL":                 model.ClaudeCarrierID(mainSelection.CanonicalID()),
		"ANTHROPIC_SMALL_FAST_MODEL":      model.ClaudeCarrierID(smallSelection.CanonicalID()),
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW": contextWindow,
	})

	if err := ensureProxy(ctx, healthURL, baseEnvironment, options, dependencies); err != nil {
		return err
	}
	process, err := dependencies.Spawn(ctx, Command{
		Path: claudePath, Args: append([]string(nil), parsed.ClaudeArgs...), Env: claudeEnvironment,
		Stdin: dependencies.Stdin, Stdout: dependencies.Stdout, Stderr: dependencies.Stderr,
	})
	if err != nil {
		return fmt.Errorf("run Claude Code: start claude: %w", err)
	}
	if process == nil {
		return errors.New("run Claude Code: start claude returned a nil process")
	}
	if err := process.Wait(); err != nil {
		return fmt.Errorf("run Claude Code: claude exited: %w", err)
	}
	return nil
}

func normalizeOptions(options Options) (Options, error) {
	defaults := config.Defaults()
	if options.Port == 0 {
		options.Port = defaults.Port
	}
	if options.Model == "" {
		options.Model = defaults.Model
	}
	if options.SmallFastModel == "" {
		options.SmallFastModel = defaults.SmallFastModel
	}
	if options.ReadyTimeout == 0 {
		options.ReadyTimeout = defaultReadyTimeout
	}
	if options.ProbeInterval == 0 {
		options.ProbeInterval = defaultProbeInterval
	}
	if options.Port < 1 || options.Port > 65535 {
		return Options{}, fmt.Errorf("run Claude Code: port must be between 1 and 65535, got %d", options.Port)
	}
	if options.ReadyTimeout < 0 {
		return Options{}, errors.New("run Claude Code: readiness timeout must be positive")
	}
	if options.ProbeInterval < 0 {
		return Options{}, errors.New("run Claude Code: probe interval must be positive")
	}
	return options, nil
}

func normalizeDependencies(dependencies Dependencies) (Dependencies, error) {
	if dependencies.Catalog == nil {
		return Dependencies{}, errors.New("run Claude Code: catalog resolver is required")
	}
	if dependencies.LookPath == nil {
		dependencies.LookPath = exec.LookPath
	}
	if dependencies.Executable == nil {
		dependencies.Executable = os.Executable
	}
	if dependencies.Spawn == nil {
		dependencies.Spawn = spawnCommand
	}
	if dependencies.Probe == nil {
		dependencies.Probe = ProbeHealth
	}
	if dependencies.Environ == nil {
		dependencies.Environ = os.Environ
	}
	if dependencies.Stdin == nil {
		dependencies.Stdin = os.Stdin
	}
	if dependencies.Stdout == nil {
		dependencies.Stdout = os.Stdout
	}
	if dependencies.Stderr == nil {
		dependencies.Stderr = os.Stderr
	}
	return dependencies, nil
}

func replaceEnvironment(environment []string, overrides map[string]string) []string {
	result := make([]string, 0, len(environment)+len(claudeEnvironmentKeys))
	for _, entry := range environment {
		key, _, found := strings.Cut(entry, "=")
		if found && isClaudeEnvironmentKey(key) {
			continue
		}
		result = append(result, entry)
	}
	for _, key := range claudeEnvironmentKeys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func isClaudeEnvironmentKey(key string) bool {
	for _, target := range append(claudeEnvironmentKeys, removedClaudeEnvironmentKeys...) {
		if strings.EqualFold(key, target) {
			return true
		}
	}
	return false
}

type commandProcess struct {
	command *exec.Cmd
}

func spawnCommand(ctx context.Context, command Command) (Process, error) {
	if ctx == nil {
		return nil, errors.New("spawn command: nil context")
	}
	if strings.TrimSpace(command.Path) == "" {
		return nil, errors.New("spawn command: executable path is required")
	}
	child := exec.CommandContext(ctx, command.Path, command.Args...)
	child.Env = append([]string(nil), command.Env...)
	child.Stdin, child.Stdout, child.Stderr = command.Stdin, command.Stdout, command.Stderr
	child.SysProcAttr = processGroupSysProcAttr(command.DetachProcessGroup)
	if err := child.Start(); err != nil {
		return nil, err
	}
	return commandProcess{command: child}, nil
}

func (process commandProcess) Wait() error {
	return process.command.Wait()
}

func (process commandProcess) Kill() error {
	return process.command.Process.Kill()
}
