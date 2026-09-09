// Package app composes Clodex's concrete single-backend runtime.
package app

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/config"
	"github.com/Aotricx/Clodex/internal/engine"
	"github.com/Aotricx/Clodex/internal/model"
	"github.com/Aotricx/Clodex/internal/oauth"
	"github.com/Aotricx/Clodex/internal/redact"
	"github.com/Aotricx/Clodex/internal/retry"
	"github.com/Aotricx/Clodex/internal/server"
	clodexstatus "github.com/Aotricx/Clodex/internal/status"
	"github.com/Aotricx/Clodex/internal/tokenizer"
	"github.com/Aotricx/Clodex/internal/upstream"
)

// Paths are Clodex's interoperable persistent files.
type Paths struct {
	Auth    string
	Catalog string
	Dump    string
}

// DefaultPaths resolves Codex's shared auth/catalog locations and Clodex's
// private diagnostic directory without trusting a mutable HOME variable.
// When CODEX_HOME is a non-empty directory, auth.json and models_cache.json
// live directly under it. The dump directory still uses the user home.
func DefaultPaths(userHomeDir func() (string, error)) (Paths, error) {
	if userHomeDir == nil {
		userHomeDir = os.UserHomeDir
	}
	home, err := userHomeDir()
	if err != nil {
		return Paths{}, fmt.Errorf("resolve Clodex home directory: %w", err)
	}
	if home == "" {
		return Paths{}, errors.New("resolve Clodex home directory: empty path")
	}
	dump := filepath.Join(home, ".clodex", "wire")
	if codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME")); codexHome != "" {
		return Paths{
			Auth: filepath.Join(codexHome, "auth.json"), Catalog: filepath.Join(codexHome, "models_cache.json"),
			Dump: dump,
		}, nil
	}
	return Paths{
		Auth: filepath.Join(home, ".codex", "auth.json"), Catalog: filepath.Join(home, ".codex", "models_cache.json"),
		Dump: dump,
	}, nil
}

// ServeOptions controls one composed proxy process.
type ServeOptions struct {
	Config      config.Config
	Version     string
	AuthPath    string
	CatalogPath string
	DumpDir     string
	HomeDir     func() (string, error)
	Stderr      io.Writer
	Ready       func(net.Addr)
	// Entropy supplies retry jitter. Serve probes it once at start and fails
	// closed if a value cannot be read. Nil uses crypto/rand.
	Entropy io.Reader
}

