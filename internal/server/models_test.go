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
