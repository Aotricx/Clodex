package model

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Aotricx/Clodex/internal/catalog"
)

func TestResolveAcceptsModelIDGrammar(t *testing.T) {
	cat := fallbackCatalog(t)
	tests := []struct {
		request, effort string
		fast            bool
		serviceTier     string
	}{
		{"gpt-5.6-sol", "medium", false, ""},
		{"gpt-5.6-sol:high", "high", false, ""},
		{"gpt-5.6-sol:fast", "medium", true, "priority"},
		{"gpt-5.6-sol:xhigh:fast", "xhigh", true, "priority"},
	}
	for _, tc := range tests {
		t.Run(tc.request, func(t *testing.T) {
			got, err := Resolve(cat, tc.request, "gpt-5.6-sol:medium", 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.Model.Slug != "gpt-5.6-sol" || got.Effort != tc.effort || got.Fast != tc.fast || got.ServiceTier != tc.serviceTier {
				t.Fatalf("Resolve() = %+v", got)
			}
		})
	}
}

func TestClaudeCarrierRoundTripsCanonicalSelection(t *testing.T) {
	cat := fallbackCatalog(t)
	canonical := "gpt-5.6-sol:high:fast"
	carrier := ClaudeCarrierID(canonical)
	if carrier != "anthropic-clodex-gpt-5.6-sol:high:fast[1m]" {
		t.Fatalf("carrier = %q", carrier)
	}
	decoded, ok := CanonicalIDFromClaudeCarrier(carrier)
	if !ok || decoded != canonical {
		t.Fatalf("decoded = %q, %v", decoded, ok)
	}
	got, err := Resolve(cat, carrier, "gpt-5.6-luna:low", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanonicalID() != canonical || got.Model.Slug != "gpt-5.6-sol" || got.Effort != "high" || !got.Fast {
		t.Fatalf("carrier Resolve() = %+v ID=%q", got, got.CanonicalID())
	}
}

func TestMalformedCarrierRemainsUnknownFallback(t *testing.T) {
	cat := fallbackCatalog(t)
	for _, request := range []string{"anthropic-clodex-gpt-5.6-sol[wrong]", "anthropic-clodex-[1m]", "anthropic-clodex-future[1m]"} {
		if _, err := Resolve(cat, request, "gpt-5.6-luna:low", 0); err == nil {
			t.Fatalf("Resolve(%q) returned nil error", request)
		}
	}
}

func TestResolveRejectsMalformedModelIDs(t *testing.T) {
	cat := fallbackCatalog(t)
	for _, request := range []string{
		"",
		":",
		":high",
		"gpt-5.6-sol:",
		"gpt-5.6-sol::fast",
		"gpt-5.6-sol:fast:high",
		"gpt-5.6-sol:fast:fast",
		"gpt-5.6-sol:high:slow",
		"gpt-5.6-sol:high:fast:extra",
		" gpt-5.6-sol",
		"gpt-5.6-sol high",
	} {
		t.Run(request, func(t *testing.T) {
			if _, err := Resolve(cat, request, "gpt-5.6-sol:medium", 0); err == nil {
				t.Fatal("Resolve() returned nil error")
			}
		})
	}
}

func TestResolveFallsBackWithoutAliasPolicy(t *testing.T) {
	cat := fallbackCatalog(t)
	tests := []struct {
		name, request, defaultID string
		budget                   int
		wantSlug, wantEffort     string
		wantFast                 bool
	}{
		{"Claude alias uses configured default", "claude-opus-4-1", "gpt-5.6-sol:xhigh", 0, "gpt-5.6-sol", "xhigh", false},
		{"requested effort wins during alias fallback", "claude-opus-4-1:low", "gpt-5.6-sol:xhigh", 0, "gpt-5.6-sol", "low", false},
		{"requested effort and fast survive alias fallback", "claude-opus-4-1:high:fast", "gpt-5.6-sol:medium", 0, "gpt-5.6-sol", "high", true},
		{"Claude fallback inherits default fast", "claude-opus-4-1", "gpt-5.6-sol:medium:fast", 0, "gpt-5.6-sol", "medium", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(cat, tc.request, tc.defaultID, tc.budget)
			if err != nil {
				t.Fatal(err)
			}
			if got.Model.Slug != tc.wantSlug || got.Effort != tc.wantEffort || got.Fast != tc.wantFast {
				t.Fatalf("Resolve() = %+v, want slug=%q effort=%q fast=%v", got, tc.wantSlug, tc.wantEffort, tc.wantFast)
			}
		})
	}
}

func TestResolveRejectsUnrecognizedNonClaudeSlugs(t *testing.T) {
	cat := fallbackCatalog(t)
	for _, request := range []string{
		"gpt-5.6-so1",
		"GPT-5.6-sol",
		"future-unknown-model",
		"future-unknown-model:fast",
		"future-unknown-model:high",
	} {
		t.Run(request, func(t *testing.T) {
			if _, err := Resolve(cat, request, "gpt-5.6-sol:medium", 0); err == nil {
				t.Fatal("Resolve() returned nil error")
			}
		})
	}
}

func TestResolveUsesCatalogClaudeSlugOverPrefixAlias(t *testing.T) {
	cat := catalog.Catalog{Models: []catalog.Model{
		{
			Slug:                     "gpt-5.6-sol",
			DefaultReasoningLevel:    "medium",
			SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "medium"}, {Effort: "high"}},
		},
		{
			Slug:                     "claude-custom",
			DefaultReasoningLevel:    "high",
			SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "medium"}, {Effort: "high"}},
		},
	}}
	got, err := Resolve(cat, "claude-custom", "gpt-5.6-sol:medium", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model.Slug != "claude-custom" || got.Effort != "high" {
		t.Fatalf("Resolve() = %+v, want catalog slug claude-custom", got)
	}
}

