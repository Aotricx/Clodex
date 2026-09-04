// Package config loads and validates Clodex runtime configuration.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is the complete configuration for the Clodex proxy.
type Config struct {
	Port                    int
	Model                   string
	SmallFastModel          string
	DebugWire               bool
	EmptyCompletionRetries  int
	TransientRetries        int
	RetryBaseDelay          time.Duration
	RetryMaxDelay           time.Duration
	GlobalRetryBudget       int
	RetryBudgetWindow       time.Duration
	CircuitFailureThreshold int
	CircuitCooldown         time.Duration
}

// Defaults returns the default Clodex configuration.
func Defaults() Config {
	return Config{
		Port:                    8484,
		Model:                   "gpt-5.6-sol:medium",
		SmallFastModel:          "gpt-5.4-mini:low",
		DebugWire:               false,
		EmptyCompletionRetries:  10,
		TransientRetries:        3,
		RetryBaseDelay:          250 * time.Millisecond,
		RetryMaxDelay:           5 * time.Second,
		GlobalRetryBudget:       100,
		RetryBudgetWindow:       time.Minute,
		CircuitFailureThreshold: 8,
		CircuitCooldown:         30 * time.Second,
	}
}

// LoadServe loads serve configuration with command-line values taking
// precedence over environment values and defaults.
func LoadServe(args []string, lookupEnv func(string) (string, bool)) (Config, error) {
	cfg := Defaults()
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}

	if value, ok, err := envInt(lookupEnv, "CLODEX_PORT"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.Port = value
	}
	if value, ok := lookupEnv("CLODEX_MODEL"); ok {
		cfg.Model = strings.TrimSpace(value)
		if cfg.Model == "" {
			return Config{}, fmt.Errorf("CLODEX_MODEL: model must not be empty")
		}
	}
	if value, ok := lookupEnv("CLODEX_SMALL_FAST_MODEL"); ok {
		cfg.SmallFastModel = strings.TrimSpace(value)
		if cfg.SmallFastModel == "" {
			return Config{}, fmt.Errorf("CLODEX_SMALL_FAST_MODEL: small fast model must not be empty")
		}
	}
	if value, ok, err := envBool(lookupEnv, "CLODEX_DEBUG_WIRE"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.DebugWire = value
	}
	if value, ok, err := envInt(lookupEnv, "CLODEX_EMPTY_RETRIES"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.EmptyCompletionRetries = value
	}
	if value, ok, err := envInt(lookupEnv, "CLODEX_TRANSIENT_RETRIES"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.TransientRetries = value
	}
	if value, ok, err := envDuration(lookupEnv, "CLODEX_RETRY_BASE_DELAY"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.RetryBaseDelay = value
	}
	if value, ok, err := envDuration(lookupEnv, "CLODEX_RETRY_MAX_DELAY"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.RetryMaxDelay = value
	}
	if value, ok, err := envInt(lookupEnv, "CLODEX_GLOBAL_RETRY_BUDGET"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.GlobalRetryBudget = value
	}
	if value, ok, err := envDuration(lookupEnv, "CLODEX_RETRY_BUDGET_WINDOW"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.RetryBudgetWindow = value
	}
	if value, ok, err := envInt(lookupEnv, "CLODEX_CIRCUIT_FAILURES"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.CircuitFailureThreshold = value
	}
	if value, ok, err := envDuration(lookupEnv, "CLODEX_CIRCUIT_COOLDOWN"); err != nil {
		return Config{}, err
	} else if ok {
		cfg.CircuitCooldown = value
	}

	var usage bytes.Buffer
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(&usage)
	flags.IntVar(&cfg.Port, "port", cfg.Port, "loopback listen port")
	flags.BoolVar(&cfg.DebugWire, "debug-wire", cfg.DebugWire, "log wire traffic")
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: clodex serve [--port N] [--debug-wire]\n")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return Config{}, &serveHelpError{usage: strings.TrimSpace(usage.String())}
		}
		return Config{}, fmt.Errorf("serve arguments: %w", err)
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// ListenAddr returns the fixed loopback listen address.
func (cfg Config) ListenAddr() string {
	return fmt.Sprintf("127.0.0.1:%d", cfg.Port)
}

type serveHelpError struct {
	usage string
}

func (e *serveHelpError) Error() string {
	if e == nil || e.usage == "" {
		return flag.ErrHelp.Error()
	}
	return e.usage
}

func (e *serveHelpError) Unwrap() error {
	return flag.ErrHelp
}

func envInt(lookupEnv func(string) (string, bool), key string) (int, bool, error) {
	value, ok := lookupEnv(key)
	if !ok {
		return 0, false, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, false, fmt.Errorf("%s: parse integer: %w", key, err)
	}
	return parsed, true, nil
}

func envBool(lookupEnv func(string) (string, bool), key string) (bool, bool, error) {
	value, ok := lookupEnv(key)
	if !ok {
		return false, false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, false, fmt.Errorf("%s: parse boolean: %w", key, err)
	}
	return parsed, true, nil
}

func envDuration(lookupEnv func(string) (string, bool), key string) (time.Duration, bool, error) {
	value, ok := lookupEnv(key)
	if !ok {
		return 0, false, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, false, fmt.Errorf("%s: parse duration: %w", key, err)
	}
	return parsed, true, nil
}

func (cfg Config) validate() error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", cfg.Port)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return fmt.Errorf("model must not be empty")
	}
	if strings.TrimSpace(cfg.SmallFastModel) == "" {
		return fmt.Errorf("small fast model must not be empty")
	}
	if cfg.EmptyCompletionRetries < 0 || cfg.EmptyCompletionRetries > 100 {
		return fmt.Errorf("empty completion retries must be between 0 and 100, got %d", cfg.EmptyCompletionRetries)
	}
	if cfg.TransientRetries < 0 || cfg.TransientRetries > 10 {
		return fmt.Errorf("transient retries must be between 0 and 10, got %d", cfg.TransientRetries)
	}
	if cfg.RetryBaseDelay <= 0 {
		return fmt.Errorf("retry base delay must be greater than zero, got %s", cfg.RetryBaseDelay)
	}
	if cfg.RetryBaseDelay > cfg.RetryMaxDelay {
		return fmt.Errorf("retry base delay %s must not exceed retry max delay %s", cfg.RetryBaseDelay, cfg.RetryMaxDelay)
	}
	if cfg.RetryMaxDelay > time.Minute {
		return fmt.Errorf("retry max delay must not exceed 1m, got %s", cfg.RetryMaxDelay)
	}
	if cfg.GlobalRetryBudget < 1 || cfg.GlobalRetryBudget > 10000 {
		return fmt.Errorf("global retry budget must be between 1 and 10000, got %d", cfg.GlobalRetryBudget)
	}
	if cfg.RetryBudgetWindow < time.Second || cfg.RetryBudgetWindow > time.Hour {
		return fmt.Errorf("retry budget window must be between 1s and 1h, got %s", cfg.RetryBudgetWindow)
	}
	if cfg.CircuitFailureThreshold < 1 || cfg.CircuitFailureThreshold > 100 {
		return fmt.Errorf("circuit failure threshold must be between 1 and 100, got %d", cfg.CircuitFailureThreshold)
	}
	if cfg.CircuitCooldown < time.Second || cfg.CircuitCooldown > 10*time.Minute {
		return fmt.Errorf("circuit cooldown must be between 1s and 10m, got %s", cfg.CircuitCooldown)
	}
	return nil
}
