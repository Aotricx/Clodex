package catalog

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestConstants(t *testing.T) {
	if BackendURL != "https://chatgpt.com/backend-api/codex" {
		t.Fatalf("BackendURL = %q", BackendURL)
	}
	if ClientVersion != "0.154.0" {
		t.Fatalf("ClientVersion = %q", ClientVersion)
	}
	if DefaultTTL != 5*time.Minute {
		t.Fatalf("DefaultTTL = %s", DefaultTTL)
	}
}

func TestLoadFallbackExactCatalog(t *testing.T) {
	c, err := LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceFallback {
		t.Fatalf("Source = %q", c.Source)
	}
	if c.Backend != BackendURL || c.ClientVersion != ClientVersion {
		t.Fatalf("fallback identity = backend %q, client %q", c.Backend, c.ClientVersion)
	}
	if !c.FetchedAt.IsZero() {
		t.Fatalf("fallback FetchedAt = %s, want zero", c.FetchedAt)
	}

	type wantModel struct {
		slug, display, description, defaultEffort, visibility string
		efforts                                               []string
		priority, maxContext                                  int
		fast, lite                                            bool
		serviceDescription, defaultService                    string
	}
	want := []wantModel{
		{"gpt-6-astra", "GPT-6-Astra", "Our most capable model for complex, demanding work.", "medium", "list", []string{"low", "medium", "high", "xhigh", "max", "ultra"}, 1, 872000, true, true, "2x speed, increased usage", ""},
		{"gpt-reserve", "GPT-Reserve", "Fast and affordable agentic coding model.", "medium", "hide", []string{"low", "medium", "high", "xhigh", "max"}, 3, 272000, true, true, "1.5x speed, increased usage", "priority"},
		{"gpt-5.6-sol", "GPT-5.6-Sol", "Reliable agentic workhorse for everyday tasks.", "medium", "list", []string{"low", "medium", "high", "xhigh", "max", "ultra"}, 6, 272000, true, true, "1.5x speed, increased usage", "priority"},
		{"gpt-5.6-terra", "GPT-5.6-Terra", "Balanced agentic coding model for everyday work.", "medium", "list", []string{"low", "medium", "high", "xhigh", "max", "ultra"}, 7, 272000, true, true, "1.5x speed, increased usage", "priority"},
		{"gpt-5.6-luna", "GPT-5.6-Luna", "Fast and affordable agentic coding model.", "medium", "list", []string{"low", "medium", "high", "xhigh", "max"}, 8, 272000, true, true, "1.5x speed, increased usage", "priority"},
		{"gpt-5.5", "GPT-5.5", "Proven previous-generation model for coding and general work.", "xhigh", "list", []string{"low", "medium", "high", "xhigh"}, 12, 272000, true, false, "1.5x speed, increased usage", "priority"},
		{"codex-auto-review", "Codex Auto Review", "Automatic approval review model for Codex.", "medium", "hide", []string{"low", "medium", "high", "xhigh", "max"}, 43, 272000, true, true, "1.5x speed, increased usage", "priority"},
	}
	if len(c.Models) != len(want) {
		t.Fatalf("len(Models) = %d, want %d", len(c.Models), len(want))
	}
	for i, w := range want {
		m := c.Models[i]
		if m.Slug != w.slug || m.DisplayName != w.display || m.Description != w.description ||
			m.DefaultReasoningLevel != w.defaultEffort || m.Visibility != w.visibility ||
			m.Priority != w.priority || m.MaxContextWindow != w.maxContext || m.UseResponsesLite != w.lite {
			t.Errorf("model %d mismatch:\n got  %+v\n want %+v", i, m, w)
		}
		if m.ContextWindow != 272000 || m.EffectiveContextWindowPercent != 95 {
			t.Errorf("%s context = %d/%d%%", m.Slug, m.ContextWindow, m.EffectiveContextWindowPercent)
		}
		if m.AutoCompactTokenLimit != nil {
			t.Errorf("%s AutoCompactTokenLimit = %v, want nil", m.Slug, *m.AutoCompactTokenLimit)
		}
		if !m.SupportsParallelToolCalls || !m.SupportsImageDetailOriginal {
			t.Errorf("%s capability flags = parallel %v, image-original %v", m.Slug, m.SupportsParallelToolCalls, m.SupportsImageDetailOriginal)
		}
		if !reflect.DeepEqual(m.InputModalities, []string{"text", "image"}) {
			t.Errorf("%s modalities = %v", m.Slug, m.InputModalities)
		}
		gotEfforts := make([]string, len(m.SupportedReasoningLevels))
		for j, level := range m.SupportedReasoningLevels {
			gotEfforts[j] = level.Effort
			if level.Description == "" {
				t.Errorf("%s effort %q has empty description", m.Slug, level.Effort)
			}
		}
		if !reflect.DeepEqual(gotEfforts, w.efforts) {
			t.Errorf("%s efforts = %v, want %v", m.Slug, gotEfforts, w.efforts)
		}
		if w.fast {
			if !reflect.DeepEqual(m.AdditionalSpeedTiers, []string{"fast"}) || len(m.ServiceTiers) != 1 ||
				m.ServiceTiers[0] != (ServiceTier{ID: "priority", Name: "Fast", Description: w.serviceDescription}) ||
				m.DefaultServiceTier != w.defaultService {
				t.Errorf("%s speed/service tiers mismatch: %+v %+v %q", m.Slug, m.AdditionalSpeedTiers, m.ServiceTiers, m.DefaultServiceTier)
			}
		} else if len(m.AdditionalSpeedTiers) != 0 || len(m.ServiceTiers) != 0 || m.DefaultServiceTier != "" {
			t.Errorf("%s unexpectedly has tiers: %+v %+v %q", m.Slug, m.AdditionalSpeedTiers, m.ServiceTiers, m.DefaultServiceTier)
		}
		if len(m.Raw) == 0 || !json.Valid(m.Raw) {
			t.Errorf("%s Raw is not retained valid JSON", m.Slug)
		}
	}
}

