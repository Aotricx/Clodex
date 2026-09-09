// Command clodex exposes the standalone Clodex proxy, OAuth, and Claude Code
// launcher surfaces.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/Aotricx/Clodex/internal/app"
	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/commandauth"
	"github.com/Aotricx/Clodex/internal/config"
	"github.com/Aotricx/Clodex/internal/launcher"
	"github.com/Aotricx/Clodex/internal/notices"
	"github.com/Aotricx/Clodex/internal/oauth"
	"github.com/Aotricx/Clodex/internal/redact"
)

var version = "dev"

type cliDependencies struct {
	version    string
	stdout     io.Writer
	stderr     io.Writer
	lookupEnv  func(string) (string, bool)
	serve      func(context.Context, config.Config) error
	authLogin  func(context.Context, io.Writer) error
	authDevice func(context.Context, io.Writer) error
	authStatus func(context.Context, io.Writer) error
	authLogout func(context.Context, io.Writer) error
	claude     func(context.Context, []string, config.Config) error
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], defaultDependencies())
	if err == nil {
		return
	}
	os.Exit(handleRunError(os.Stderr, err))
}

func run(ctx context.Context, args []string, dependencies cliDependencies) error {
	if ctx == nil {
		return errors.New("nil command context")
	}
	if len(args) == 0 {
		return errors.New("command is required; run clodex help")
	}
	switch args[0] {
	case "serve":
		cfg, err := config.LoadServe(args[1:], dependencies.lookupEnv)
		if err != nil {
			if errors.Is(err, flag.ErrHelp) {
				_, writeErr := fmt.Fprintln(dependencies.stdout, err.Error())
				return writeErr
			}
			return err
		}
		return dependencies.serve(ctx, cfg)
	case "auth":
		if isHelpArg(args[1:]) {
			_, err := io.WriteString(dependencies.stdout, usage)
			return err
		}
		return runAuth(ctx, args[1:], dependencies)
	case "claude":
		if isHelpArg(args[1:]) {
			_, err := io.WriteString(dependencies.stdout, usage)
			return err
		}
		cfg, err := config.LoadServe(nil, dependencies.lookupEnv)
		if err != nil {
			return err
		}
		return dependencies.claude(ctx, args[1:], cfg)
	case "version":
		if len(args) != 1 {
			return errors.New("version takes no arguments")
		}
		_, err := fmt.Fprintln(dependencies.stdout, dependencies.version)
		return err
	case "licenses":
		if len(args) != 1 {
			return errors.New("licenses takes no arguments")
		}
		_, err := io.WriteString(dependencies.stdout, notices.Text())
		return err
	case "help", "--help", "-h":
		if len(args) != 1 {
			return errors.New("help takes no arguments")
		}
		_, err := io.WriteString(dependencies.stdout, usage)
		return err
	default:
		return fmt.Errorf("unknown command %q; run clodex help", args[0])
	}
}

func runAuth(ctx context.Context, args []string, dependencies cliDependencies) error {
	if len(args) == 0 {
		return errors.New("auth command is required: login, device, status, or logout")
	}
	if len(args) != 1 {
		return fmt.Errorf("auth %s takes no arguments", args[0])
	}
	switch args[0] {
	case "login":
		return dependencies.authLogin(ctx, dependencies.stdout)
	case "device":
		return dependencies.authDevice(ctx, dependencies.stdout)
	case "status":
		return dependencies.authStatus(ctx, dependencies.stdout)
	case "logout":
		return dependencies.authLogout(ctx, dependencies.stdout)
	default:
		return fmt.Errorf("unknown auth command %q", args[0])
	}
}

func isHelpArg(args []string) bool {
	return len(args) == 1 && (args[0] == "--help" || args[0] == "-h")
}

