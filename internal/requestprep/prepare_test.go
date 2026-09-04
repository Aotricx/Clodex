package requestprep

import (
	"strings"
	"testing"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/translate"
)

func TestPrepareUsesOneCatalogGroundedPathForMessagesAndCounting(t *testing.T) {
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	message, err := anthropic.DecodeRequest(strings.NewReader(`{
		"model":"claude-opus-4-8","max_tokens":32000,
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","budget_tokens":20000}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	count, err := anthropic.DecodeCountTokensRequest(strings.NewReader(`{
		"model":"claude-opus-4-8",
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","budget_tokens":20000}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	messagePrepared, err := Prepare(cat, message, "gpt-5.6-sol:medium", "session-1")
	if err != nil {
		t.Fatalf("Prepare(message) error = %v", err)
	}
	countPrepared, err := Prepare(cat, count, "gpt-5.6-sol:medium", "session-1")
	if err != nil {
		t.Fatalf("Prepare(count) error = %v", err)
	}
	if messagePrepared.Selection.Model.Slug != "gpt-5.6-sol" || messagePrepared.Selection.Effort != "medium" {
		t.Fatalf("message selection = %#v", messagePrepared.Selection)
	}
	if countPrepared.Selection.Model.Slug != messagePrepared.Selection.Model.Slug || countPrepared.Selection.Effort != messagePrepared.Selection.Effort || countPrepared.Selection.ServiceTier != messagePrepared.Selection.ServiceTier || countPrepared.Selection.Fast != messagePrepared.Selection.Fast {
		t.Fatalf("count selection = %#v, want %#v", countPrepared.Selection, messagePrepared.Selection)
	}
	if countPrepared.Translation.Request.PromptCacheKey != "session-1" {
		t.Fatalf("count prompt cache key = %q", countPrepared.Translation.Request.PromptCacheKey)
	}
	if warningCount(messagePrepared.Translation.Warnings, translate.WarningMaxTokensUnsupported) != 1 || warningCount(countPrepared.Translation.Warnings, translate.WarningMaxTokensUnsupported) != 0 {
		t.Fatalf("message/count warnings = %#v / %#v", messagePrepared.Translation.Warnings, countPrepared.Translation.Warnings)
	}
}

func TestPrepareDisabledThinkingStillEmitsCatalogReasoning(t *testing.T) {
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	req, err := anthropic.DecodeRequest(strings.NewReader(`{
		"model":"gpt-5.6-sol","max_tokens":1,
		"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"disabled"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(cat, req, "gpt-5.6-sol", "")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Selection.Effort == "" || prepared.Translation.Request.Reasoning == nil || prepared.Translation.Request.Reasoning.Effort == "" {
		t.Fatalf("disabled thinking omitted reasoning: selection=%#v reasoning=%#v", prepared.Selection, prepared.Translation.Request.Reasoning)
	}
	if warningCount(prepared.Translation.Warnings, translate.WarningThinkingTypeUnsupported) != 1 {
		t.Fatalf("warnings = %#v, want thinking.type_unsupported", prepared.Translation.Warnings)
	}
}

func TestPrepareMapsHugeThinkingBudgetWithoutIntegerOverflow(t *testing.T) {
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	req, err := anthropic.DecodeCountTokensRequest(strings.NewReader(`{
		"model":"gpt-5.6-sol","messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"enabled","budget_tokens":9223372036854775807}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(cat, req, "gpt-5.6-sol", "")
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Selection.Effort != "ultra" {
		t.Fatalf("effort = %q, want ultra", prepared.Selection.Effort)
	}
}

func TestPrepareReturnsHonestModelAndTranslationFailures(t *testing.T) {
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	req := &anthropic.MessageRequest{Model: "gpt-5.6-sol:minimal"}
	if _, err := Prepare(cat, req, "gpt-5.6-sol:medium", ""); err == nil || !strings.Contains(err.Error(), "minimal") {
		t.Fatalf("Prepare() error = %v", err)
	}
	if _, err := Prepare(cat, nil, "gpt-5.6-sol:medium", ""); err == nil {
		t.Fatal("Prepare(nil) error = nil")
	}
}

func warningCount(warnings []translate.Warning, kind string) int {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return warning.Count
		}
	}
	return 0
}
