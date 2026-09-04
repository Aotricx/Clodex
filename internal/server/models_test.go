package server

import (
	"errors"
	"net/url"
	"testing"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/model"
)

func TestListModelsExposesEveryCatalogEffortAndFastVariant(t *testing.T) {
	cat := catalog.Catalog{Models: []catalog.Model{{
		Slug:                     "gpt-test",
		DisplayName:              "GPT Test",
		DefaultReasoningLevel:    "medium",
		SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "low"}, {Effort: "medium"}},
		AdditionalSpeedTiers:     []string{"fast"},
		ServiceTiers:             []catalog.ServiceTier{{ID: "priority", Name: "Fast"}},
	}}}
	got, err := ListModels(cat, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	canonicalIDs := []string{"gpt-test", "gpt-test:low", "gpt-test:medium", "gpt-test:fast", "gpt-test:low:fast", "gpt-test:medium:fast"}
	var wantIDs []string
	for _, canonical := range canonicalIDs {
		wantIDs = append(wantIDs, canonical, model.ClaudeCarrierID(canonical))
	}
	if len(got.Data) != len(wantIDs) || got.HasMore || got.FirstID != wantIDs[0] || got.LastID != wantIDs[len(wantIDs)-1] {
		t.Fatalf("list metadata = %#v", got)
	}
	for index, want := range wantIDs {
		if got.Data[index].Type != "model" || got.Data[index].ID != want || got.Data[index].DisplayName == "" {
			t.Errorf("model %d = %#v, want ID %q", index, got.Data[index], want)
		}
	}
}

func TestListModelsPaginatesClaudeGatewayQueryWithoutInventedModels(t *testing.T) {
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	all, err := ListModels(cat, url.Values{"limit": {"1000"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Data) <= len(cat.Models) || all.HasMore {
		t.Fatalf("all models = %d, catalog slugs = %d, has_more = %v", len(all.Data), len(cat.Models), all.HasMore)
	}

	page, err := ListModels(cat, url.Values{"limit": {"2"}, "after_id": {all.Data[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 2 || page.Data[0].ID != all.Data[1].ID || page.Data[1].ID != all.Data[2].ID || !page.HasMore {
		t.Fatalf("page = %#v", page)
	}
}

func TestListModelsBeforeIDReturnsThePageImmediatelyBeforeCursor(t *testing.T) {
	cat := catalog.Catalog{Models: []catalog.Model{{
		Slug:                     "gpt-test",
		DisplayName:              "GPT Test",
		DefaultReasoningLevel:    "medium",
		SupportedReasoningLevels: []catalog.ReasoningLevel{{Effort: "low"}, {Effort: "medium"}},
		AdditionalSpeedTiers:     []string{"fast"},
		ServiceTiers:             []catalog.ServiceTier{{ID: "priority", Name: "Fast"}},
	}}}
	all, err := ListModels(cat, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Data) < 5 {
		t.Fatalf("expandModels produced %d models, need at least 5", len(all.Data))
	}

	cursor := all.Data[len(all.Data)-1].ID
	page, err := ListModels(cat, url.Values{"limit": {"2"}, "before_id": {cursor}})
	if err != nil {
		t.Fatal(err)
	}
	want0 := all.Data[len(all.Data)-3].ID
	want1 := all.Data[len(all.Data)-2].ID
	if len(page.Data) != 2 || page.Data[0].ID != want0 || page.Data[1].ID != want1 || !page.HasMore {
		t.Fatalf("before_id page = %#v, want IDs %q, %q with has_more toward the start", page, want0, want1)
	}
	if page.FirstID != want0 || page.LastID != want1 {
		t.Fatalf("before_id cursors first=%q last=%q, want %q %q", page.FirstID, page.LastID, want0, want1)
	}

	fits, err := ListModels(cat, url.Values{"limit": {"2"}, "before_id": {all.Data[2].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fits.Data) != 2 || fits.Data[0].ID != all.Data[0].ID || fits.Data[1].ID != all.Data[1].ID || fits.HasMore {
		t.Fatalf("before_id prefix page = %#v, want the two items at the start with has_more=false", fits)
	}
}

func TestListModelsRejectsInvalidPagination(t *testing.T) {
	cat, err := catalog.LoadFallback()
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []url.Values{
		{"limit": {"0"}},
		{"limit": {"1001"}},
		{"limit": {"x"}},
		{"after_id": {"missing"}},
		{"after_id": {"x"}, "before_id": {"y"}},
	} {
		_, err := ListModels(cat, query)
		var requestErr *anthropic.RequestError
		if !errors.As(err, &requestErr) || requestErr.StatusCode != 400 || requestErr.Response.Error.Type != "invalid_request_error" {
			t.Fatalf("ListModels(%v) error = %#v", query, err)
		}
	}
}