func TestResolveKnownModelDoesNotInheritDefaultFast(t *testing.T) {
	cat := fallbackCatalog(t)
	got, err := Resolve(cat, "gpt-5.6-sol", "gpt-5.6-sol:medium:fast", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Fast || got.ServiceTier != "" {
		t.Fatalf("Resolve() = %+v, want non-fast direct selection", got)
	}
}

func TestResolveEffortPrecedence(t *testing.T) {
	cat := fallbackCatalog(t)
	tests := []struct {
		name, request, defaultID string
		budget                   int
		want                     string
	}{
		{"explicit request over default and budget", "gpt-5.6-sol:high", "gpt-5.6-sol:xhigh", 1024, "high"},
		{"fallback default suffix over budget", "claude-opus-4-1", "gpt-5.6-sol:xhigh", 1024, "xhigh"},
		{"fallback budget over catalog default", "claude-opus-4-1", "gpt-5.6-sol", 9000, "high"},
		{"direct request does not inherit configured default suffix", "gpt-5.6-sol", "gpt-5.6-sol:xhigh", 1024, "low"},
		{"catalog default last", "gpt-5.6-sol", "gpt-5.6-sol:xhigh", 0, "medium"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(cat, tc.request, tc.defaultID, tc.budget)
			if err != nil {
				t.Fatal(err)
			}
			if got.Effort != tc.want {
				t.Fatalf("Effort = %q, want %q", got.Effort, tc.want)
			}
		})
	}
}

func TestResolveThinkingBudgetBoundaries(t *testing.T) {
	cat := fallbackCatalog(t)
	tests := []struct {
		budget int
		want   string
	}{
		{0, "medium"},
		{-1, "medium"},
		{1, "low"},
		{2048, "low"},
		{2049, "medium"},
		{8192, "medium"},
		{8193, "high"},
		{16384, "high"},
		{16385, "xhigh"},
		{32768, "xhigh"},
		{32769, "max"},
		{65536, "max"},
		{65537, "ultra"},
	}
	for _, tc := range tests {
		t.Run(tc.want+"/"+strings.ReplaceAll(strconv.Itoa(tc.budget), "-", "negative-"), func(t *testing.T) {
			got, err := Resolve(cat, "gpt-5.6-sol", "gpt-5.6-sol:medium", tc.budget)
			if err != nil {
				t.Fatal(err)
			}
			if got.Effort != tc.want {
				t.Fatalf("budget %d: Effort = %q, want %q", tc.budget, got.Effort, tc.want)
			}
		})
	}
}

func TestResolveProjectsBudgetOntoCatalogEfforts(t *testing.T) {
	cat := fallbackCatalog(t)
	got, err := Resolve(cat, "gpt-5.5", "gpt-5.6-sol:medium", 65537)
	if err != nil {
		t.Fatal(err)
	}
	if got.Effort != "xhigh" {
		t.Fatalf("gpt-5.5 ultra budget = %q, want xhigh", got.Effort)
	}

	sparse := catalog.Catalog{Models: []catalog.Model{{
		Slug:                     "sparse",
		DefaultReasoningLevel:    "medium",
		SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "medium"}, {Effort: "max"}},
	}}}
	for _, tc := range []struct {
		budget int
		want   string
	}{{1, "medium"}, {32768, "medium"}, {32769, "max"}} {
		got, err := Resolve(sparse, "sparse", "sparse", tc.budget)
		if err != nil {
			t.Fatal(err)
		}
		if got.Effort != tc.want {
			t.Errorf("budget %d: Effort = %q, want %q", tc.budget, got.Effort, tc.want)
		}
	}
}

