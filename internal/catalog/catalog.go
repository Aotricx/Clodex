package catalog

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	BackendURL    = "https://chatgpt.com/backend-api/codex"
	ClientVersion = "0.144.6"
	DefaultTTL    = 5 * time.Minute

	// clockSkewTolerance permits small differences between local and server clocks.
	clockSkewTolerance                   = 30 * time.Second
	defaultEffectiveContextWindowPercent = 95
)

type Source string

const (
	SourceLive     Source = "live"
	SourceFallback Source = "fallback"
	SourceCache    Source = "cache"
)

type ReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description,omitempty"`
}

type ServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

type Model struct {
	Slug                          string           `json:"slug"`
	DisplayName                   string           `json:"display_name,omitempty"`
	Description                   string           `json:"description,omitempty"`
	DefaultReasoningLevel         string           `json:"default_reasoning_level"`
	SupportedReasoningLevels      []ReasoningLevel `json:"supported_reasoning_levels"`
	ContextWindow                 int              `json:"context_window"`
	MaxContextWindow              int              `json:"max_context_window"`
	AutoCompactTokenLimit         *int             `json:"auto_compact_token_limit,omitempty"`
	EffectiveContextWindowPercent int              `json:"effective_context_window_percent"`
	AdditionalSpeedTiers          []string         `json:"additional_speed_tiers,omitempty"`
	ServiceTiers                  []ServiceTier    `json:"service_tiers,omitempty"`
	DefaultServiceTier            string           `json:"default_service_tier,omitempty"`
	SupportsParallelToolCalls     bool             `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal   bool             `json:"supports_image_detail_original"`
	InputModalities               []string         `json:"input_modalities"`
	UseResponsesLite              bool             `json:"use_responses_lite"`
	Visibility                    string           `json:"visibility,omitempty"`
	Priority                      int              `json:"priority,omitempty"`
	Raw                           json.RawMessage  `json:"-"`
}

// UnmarshalJSON mirrors Codex's serde default for model catalogs that omit
// effective_context_window_percent. An explicit zero remains zero and is
// rejected by Validate.
func (m *Model) UnmarshalJSON(data []byte) error {
	type modelWithoutMethods Model
	decoded := modelWithoutMethods{EffectiveContextWindowPercent: defaultEffectiveContextWindowPercent}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Model(decoded)
	return nil
}

type Catalog struct {
	Models        []Model
	FetchedAt     time.Time
	ETag          string
	ClientVersion string
	Backend       string
	Source        Source
}

//go:embed fallback.json
var fallbackFS embed.FS

type envelope struct {
	FetchedAt     time.Time         `json:"fetched_at,omitempty"`
	ETag          string            `json:"etag,omitempty"`
	ClientVersion string            `json:"client_version,omitempty"`
	Backend       string            `json:"backend,omitempty"`
	Models        []json.RawMessage `json:"models"`
}

type cacheEnvelope struct {
	FetchedAt     time.Time `json:"fetched_at"`
	ETag          string    `json:"etag,omitempty"`
	ClientVersion string    `json:"client_version"`
	Backend       string    `json:"backend"`
	Models        []Model   `json:"models"`
}

func (c Catalog) Find(slug string) (Model, bool) {
	for _, model := range c.Models {
		if model.Slug == slug {
			return model, true
		}
	}
	return Model{}, false
}

func (c Catalog) Age(now time.Time) time.Duration {
	return now.Sub(c.FetchedAt)
}

func (c Catalog) Fresh(now time.Time, ttl time.Duration, expectedClientVersion, expectedBackend string) bool {
	if c.FetchedAt.IsZero() || ttl < 0 {
		return false
	}
	if c.ClientVersion != expectedClientVersion || c.Backend != expectedBackend {
		return false
	}
	age := c.Age(now)
	if age < -clockSkewTolerance {
		return false
	}
	return age <= ttl
}

func LoadFallback() (Catalog, error) {
	data, err := fallbackFS.ReadFile("fallback.json")
	if err != nil {
		return Catalog{}, fmt.Errorf("read embedded fallback: %w", err)
	}
	if err := rejectForbiddenFields(data); err != nil {
		return Catalog{}, fmt.Errorf("unsafe embedded fallback: %w", err)
	}
	return Parse(data, SourceFallback)
}

func Parse(data []byte, source Source) (Catalog, error) {
	var wire envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&wire); err != nil {
		return Catalog{}, fmt.Errorf("parse catalog JSON: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Catalog{}, errors.New("parse catalog JSON: trailing JSON value")
		}
		return Catalog{}, fmt.Errorf("parse catalog JSON: trailing data: %w", err)
	}

	catalog := Catalog{
		Models:        make([]Model, 0, len(wire.Models)),
		FetchedAt:     wire.FetchedAt,
		ETag:          wire.ETag,
		ClientVersion: wire.ClientVersion,
		Backend:       wire.Backend,
		Source:        source,
	}
	for i, raw := range wire.Models {
		var model Model
		if err := json.Unmarshal(raw, &model); err != nil {
			return Catalog{}, fmt.Errorf("parse model %d: %w", i, err)
		}
		model.Raw = append(json.RawMessage(nil), raw...)
		catalog.Models = append(catalog.Models, model)
	}
	if err := catalog.Validate(); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c Catalog) Validate() error {
	if c.Source != SourceFallback && c.FetchedAt.IsZero() {
		return errors.New("catalog fetched_at is required outside fallback")
	}
	if len(c.Models) == 0 {
		return errors.New("catalog models must not be empty")
	}

	slugs := make(map[string]struct{}, len(c.Models))
	for i, model := range c.Models {
		prefix := fmt.Sprintf("model %d", i)
		if model.Slug == "" {
			return fmt.Errorf("%s slug must not be empty", prefix)
		}
		if _, exists := slugs[model.Slug]; exists {
			return fmt.Errorf("duplicate model slug %q", model.Slug)
		}
		slugs[model.Slug] = struct{}{}
		prefix = fmt.Sprintf("model %q", model.Slug)

		if model.ContextWindow <= 0 {
			return fmt.Errorf("%s context_window must be positive", prefix)
		}
		if model.MaxContextWindow < model.ContextWindow {
			return fmt.Errorf("%s max_context_window (%d) must be at least context_window (%d)", prefix, model.MaxContextWindow, model.ContextWindow)
		}
		if model.EffectiveContextWindowPercent < 1 || model.EffectiveContextWindowPercent > 100 {
			return fmt.Errorf("%s effective_context_window_percent must be between 1 and 100", prefix)
		}
		if len(model.SupportedReasoningLevels) == 0 {
			return fmt.Errorf("%s supported_reasoning_levels must not be empty", prefix)
		}
		efforts := make(map[string]struct{}, len(model.SupportedReasoningLevels))
		for _, level := range model.SupportedReasoningLevels {
			if level.Effort == "" {
				return fmt.Errorf("%s reasoning effort must not be empty", prefix)
			}
			if _, exists := efforts[level.Effort]; exists {
				return fmt.Errorf("%s has duplicate reasoning effort %q", prefix, level.Effort)
			}
			efforts[level.Effort] = struct{}{}
		}
		if _, exists := efforts[model.DefaultReasoningLevel]; !exists {
			return fmt.Errorf("%s default_reasoning_level %q is not supported", prefix, model.DefaultReasoningLevel)
		}
		tierIDs := make(map[string]struct{}, len(model.ServiceTiers))
		for _, tier := range model.ServiceTiers {
			if tier.ID == "" {
				return fmt.Errorf("%s service tier ID must not be empty", prefix)
			}
			if _, exists := tierIDs[tier.ID]; exists {
				return fmt.Errorf("%s has duplicate service tier ID %q", prefix, tier.ID)
			}
			tierIDs[tier.ID] = struct{}{}
		}
		if model.DefaultServiceTier != "" {
			if _, exists := tierIDs[model.DefaultServiceTier]; !exists {
				return fmt.Errorf("%s default_service_tier %q is not supported", prefix, model.DefaultServiceTier)
			}
		}
		if len(model.InputModalities) == 0 {
			return fmt.Errorf("%s input_modalities must not be empty", prefix)
		}
	}
	return nil
}

func LoadFile(path string, source Source) (Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, fmt.Errorf("read catalog %q: %w", path, err)
	}
	catalog, err := Parse(data, source)
	if err != nil {
		return Catalog{}, fmt.Errorf("load catalog %q: %w", path, err)
	}
	return catalog, nil
}

func Save(path string, catalog Catalog) error {
	if err := catalog.Validate(); err != nil {
		return fmt.Errorf("save catalog: %w", err)
	}
	backend := catalog.Backend
	if backend == "" {
		backend = BackendURL
	}
	wire := cacheEnvelope{
		FetchedAt:     catalog.FetchedAt,
		ETag:          catalog.ETag,
		ClientVersion: catalog.ClientVersion,
		Backend:       backend,
		Models:        catalog.Models,
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return fmt.Errorf("serialize catalog: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create catalog directory %q: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".catalog-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary catalog: %w", err)
	}
	tempName := temp.Name()
	closed := false
	defer func() {
		if !closed {
			_ = temp.Close()
		}
		_ = os.Remove(tempName)
	}()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set temporary catalog permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		return fmt.Errorf("write temporary catalog: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync temporary catalog: %w", err)
	}
	if err := temp.Close(); err != nil {
		closed = true
		return fmt.Errorf("close temporary catalog: %w", err)
	}
	closed = true
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace catalog %q: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		directory, err := os.Open(dir)
		if err != nil {
			return fmt.Errorf("open catalog directory for sync: %w", err)
		}
		syncErr := directory.Sync()
		closeErr := directory.Close()
		if syncErr != nil {
			return fmt.Errorf("sync catalog directory: %w", syncErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close catalog directory: %w", closeErr)
		}
	}
	return nil
}

func rejectForbiddenFields(data []byte) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("parse fallback for safety scan: %w", err)
	}
	forbidden := map[string]struct{}{
		"authorization":     {},
		"token":             {},
		"account":           {},
		"email":             {},
		"base_instructions": {},
		"instructions":      {},
	}
	var scan func(any) error
	scan = func(current any) error {
		switch current := current.(type) {
		case map[string]any:
			for key, child := range current {
				if _, blocked := forbidden[strings.ToLower(key)]; blocked {
					return fmt.Errorf("forbidden field %q", key)
				}
				if err := scan(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range current {
				if err := scan(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return scan(value)
}