func TestFallbackContainsNoSensitiveFields(t *testing.T) {
	data, err := os.ReadFile("fallback.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectForbiddenFields(data); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"authorization", "token", "account", "email", "base_instructions", "instructions"} {
		data := []byte(`{"models":[{"slug":"safe","` + field + `":"secret"}]}`)
		if err := rejectForbiddenFields(data); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("field %q: err = %v", field, err)
		}
	}
}

func TestFind(t *testing.T) {
	c := validCatalog()
	got, ok := c.Find("model-a")
	if !ok || got.Slug != "model-a" {
		t.Fatalf("Find existing = %+v, %v", got, ok)
	}
	if _, ok := c.Find("missing"); ok {
		t.Fatal("Find missing returned ok")
	}
}

func TestAge(t *testing.T) {
	fetched := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	c := Catalog{FetchedAt: fetched}
	if got := c.Age(fetched.Add(3 * time.Minute)); got != 3*time.Minute {
		t.Fatalf("Age = %s", got)
	}
}

func TestParseRetainsRawUnknownModelFields(t *testing.T) {
	data := validEnvelopeJSON(`"future_model_field":{"enabled":true}`)
	c, err := Parse(data, SourceCache)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceCache || c.ETag != "etag-1" || c.Backend != BackendURL {
		t.Fatalf("metadata mismatch: %+v", c)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(c.Models[0].Raw, &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["future_model_field"]); got != `{"enabled":true}` {
		t.Fatalf("unknown Raw field = %s", got)
	}
	if _, ok := raw["slug"]; !ok {
		t.Fatal("Raw does not retain known fields")
	}
}

func TestParseAcceptsImportedCodexCacheWithoutBackend(t *testing.T) {
	data := bytes.Replace(validEnvelopeJSON(""), []byte(`"backend":"https://chatgpt.com/backend-api/codex",`), nil, 1)
	c, err := Parse(data, SourceCache)
	if err != nil {
		t.Fatal(err)
	}
	if c.Backend != "" {
		t.Fatalf("Backend = %q, want empty for imported cache", c.Backend)
	}
}

func TestParseRejectsInvalidAndTrailingJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"invalid", []byte(`{"models":[`)},
		{"trailing object", append(validEnvelopeJSON(""), []byte(` {}`)...)},
		{"trailing garbage", append(validEnvelopeJSON(""), []byte(` nope`)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse(tc.data, SourceCache); err == nil {
				t.Fatal("Parse returned nil error")
			}
		})
	}
}

func TestParseRequiresFetchedAtOutsideFallback(t *testing.T) {
	data := []byte(`{"client_version":"0.144.6","models":[{"slug":"m","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"medium"}],"context_window":1,"max_context_window":1,"effective_context_window_percent":100,"input_modalities":["text"]}]}`)
	for _, source := range []Source{SourceLive, SourceCache} {
		if _, err := Parse(data, source); err == nil || !strings.Contains(err.Error(), "fetched_at") {
			t.Errorf("source %q: err = %v", source, err)
		}
	}
	if _, err := Parse(data, SourceFallback); err != nil {
		t.Fatalf("fallback without fetched_at: %v", err)
	}
}