func TestResolveValidatesExplicitAndDefaultEfforts(t *testing.T) {
	cat := fallbackCatalog(t)
	tests := []struct {
		name, request, defaultID string
	}{
		{"unsupported requested effort", "gpt-5.5:max", "gpt-5.6-sol:medium"},
		{"unsupported fallback default effort", "claude-opus-4-1", "gpt-5.5:max"},
		{"unknown requested effort", "gpt-5.6-sol:minimal", "gpt-5.6-sol:medium"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(cat, tc.request, tc.defaultID, 0)
			if err == nil || !strings.Contains(err.Error(), "effort") {
				t.Fatalf("Resolve() error = %v", err)
			}
		})
	}
}

func TestResolveValidatesFastCapability(t *testing.T) {
	cat := fallbackCatalog(t)
	got, err := Resolve(cat, "gpt-5.6-sol:fast", "gpt-5.6-sol:medium", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Fast || got.ServiceTier != "priority" {
		t.Fatalf("fast selection = %+v", got)
	}

	// Every model in the live catalog advertises a fast tier, so the rejection
	// cases need a catalog that explicitly contains a model without one.
	nofast := catalog.Catalog{Models: []catalog.Model{
		{Slug: "nofast", DefaultReasoningLevel: "medium", SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "medium"}}},
		{Slug: "hasfast", DefaultReasoningLevel: "medium", SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "medium"}},
			AdditionalSpeedTiers: []string{"fast"}, ServiceTiers: []catalog.ServiceTier{{ID: "priority"}}, DefaultServiceTier: "priority"},
	}}
	for _, tc := range []struct {
		name, request, defaultID string
	}{
		{"model does not advertise fast", "nofast:fast", "hasfast:medium"},
		{"fallback model does not advertise fast", "claude-opus-4-1:fast", "nofast:medium"},
		{"default fast unsupported", "hasfast", "nofast:medium:fast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(nofast, tc.request, tc.defaultID, 0)
			if err == nil || !strings.Contains(err.Error(), "fast") {
				t.Fatalf("Resolve() error = %v", err)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		model catalog.Model
	}{
		{"missing priority service tier", catalog.Model{Slug: "m", DefaultReasoningLevel: "medium", SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "medium"}}, AdditionalSpeedTiers: []string{"fast"}}},
		{"missing fast speed tier", catalog.Model{Slug: "m", DefaultReasoningLevel: "medium", SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "medium"}}, ServiceTiers: []catalog.ServiceTier{{ID: "priority"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Resolve(catalog.Catalog{Models: []catalog.Model{tc.model}}, "m:fast", "m", 0)
			if err == nil || !strings.Contains(err.Error(), "fast") {
				t.Fatalf("Resolve() error = %v", err)
			}
		})
	}
}

func TestResolveRejectsInvalidDefaultIDEvenWhenRequestIsKnown(t *testing.T) {
	cat := fallbackCatalog(t)
	for _, defaultID := range []string{
		"missing-model:medium",
		"gpt-5.6-sol::fast",
		"gpt-5.5:max",
	} {
		t.Run(defaultID, func(t *testing.T) {
			if _, err := Resolve(cat, "gpt-5.6-sol", defaultID, 0); err == nil {
				t.Fatal("Resolve() returned nil error")
			}
		})
	}
}

func TestNearestSupportedFloor(t *testing.T) {
	cat := fallbackCatalog(t)
	tests := []struct {
		model, rejected, want string
	}{
		{"gpt-5.6-sol", "ultra", "max"},
		{"gpt-5.6-sol", "max", "xhigh"},
		{"gpt-5.6-luna", "ultra", "max"},
		{"gpt-5.5", "max", "xhigh"},
		{"gpt-5.5", "high", "medium"},
	}
	for _, tc := range tests {
		t.Run(tc.model+"/"+tc.rejected, func(t *testing.T) {
			m, ok := cat.Find(tc.model)
			if !ok {
				t.Fatal("fixture model missing")
			}
			got, err := NearestSupportedFloor(m, tc.rejected)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("NearestSupportedFloor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNearestSupportedFloorUsesSparseCatalogAndExcludesRejectedLevel(t *testing.T) {
	m := catalog.Model{
		Slug: "sparse",
		SupportedReasoningLevels: []catalog.ReasoningLevel{
			{Effort: "low"},
			{Effort: "xhigh"},
		},
	}
	for _, tc := range []struct {
		rejected, want string
	}{{"max", "xhigh"}, {"high", "low"}, {"xhigh", "low"}} {
		got, err := NearestSupportedFloor(m, tc.rejected)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("rejected %q: got %q, want %q", tc.rejected, got, tc.want)
		}
	}
	for _, rejected := range []string{"low", "unknown"} {
		if _, err := NearestSupportedFloor(m, rejected); err == nil {
			t.Errorf("rejected %q: nil error", rejected)
		}
	}
}

func fallbackCatalog(t *testing.T) catalog.Catalog {
	t.Helper()
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	return cat
}