// Serve composes OAuth, catalog discovery, translation, transport, retry,
// status, and the loopback HTTP server.
func Serve(ctx context.Context, options ServeOptions) error {
	if ctx == nil {
		return errors.New("start Clodex: nil context")
	}
	if options.Version == "" {
		return errors.New("start Clodex: version is required")
	}
	var paths Paths
	if options.AuthPath != "" && options.CatalogPath != "" && options.DumpDir != "" {
		paths = Paths{Auth: options.AuthPath, Catalog: options.CatalogPath, Dump: options.DumpDir}
	} else {
		resolved, err := DefaultPaths(options.HomeDir)
		if err != nil {
			return err
		}
		paths = resolved
		if options.AuthPath != "" {
			paths.Auth = options.AuthPath
		}
		if options.CatalogPath != "" {
			paths.Catalog = options.CatalogPath
		}
		if options.DumpDir != "" {
			paths.Dump = options.DumpDir
		}
	}
	stderr := options.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	store := &auth.Store{Path: paths.Auth}
	state := clodexstatus.New(options.Version)
	if err := refreshAuthStatus(store, state); err != nil {
		return fmt.Errorf("start Clodex: %w", err)
	}

	oauthClient := &oauth.Client{}
	coordinator := &auth.Coordinator{Store: store, OAuth: oauthClient}
	discovery := &catalog.DiscoveryClient{Auth: coordinator}
	catalogManager := &catalog.Manager{CachePath: paths.Catalog, Discovery: discovery}
	resolution, err := catalogManager.Resolve(ctx)
	if err != nil {
		return fmt.Errorf("start Clodex: resolve model catalog: %w", err)
	}
	state.SetCatalog(string(resolution.Source), resolution.Catalog.FetchedAt)
	if _, err := model.Resolve(resolution.Catalog, options.Config.Model, options.Config.Model, 0); err != nil {
		return fmt.Errorf("start Clodex: default model: %w", err)
	}
	if _, err := model.Resolve(resolution.Catalog, options.Config.SmallFastModel, options.Config.SmallFastModel, 0); err != nil {
		return fmt.Errorf("start Clodex: small fast model: %w", err)
	}

	counter, err := tokenizer.New()
	if err != nil {
		return fmt.Errorf("start Clodex: tokenizer: %w", err)
	}
	entropy := options.Entropy
	if entropy == nil {
		entropy = cryptorand.Reader
	}
	if _, err := cryptoFloat64(entropy); err != nil {
		return fmt.Errorf("start Clodex: random source: %w", err)
	}
	retryController, err := retry.NewReal(retry.Config{
		BaseDelay: options.Config.RetryBaseDelay, MaxDelay: options.Config.RetryMaxDelay,
		RetryAfterLimit: 5 * time.Minute, Budget: options.Config.GlobalRetryBudget,
		BudgetWindow: options.Config.RetryBudgetWindow, FailureThreshold: options.Config.CircuitFailureThreshold,
		CircuitCooldown: options.Config.CircuitCooldown,
	}, func() float64 {
		value, err := cryptoFloat64(entropy)
		if err != nil {
			// Mid-process entropy failure cannot abort an in-flight retry
			// delay; 0.5 is equal-jitter's midpoint. Serve already failed
			// closed if the start probe could not read entropy.
			return 0.5
		}
		return value
	})
	if err != nil {
		return fmt.Errorf("start Clodex: retry controller: %w", err)
	}
	syncRetryStatus(state, retryController.Snapshot())

	transport := &upstream.Client{
		Auth: coordinator, DumpDir: paths.Dump, DebugWire: options.Config.DebugWire,
		OnDumpError: func(err error) {
			_, _ = fmt.Fprintf(stderr, "clodex: diagnostic dump failed: %s\n", redact.Text(err.Error()))
		},
	}
	emptyRetries := options.Config.EmptyCompletionRetries
	transientRetries := options.Config.TransientRetries
	turnEngine := &engine.Engine{
		Transport: transport, Retry: retryController,
		MaxEmptyRetries: &emptyRetries, MaxTransientRetries: &transientRetries,
		Dump: func(trigger engine.DumpTrigger) {
			state.RecordRetry(string(trigger.Reason))
			syncRetryStatus(state, retryController.Snapshot())
		},
		RateLimit: state.SetRateLimit,
	}
	messages, err := server.NewMessages(server.MessagesOptions{
		Catalog: catalogManager, Status: state, DefaultModel: options.Config.Model, Engine: turnEngine,
	})
	if err != nil {
		return fmt.Errorf("start Clodex: messages service: %w", err)
	}
	handler, err := server.New(server.Options{
		Version: options.Version, Status: state, Catalog: catalogManager, Counter: counter,
		DefaultModel: options.Config.Model, Messages: messages,
		BeforeStatus: func(ctx context.Context) error {
			return refreshHTTPStatus(ctx, store, state, retryController)
		},
	})
	if err != nil {
		return fmt.Errorf("start Clodex: HTTP handler: %w", err)
	}
	return server.Serve(ctx, options.Config.Port, handler, options.Ready)
}

func refreshHTTPStatus(ctx context.Context, store *auth.Store, state *clodexstatus.State, retryController *retry.Controller) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := refreshAuthStatus(store, state); err != nil {
		return err
	}
	if retryController != nil {
		syncRetryStatus(state, retryController.Snapshot())
	}
	return nil
}

func refreshAuthStatus(store *auth.Store, state *clodexstatus.State) error {
	file, err := store.Read()
	if errors.Is(err, os.ErrNotExist) {
		state.SetAuth(clodexstatus.AuthSummary{Source: "none", Account: "unknown", Plan: "Unknown"})
		return nil
	}
	if err != nil {
		return fmt.Errorf("read auth: %w", err)
	}
	summary, err := file.Summary()
	if err != nil {
		return fmt.Errorf("summarize auth: %w", err)
	}
	state.SetAuth(clodexstatus.AuthSummary{Source: summary.Source, Account: summary.Account, Plan: summary.Plan, Expiry: summary.Expiry})
	return nil
}

func syncRetryStatus(state *clodexstatus.State, snapshot retry.Snapshot) {
	state.SetRemainingRetryBudget(int64(snapshot.RemainingBudget))
	state.SetCircuit(clodexstatus.CircuitState{
		Open: snapshot.State == retry.StateOpen, Failures: int64(snapshot.Failures), OpenUntil: snapshot.OpenUntil,
	})
}

func cryptoFloat64(entropy io.Reader) (float64, error) {
	if entropy == nil {
		entropy = cryptorand.Reader
	}
	var buffer [8]byte
	if _, err := io.ReadFull(entropy, buffer[:]); err != nil {
		return 0, err
	}
	const mantissaBits = 53
	value := binary.LittleEndian.Uint64(buffer[:]) >> (64 - mantissaBits)
	return float64(value) / float64(uint64(1)<<mantissaBits), nil
}