func TestParseAppliesCodexEffectiveContextDefaultWhenOmitted(t *testing.T) {
	data := []byte(`{"models":[{"slug":"m","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"medium"}],"context_window":272000,"max_context_window":272000,"input_modalities":["text","image"]}]}`)
	got, err := Parse(data, SourceFallback)
	if err != nil {
		t.Fatal(err)
	}
	if got.Models[0].EffectiveContextWindowPercent != 95 {
		t.Fatalf("effective context percent = %d, want Codex default 95", got.Models[0].EffectiveContextWindowPercent)
	}
}

func TestValidateRules(t *testing.T) {
	tests := []struct {
		name, want string
		mutate     func(*Catalog)
	}{
		{"models required", "models", func(c *Catalog) { c.Models = nil }},
		{"slug required", "slug", func(c *Catalog) { c.Models[0].Slug = "" }},
		{"unique slugs", "duplicate", func(c *Catalog) { c.Models = append(c.Models, c.Models[0]) }},
		{"positive context", "context_window", func(c *Catalog) { c.Models[0].ContextWindow = 0 }},
		{"max at least context", "max_context_window", func(c *Catalog) { c.Models[0].MaxContextWindow = c.Models[0].ContextWindow - 1 }},
		{"effective percent lower bound", "effective_context_window_percent", func(c *Catalog) { c.Models[0].EffectiveContextWindowPercent = 0 }},
		{"effective percent upper bound", "effective_context_window_percent", func(c *Catalog) { c.Models[0].EffectiveContextWindowPercent = 101 }},
		{"reasoning efforts required", "supported_reasoning_levels", func(c *Catalog) { c.Models[0].SupportedReasoningLevels = nil }},
		{"reasoning effort required", "effort", func(c *Catalog) { c.Models[0].SupportedReasoningLevels[0].Effort = "" }},
		{"unique reasoning efforts", "duplicate", func(c *Catalog) {
			c.Models[0].SupportedReasoningLevels = append(c.Models[0].SupportedReasoningLevels, c.Models[0].SupportedReasoningLevels[0])
		}},
		{"default effort supported", "default_reasoning_level", func(c *Catalog) { c.Models[0].DefaultReasoningLevel = "high" }},
		{"unique service tier IDs", "service tier", func(c *Catalog) {
			c.Models[0].ServiceTiers = append(c.Models[0].ServiceTiers, c.Models[0].ServiceTiers[0])
		}},
		{"service tier ID required", "service tier ID", func(c *Catalog) { c.Models[0].ServiceTiers[0].ID = "" }},
		{"default service tier supported", "default_service_tier", func(c *Catalog) { c.Models[0].DefaultServiceTier = "standard" }},
		{"default service tier requires tiers", "default_service_tier", func(c *Catalog) { c.Models[0].ServiceTiers = nil }},
		{"modalities required", "input_modalities", func(c *Catalog) { c.Models[0].InputModalities = nil }},
		{"cache fetched at required", "fetched_at", func(c *Catalog) { c.FetchedAt = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validCatalog()
			tc.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("Validate error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateAllowsEmptyDefaultServiceTier(t *testing.T) {
	c := validCatalog()
	c.Models[0].DefaultServiceTier = ""
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFreshBoundariesAndIdentity(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	base := validCatalog()
	base.FetchedAt = now.Add(-DefaultTTL)
	tests := []struct {
		name            string
		mutate          func(*Catalog)
		expectedClient  string
		expectedBackend string
		want            bool
	}{
		{"age equal ttl", nil, ClientVersion, BackendURL, true},
		{"age beyond ttl", func(c *Catalog) { c.FetchedAt = now.Add(-DefaultTTL - time.Nanosecond) }, ClientVersion, BackendURL, false},
		{"client mismatch", nil, "different-client", BackendURL, false},
		{"backend mismatch", nil, ClientVersion, "https://other.invalid", false},
		{"missing backend accepted only when expected missing", func(c *Catalog) { c.Backend = "" }, ClientVersion, "", true},
		{"missing backend rejected for codex backend", func(c *Catalog) { c.Backend = "" }, ClientVersion, BackendURL, false},
		{"future within tolerance", func(c *Catalog) { c.FetchedAt = now.Add(30 * time.Second) }, ClientVersion, BackendURL, true},
		{"future beyond tolerance", func(c *Catalog) { c.FetchedAt = now.Add(30*time.Second + time.Nanosecond) }, ClientVersion, BackendURL, false},
		{"zero fetched at", func(c *Catalog) { c.FetchedAt = time.Time{} }, ClientVersion, BackendURL, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			if got := c.Fresh(now, DefaultTTL, tc.expectedClient, tc.expectedBackend); got != tc.want {
				t.Fatalf("Fresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSaveLoadRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "models.json")
	c := validCatalog()
	c.Backend = "" // Clodex-owned cache must still identify its sole backend.
	c.Models = append(c.Models, c.Models[0])
	c.Models[0].Slug = "z-model"
	c.Models[1].Slug = "a-model"
	c.Models[0].Raw = json.RawMessage(`{"max_output_tokens":999,"base_instructions":"must not serialize"}`)

	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := Save(path, c); err != nil {
			t.Fatal(err)
		}
		info, err = os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("overwrite mode = %o, want 600", info.Mode().Perm())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(data), []byte("output")) || bytes.Contains(data, []byte("base_instructions")) || bytes.Contains(data, []byte(`"raw"`)) {
		t.Fatalf("unsafe/non-envelope field serialized: %s", data)
	}

	loaded, err := LoadFile(path, SourceCache)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Backend != BackendURL || loaded.Source != SourceCache {
		t.Fatalf("loaded identity = backend %q, source %q", loaded.Backend, loaded.Source)
	}
	if got := []string{loaded.Models[0].Slug, loaded.Models[1].Slug}; !reflect.DeepEqual(got, []string{"z-model", "a-model"}) {
		t.Fatalf("model order = %v", got)
	}
	for i := range loaded.Models {
		c.Models[i].Raw = nil
		loaded.Models[i].Raw = nil
	}
	c.Backend = BackendURL
	if !reflect.DeepEqual(loaded.Models, c.Models) || loaded.FetchedAt != c.FetchedAt || loaded.ETag != c.ETag || loaded.ClientVersion != c.ClientVersion {
		t.Fatalf("round trip mismatch:\n got  %+v\n want %+v", loaded, c)
	}
}

func TestSaveInvalidDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	original := []byte("original")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	c := validCatalog()
	c.Models = nil
	if err := Save(path, c); err == nil {
		t.Fatal("Save invalid catalog returned nil error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("invalid Save overwrote destination: %q", got)
	}
}

func TestSaveFailureCleansTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "destination")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Save(path, validCatalog()); err == nil {
		t.Fatal("Save over directory returned nil error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "destination" {
		t.Fatalf("temporary files remain: %v", entries)
	}
}

func validCatalog() Catalog {
	return Catalog{
		Models: []Model{{
			Slug:                          "model-a",
			DisplayName:                   "Model A",
			Description:                   "Test model.",
			DefaultReasoningLevel:         "medium",
			SupportedReasoningLevels:      []ReasoningLevel{{Effort: "medium", Description: "Balanced"}},
			ContextWindow:                 100,
			MaxContextWindow:              200,
			EffectiveContextWindowPercent: 95,
			AdditionalSpeedTiers:          []string{"fast"},
			ServiceTiers:                  []ServiceTier{{ID: "priority", Name: "Fast"}},
			DefaultServiceTier:            "priority",
			SupportsParallelToolCalls:     true,
			SupportsImageDetailOriginal:   true,
			InputModalities:               []string{"text", "image"},
			Visibility:                    "list",
			Priority:                      1,
		}},
		FetchedAt:     time.Date(2026, 7, 21, 12, 0, 0, 123, time.UTC),
		ETag:          "etag-1",
		ClientVersion: ClientVersion,
		Backend:       BackendURL,
		Source:        SourceCache,
	}
}

func validEnvelopeJSON(extraModelField string) []byte {
	comma := ""
	if extraModelField != "" {
		comma = "," + extraModelField
	}
	return []byte(`{
		"fetched_at":"2026-07-21T12:00:00Z",
		"etag":"etag-1",
		"client_version":"0.144.6",
		"backend":"https://chatgpt.com/backend-api/codex",
		"future_envelope_field":true,
		"models":[{
			"slug":"model-a",
			"display_name":"Model A",
			"description":"Test model.",
			"default_reasoning_level":"medium",
			"supported_reasoning_levels":[{"effort":"medium","description":"Balanced"}],
			"context_window":100,
			"max_context_window":200,
			"effective_context_window_percent":95,
			"service_tiers":[{"id":"priority","name":"Fast"}],
			"input_modalities":["text"]` + comma + `
		}]
	}`)
}
