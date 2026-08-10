// Package requestprep performs the shared catalog/model/translation step used
// by both Messages generation and offline token counting.
package requestprep

import (
	"errors"
	"fmt"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/model"
	"github.com/Aotricx/Clodex/internal/translate"
)

// Prepared is one catalog-grounded, fully translated request.
type Prepared struct {
	Selection   model.Selection
	Translation translate.Result
}

// Prepare resolves the Anthropic-facing model ID and translates the complete
// stateless request. defaultModel is the owner-selected CLODEX_MODEL value;
// promptCacheKey should be the stable Claude session ID when available.
func Prepare(cat catalog.Catalog, req *anthropic.MessageRequest, defaultModel, promptCacheKey string) (Prepared, error) {
	if req == nil {
		return Prepared{}, errors.New("prepare request: nil Anthropic request")
	}
	thinkingBudget := 0
	if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
		thinkingBudget = boundedInt(req.Thinking.BudgetTokens)
	}
	selection, err := model.Resolve(cat, req.Model, defaultModel, thinkingBudget)
	if err != nil {
		return Prepared{}, fmt.Errorf("prepare request: resolve model: %w", err)
	}
	translated, err := translate.TranslateRequest(req, selection, translate.Options{PromptCacheKey: promptCacheKey})
	if err != nil {
		return Prepared{}, fmt.Errorf("prepare request: translate: %w", err)
	}
	return Prepared{Selection: selection, Translation: translated}, nil
}

func boundedInt(value int64) int {
	maxInt := int(^uint(0) >> 1)
	if value > int64(maxInt) {
		return maxInt
	}
	return int(value)
}
