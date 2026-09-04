package config

import (
	"errors"
	"flag"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadServeDefaults(t *testing.T) {
	want := Config{
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

	if got := Defaults(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Defaults() = %#v, want %#v", got, want)
	}

	got, err := LoadServe(nil, envLookup(nil))
	if err != nil {
		t.Fatalf("LoadServe() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadServe() = %#v, want %#v", got, want)
	}
}

func TestLoadServeEnvironmentOverrides(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  func(*Config)
	}{
		{name: "port integer", key: "CLODEX_PORT", value: "9494", want: func(c *Config) { c.Port = 9494 }},
		{name: "model string is trimmed", key: "CLODEX_MODEL", value: "  gpt-custom:high  ", want: func(c *Config) { c.Model = "gpt-custom:high" }},
		{name: "small model string is trimmed", key: "CLODEX_SMALL_FAST_MODEL", value: "  gpt-small:low  ", want: func(c *Config) { c.SmallFastModel = "gpt-small:low" }},
		{name: "debug boolean", key: "CLODEX_DEBUG_WIRE", value: "TRUE", want: func(c *Config) { c.DebugWire = true }},
		{name: "empty retries integer", key: "CLODEX_EMPTY_RETRIES", value: "12", want: func(c *Config) { c.EmptyCompletionRetries = 12 }},
		{name: "transient retries integer", key: "CLODEX_TRANSIENT_RETRIES", value: "4", want: func(c *Config) { c.TransientRetries = 4 }},
		{name: "retry base duration", key: "CLODEX_RETRY_BASE_DELAY", value: "375ms", want: func(c *Config) { c.RetryBaseDelay = 375 * time.Millisecond }},
		{name: "retry max duration", key: "CLODEX_RETRY_MAX_DELAY", value: "7s", want: func(c *Config) { c.RetryMaxDelay = 7 * time.Second }},
		{name: "global retry budget integer", key: "CLODEX_GLOBAL_RETRY_BUDGET", value: "250", want: func(c *Config) { c.GlobalRetryBudget = 250 }},
		{name: "retry budget window duration", key: "CLODEX_RETRY_BUDGET_WINDOW", value: "90s", want: func(c *Config) { c.RetryBudgetWindow = 90 * time.Second }},
		{name: "circuit failures integer", key: "CLODEX_CIRCUIT_FAILURES", value: "12", want: func(c *Config) { c.CircuitFailureThreshold = 12 }},
		{name: "circuit cooldown duration", key: "CLODEX_CIRCUIT_COOLDOWN", value: "45s", want: func(c *Config) { c.CircuitCooldown = 45 * time.Second }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := Defaults()
			tt.want(&want)
			got, err := LoadServe(nil, envLookup(map[string]string{tt.key: tt.value}))
			if err != nil {
				t.Fatalf("LoadServe() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("LoadServe() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestLoadServeCLIOverridesEnvironment(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want func(*Config)
	}{
		{
			name: "port",
			args: []string{"--port=9292"},
			env:  map[string]string{"CLODEX_PORT": "9191"},
			want: func(c *Config) { c.Port = 9292 },
		},
		{
			name: "explicit false boolean",
			args: []string{"--debug-wire=false"},
			env:  map[string]string{"CLODEX_DEBUG_WIRE": "true"},
			want: func(c *Config) { c.DebugWire = false },
		},
		{
			name: "bare true boolean",
			args: []string{"--debug-wire"},
			env:  map[string]string{"CLODEX_DEBUG_WIRE": "false"},
			want: func(c *Config) { c.DebugWire = true },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := Defaults()
			tt.want(&want)
			got, err := LoadServe(tt.args, envLookup(tt.env))
			if err != nil {
				t.Fatalf("LoadServe() error = %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("LoadServe() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestLoadServeAlwaysListensOnLoopback(t *testing.T) {
	cfg, err := LoadServe([]string{"--port", "4321"}, envLookup(nil))
	if err != nil {
		t.Fatalf("LoadServe() error = %v", err)
	}
	if got, want := cfg.ListenAddr(), "127.0.0.1:4321"; got != want {
		t.Fatalf("ListenAddr() = %q, want %q", got, want)
	}
}

func TestLoadServeAcceptsValidationBoundaries(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "lower bounds",
			env: map[string]string{
				"CLODEX_PORT":                "1",
				"CLODEX_MODEL":               "m",
				"CLODEX_SMALL_FAST_MODEL":    "s",
				"CLODEX_EMPTY_RETRIES":       "0",
				"CLODEX_TRANSIENT_RETRIES":   "0",
				"CLODEX_RETRY_BASE_DELAY":    "1ns",
				"CLODEX_RETRY_MAX_DELAY":     "1ns",
				"CLODEX_GLOBAL_RETRY_BUDGET": "1",
				"CLODEX_RETRY_BUDGET_WINDOW": "1s",
				"CLODEX_CIRCUIT_FAILURES":    "1",
				"CLODEX_CIRCUIT_COOLDOWN":    "1s",
			},
		},
		{
			name: "upper bounds",
			env: map[string]string{
				"CLODEX_PORT":                "65535",
				"CLODEX_EMPTY_RETRIES":       "100",
				"CLODEX_TRANSIENT_RETRIES":   "10",
				"CLODEX_RETRY_BASE_DELAY":    "1m",
				"CLODEX_RETRY_MAX_DELAY":     "1m",
				"CLODEX_GLOBAL_RETRY_BUDGET": "10000",
				"CLODEX_RETRY_BUDGET_WINDOW": "1h",
				"CLODEX_CIRCUIT_FAILURES":    "100",
				"CLODEX_CIRCUIT_COOLDOWN":    "10m",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := LoadServe(nil, envLookup(tt.env)); err != nil {
				t.Fatalf("LoadServe() error = %v", err)
			}
		})
	}
}

func TestLoadServeRejectsInvalidEnvironment(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{name: "port not integer", env: map[string]string{"CLODEX_PORT": "nope"}, wantErr: "CLODEX_PORT"},
		{name: "port below range", env: map[string]string{"CLODEX_PORT": "0"}, wantErr: "port"},
		{name: "port above range", env: map[string]string{"CLODEX_PORT": "65536"}, wantErr: "port"},
		{name: "model empty", env: map[string]string{"CLODEX_MODEL": ""}, wantErr: "CLODEX_MODEL"},
		{name: "model whitespace", env: map[string]string{"CLODEX_MODEL": " \t "}, wantErr: "model"},
		{name: "small model empty", env: map[string]string{"CLODEX_SMALL_FAST_MODEL": ""}, wantErr: "CLODEX_SMALL_FAST_MODEL"},
		{name: "small model whitespace", env: map[string]string{"CLODEX_SMALL_FAST_MODEL": " \t "}, wantErr: "small fast model"},
		{name: "debug invalid", env: map[string]string{"CLODEX_DEBUG_WIRE": "sometimes"}, wantErr: "CLODEX_DEBUG_WIRE"},
		{name: "empty retries not integer", env: map[string]string{"CLODEX_EMPTY_RETRIES": "many"}, wantErr: "CLODEX_EMPTY_RETRIES"},
		{name: "empty retries below range", env: map[string]string{"CLODEX_EMPTY_RETRIES": "-1"}, wantErr: "empty completion retries"},
		{name: "empty retries above range", env: map[string]string{"CLODEX_EMPTY_RETRIES": "101"}, wantErr: "empty completion retries"},
		{name: "transient retries not integer", env: map[string]string{"CLODEX_TRANSIENT_RETRIES": "many"}, wantErr: "CLODEX_TRANSIENT_RETRIES"},
		{name: "transient retries below range", env: map[string]string{"CLODEX_TRANSIENT_RETRIES": "-1"}, wantErr: "transient retries"},
		{name: "transient retries above range", env: map[string]string{"CLODEX_TRANSIENT_RETRIES": "11"}, wantErr: "transient retries"},
		{name: "base delay invalid", env: map[string]string{"CLODEX_RETRY_BASE_DELAY": "soon"}, wantErr: "CLODEX_RETRY_BASE_DELAY"},
		{name: "base delay zero", env: map[string]string{"CLODEX_RETRY_BASE_DELAY": "0s"}, wantErr: "retry base delay"},
		{name: "base delay negative", env: map[string]string{"CLODEX_RETRY_BASE_DELAY": "-1s"}, wantErr: "retry base delay"},
		{name: "base delay exceeds max", env: map[string]string{"CLODEX_RETRY_BASE_DELAY": "6s"}, wantErr: "retry base delay"},
		{name: "max delay invalid", env: map[string]string{"CLODEX_RETRY_MAX_DELAY": "later"}, wantErr: "CLODEX_RETRY_MAX_DELAY"},
		{name: "max delay below base", env: map[string]string{"CLODEX_RETRY_MAX_DELAY": "249ms"}, wantErr: "retry base delay"},
		{name: "max delay above range", env: map[string]string{"CLODEX_RETRY_MAX_DELAY": "1m1ns"}, wantErr: "retry max delay"},
		{name: "global budget not integer", env: map[string]string{"CLODEX_GLOBAL_RETRY_BUDGET": "many"}, wantErr: "CLODEX_GLOBAL_RETRY_BUDGET"},
		{name: "global budget below range", env: map[string]string{"CLODEX_GLOBAL_RETRY_BUDGET": "0"}, wantErr: "global retry budget"},
		{name: "global budget above range", env: map[string]string{"CLODEX_GLOBAL_RETRY_BUDGET": "10001"}, wantErr: "global retry budget"},
		{name: "budget window invalid", env: map[string]string{"CLODEX_RETRY_BUDGET_WINDOW": "later"}, wantErr: "CLODEX_RETRY_BUDGET_WINDOW"},
		{name: "budget window below range", env: map[string]string{"CLODEX_RETRY_BUDGET_WINDOW": "999ms"}, wantErr: "retry budget window"},
		{name: "budget window above range", env: map[string]string{"CLODEX_RETRY_BUDGET_WINDOW": "1h1ns"}, wantErr: "retry budget window"},
		{name: "circuit failures not integer", env: map[string]string{"CLODEX_CIRCUIT_FAILURES": "many"}, wantErr: "CLODEX_CIRCUIT_FAILURES"},
		{name: "circuit failures below range", env: map[string]string{"CLODEX_CIRCUIT_FAILURES": "0"}, wantErr: "circuit failure threshold"},
		{name: "circuit failures above range", env: map[string]string{"CLODEX_CIRCUIT_FAILURES": "101"}, wantErr: "circuit failure threshold"},
		{name: "circuit cooldown invalid", env: map[string]string{"CLODEX_CIRCUIT_COOLDOWN": "later"}, wantErr: "CLODEX_CIRCUIT_COOLDOWN"},
		{name: "circuit cooldown below range", env: map[string]string{"CLODEX_CIRCUIT_COOLDOWN": "999ms"}, wantErr: "circuit cooldown"},
		{name: "circuit cooldown above range", env: map[string]string{"CLODEX_CIRCUIT_COOLDOWN": "10m1ns"}, wantErr: "circuit cooldown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadServe(nil, envLookup(tt.env))
			if err == nil {
				t.Fatal("LoadServe() error = nil, want error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErr)) {
				t.Fatalf("LoadServe() error = %q, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadServeRejectsEverySetButEmptyEnvironmentValue(t *testing.T) {
	keys := []string{
		"CLODEX_PORT",
		"CLODEX_MODEL",
		"CLODEX_SMALL_FAST_MODEL",
		"CLODEX_DEBUG_WIRE",
		"CLODEX_EMPTY_RETRIES",
		"CLODEX_TRANSIENT_RETRIES",
		"CLODEX_RETRY_BASE_DELAY",
		"CLODEX_RETRY_MAX_DELAY",
		"CLODEX_GLOBAL_RETRY_BUDGET",
		"CLODEX_RETRY_BUDGET_WINDOW",
		"CLODEX_CIRCUIT_FAILURES",
		"CLODEX_CIRCUIT_COOLDOWN",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			_, err := LoadServe(nil, envLookup(map[string]string{key: ""}))
			if err == nil {
				t.Fatal("LoadServe() error = nil, want error")
			}
			if !strings.Contains(err.Error(), key) {
				t.Fatalf("LoadServe() error = %q, want it to name %q", err, key)
			}
		})
	}
}

func TestLoadServeHelpUnwrapsFlagErrHelpAndIncludesUsage(t *testing.T) {
	_, err := LoadServe([]string{"--help"}, envLookup(nil))
	if err == nil {
		t.Fatal("LoadServe(--help) error = nil, want flag.ErrHelp")
	}
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("LoadServe(--help) error = %v, want errors.Is flag.ErrHelp", err)
	}
	if !strings.Contains(err.Error(), "--port") {
		t.Fatalf("LoadServe(--help) error = %q, want usage mentioning --port", err)
	}
}

func TestLoadServeRejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "unknown flag", args: []string{"--bind", "0.0.0.0"}, wantErr: "bind"},
		{name: "positional argument", args: []string{"surprise"}, wantErr: "positional"},
		{name: "port not integer", args: []string{"--port", "nope"}, wantErr: "port"},
		{name: "port below range", args: []string{"--port", "0"}, wantErr: "port"},
		{name: "port above range", args: []string{"--port", "65536"}, wantErr: "port"},
		{name: "debug invalid", args: []string{"--debug-wire=maybe"}, wantErr: "debug-wire"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadServe(tt.args, envLookup(nil))
			if err == nil {
				t.Fatal("LoadServe() error = nil, want error")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tt.wantErr)) {
				t.Fatalf("LoadServe() error = %q, want it to name %q", err, tt.wantErr)
			}
		})
	}
}

func envLookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