func defaultDependencies() cliDependencies {
	return cliDependencies{
		version: version, stdout: os.Stdout, stderr: os.Stderr, lookupEnv: os.LookupEnv,
		serve: func(ctx context.Context, cfg config.Config) error {
			return app.Serve(ctx, app.ServeOptions{
				Config: cfg, Version: version, Stderr: os.Stderr,
				Ready: func(address net.Addr) {
					_, _ = fmt.Fprintf(os.Stderr, "clodex %s listening on http://%s\n", version, address.String())
				},
			})
		},
		authLogin: func(ctx context.Context, writer io.Writer) error {
			service, err := newAuthService(writer)
			if err != nil {
				return err
			}
			summary, err := service.BrowserLogin(ctx)
			if err != nil {
				return err
			}
			return writeAuthenticated(writer, summary)
		},
		authDevice: func(ctx context.Context, writer io.Writer) error {
			service, err := newAuthService(writer)
			if err != nil {
				return err
			}
			summary, err := service.DeviceLogin(ctx)
			if err != nil {
				return err
			}
			return writeAuthenticated(writer, summary)
		},
		authStatus: func(ctx context.Context, writer io.Writer) error {
			service, err := newAuthService(writer)
			if err != nil {
				return err
			}
			_, err = service.Status(ctx, commandauth.StatusText)
			return err
		},
		authLogout: func(ctx context.Context, writer io.Writer) error {
			service, err := newAuthService(writer)
			if err != nil {
				return err
			}
			if err := service.Logout(ctx); err != nil {
				return err
			}
			_, err = fmt.Fprintln(writer, "logged out")
			return err
		},
		claude: runClaude,
	}
}

func newAuthService(writer io.Writer) (*commandauth.Service, error) {
	path, err := commandauth.DefaultAuthPath()
	if err != nil {
		return nil, err
	}
	return &commandauth.Service{OAuth: &oauth.Client{}, Store: &auth.Store{Path: path}, Writer: writer}, nil
}

func requireCodexAuth(store *auth.Store) error {
	_, err := store.Read()
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clodex claude requires credentials in ~/.codex/auth.json; run clodex auth login or clodex auth device first: %w", err)
	}
	return err
}

func runClaude(ctx context.Context, args []string, cfg config.Config) error {
	paths, err := app.DefaultPaths(nil)
	if err != nil {
		return err
	}
	store := &auth.Store{Path: paths.Auth}
	if err := requireCodexAuth(store); err != nil {
		return err
	}
	coordinator := &auth.Coordinator{Store: store, OAuth: &oauth.Client{}}
	manager := &catalog.Manager{CachePath: paths.Catalog, Discovery: &catalog.DiscoveryClient{Auth: coordinator}}
	return launcher.Run(ctx, args, launcher.Options{
		Port: cfg.Port, Model: cfg.Model, SmallFastModel: cfg.SmallFastModel,
		Dependencies: launcher.Dependencies{Catalog: manager},
	})
}

func writeAuthenticated(writer io.Writer, summary auth.Summary) error {
	expiry := "unknown"
	if summary.Expiry != nil {
		expiry = summary.Expiry.UTC().Format(time.RFC3339)
	}
	_, err := fmt.Fprintf(writer, "authenticated source=%s account=%s plan=%s expiry=%s\n", summary.Source, summary.Account, summary.Plan, expiry)
	return err
}

func handleRunError(stderr io.Writer, err error) int {
	code := errorExitCode(err)
	if errors.Is(err, context.Canceled) {
		return code
	}
	_, _ = fmt.Fprintf(stderr, "clodex: %s\n", redact.Text(err.Error()))
	return code
}

func errorExitCode(err error) int {
	if errors.Is(err, context.Canceled) {
		return 130
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(interface {
			Signaled() bool
			Signal() syscall.Signal
		}); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		if exitError.ExitCode() >= 0 {
			return exitError.ExitCode()
		}
	}
	return 1
}

const usage = `Clodex runs GPT/Codex models inside Claude Code through a local proxy.

Usage:
  clodex serve [--port N] [--debug-wire]
  clodex auth login
  clodex auth device
  clodex auth status
  clodex auth logout
  clodex claude [--model MODEL] [--small-fast-model MODEL] [-- <claude args>]
  clodex version
  clodex licenses

The proxy always binds 127.0.0.1. Configuration also accepts CLODEX_PORT,
CLODEX_MODEL, CLODEX_SMALL_FAST_MODEL, CLODEX_DEBUG_WIRE, and retry variables.
`
