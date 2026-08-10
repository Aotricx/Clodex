package server

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/catalog"
	clodexmodel "github.com/Aotricx/Clodex/internal/model"
)

const maxModelsPage = 1000

// Model is the Anthropic model-list representation. The Codex catalog does
// not publish creation timestamps, so Clodex does not fabricate one.
type Model struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

// ModelList is the Anthropic pagination envelope.
type ModelList struct {
	Data    []Model `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID string  `json:"first_id,omitempty"`
	LastID  string  `json:"last_id,omitempty"`
}

// ListModels expands every catalog slug into its valid effort and fast IDs,
// then applies Anthropic-style cursor pagination.
func ListModels(cat catalog.Catalog, query url.Values) (ModelList, error) {
	limit := maxModelsPage
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxModelsPage {
			return ModelList{}, invalidRequest("limit must be an integer between 1 and %d", maxModelsPage)
		}
		limit = parsed
	}
	afterID := query.Get("after_id")
	beforeID := query.Get("before_id")
	if afterID != "" && beforeID != "" {
		return ModelList{}, invalidRequest("after_id and before_id are mutually exclusive")
	}

	models := expandModels(cat)
	start, end := 0, len(models)
	if afterID != "" {
		index := modelIndex(models, afterID)
		if index < 0 {
			return ModelList{}, invalidRequest("after_id %q is not present in the model catalog", afterID)
		}
		start = index + 1
	}
	if beforeID != "" {
		index := modelIndex(models, beforeID)
		if index < 0 {
			return ModelList{}, invalidRequest("before_id %q is not present in the model catalog", beforeID)
		}
		end = index
	}
	if end < start {
		end = start
	}
	candidates := models[start:end]
	hasMore := len(candidates) > limit
	if hasMore {
		candidates = candidates[:limit]
	}
	data := append([]Model(nil), candidates...)
	result := ModelList{Data: data, HasMore: hasMore}
	if len(data) > 0 {
		result.FirstID = data[0].ID
		result.LastID = data[len(data)-1].ID
	}
	return result, nil
}

func expandModels(cat catalog.Catalog) []Model {
	var canonical []Model
	for _, source := range cat.Models {
		displayName := source.DisplayName
		if displayName == "" {
			displayName = source.Slug
		}
		canonical = append(canonical, Model{Type: "model", ID: source.Slug, DisplayName: displayName})
		for _, level := range source.SupportedReasoningLevels {
			canonical = append(canonical, Model{Type: "model", ID: source.Slug + ":" + level.Effort, DisplayName: variantName(displayName, level.Effort)})
		}
		if !modelHasFast(source) {
			continue
		}
		canonical = append(canonical, Model{Type: "model", ID: source.Slug + ":fast", DisplayName: variantName(displayName, "fast")})
		for _, level := range source.SupportedReasoningLevels {
			canonical = append(canonical, Model{Type: "model", ID: source.Slug + ":" + level.Effort + ":fast", DisplayName: variantName(displayName, level.Effort+", fast")})
		}
	}
	models := make([]Model, 0, len(canonical)*2)
	for _, entry := range canonical {
		models = append(models, entry)
		carrier := entry
		carrier.ID = clodexmodel.ClaudeCarrierID(entry.ID)
		models = append(models, carrier)
	}
	return models
}

func modelHasFast(model catalog.Model) bool {
	fast, priority := false, false
	for _, tier := range model.AdditionalSpeedTiers {
		fast = fast || tier == "fast"
	}
	for _, tier := range model.ServiceTiers {
		priority = priority || tier.ID == "priority"
	}
	return fast && priority
}

func variantName(displayName, variant string) string {
	return fmt.Sprintf("%s (%s)", displayName, variant)
}

func modelIndex(models []Model, id string) int {
	for index, model := range models {
		if model.ID == id {
			return index
		}
	}
	return -1
}

func invalidRequest(format string, values ...any) *anthropic.RequestError {
	message := strings.TrimSpace(fmt.Sprintf(format, values...))
	return &anthropic.RequestError{
		StatusCode: 400,
		Response: anthropic.ErrorResponse{
			Type:  "error",
			Error: anthropic.ErrorDetail{Type: "invalid_request_error", Message: message},
		},
	}
}
