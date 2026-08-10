// Package model resolves external model IDs against the Codex model catalog.
package model

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Aotricx/Clodex/internal/catalog"
)

const (
	fastSuffix          = "fast"
	fastServiceTier     = "priority"
	claudeCarrierPrefix = "anthropic-clodex-"
	claudeCarrierSuffix = "[1m]"
	lowBudgetMax        = 2048
	mediumBudgetMax     = 8192
	highBudgetMax       = 16384
	xhighBudgetMax      = 32768
	maxBudgetMax        = 65536
)

// thinkingBudgetTierOrder is owner-approved vocabulary for ranking thinking
// budget thresholds and finding lower tiers. It is not an effort allowlist;
// every supported effort still comes from catalog.Model.SupportedReasoningLevels.
var thinkingBudgetTierOrder = []string{"low", "medium", "high", "xhigh", "max", "ultra"}

// Selection is one catalog-validated backend model choice.
type Selection struct {
	Model       catalog.Model
	Effort      string
	Fast        bool
	ServiceTier string
}

// CanonicalID returns the fully explicit catalog ID represented by Selection.
func (s Selection) CanonicalID() string {
	id := s.Model.Slug
	if s.Effort != "" {
		id += ":" + s.Effort
	}
	if s.Fast {
		id += ":fast"
	}
	return id
}

// ClaudeCarrierID mechanically wraps one canonical Clodex ID in a model name
// Claude Code discovers and treats as one-million-context capable. The actual
// auto-compaction window remains the catalog context supplied separately.
func ClaudeCarrierID(canonicalID string) string {
	return claudeCarrierPrefix + canonicalID + claudeCarrierSuffix
}

// CanonicalIDFromClaudeCarrier reverses ClaudeCarrierID. It performs no model
// policy; Resolve still parses and validates the recovered ID against catalog.
func CanonicalIDFromClaudeCarrier(carrierID string) (string, bool) {
	if !strings.HasPrefix(carrierID, claudeCarrierPrefix) || !strings.HasSuffix(carrierID, claudeCarrierSuffix) {
		return "", false
	}
	canonical := strings.TrimSuffix(strings.TrimPrefix(carrierID, claudeCarrierPrefix), claudeCarrierSuffix)
	if canonical == "" {
		return "", false
	}
	return canonical, true
}

type parsedID struct {
	slug   string
	effort string
	fast   bool
}

// Resolve selects a model and effort without applying model policy or aliases.
// Positive thinking budgets map to low through ultra at inclusive boundaries
// 2048, 8192, 16384, 32768, and 65536 tokens. Unsupported budget tiers project
// downward onto efforts advertised by the selected model.
func Resolve(cat catalog.Catalog, requestedID, defaultID string, thinkingBudget int) (Selection, error) {
	configured, err := parseID(defaultID)
	if err != nil {
		return Selection{}, fmt.Errorf("invalid default model ID %q: %w", defaultID, err)
	}
	defaultModel, ok := cat.Find(configured.slug)
	if !ok {
		return Selection{}, fmt.Errorf("default model %q is not present in catalog", configured.slug)
	}
	if configured.effort != "" && !supportsEffort(defaultModel, configured.effort) {
		return Selection{}, fmt.Errorf("default model %q does not support effort %q", configured.slug, configured.effort)
	}
	if configured.fast {
		if err := validateFast(defaultModel); err != nil {
			return Selection{}, fmt.Errorf("default model %q: %w", configured.slug, err)
		}
	}

	if canonical, ok := CanonicalIDFromClaudeCarrier(requestedID); ok {
		requestedID = canonical
	}
	requested, err := parseID(requestedID)
	if err != nil {
		return Selection{}, fmt.Errorf("invalid requested model ID %q: %w", requestedID, err)
	}
	selectedModel, recognized := cat.Find(requested.slug)
	fallback := strings.HasPrefix(requested.slug, "claude-") || !recognized
	if fallback {
		selectedModel = defaultModel
	}

	effort := requested.effort
	if effort == "" && fallback {
		effort = configured.effort
	}
	if effort == "" && thinkingBudget > 0 {
		effort, err = effortForBudget(selectedModel, thinkingBudget)
		if err != nil {
			return Selection{}, err
		}
	}
	if effort == "" {
		effort = selectedModel.DefaultReasoningLevel
	}
	if !supportsEffort(selectedModel, effort) {
		return Selection{}, fmt.Errorf("model %q does not support effort %q", selectedModel.Slug, effort)
	}

	effectiveFast := requested.fast || fallback && configured.fast
	selection := Selection{Model: selectedModel, Effort: effort, Fast: effectiveFast}
	if selection.Fast {
		if err := validateFast(selectedModel); err != nil {
			return Selection{}, fmt.Errorf("model %q: %w", selectedModel.Slug, err)
		}
		selection.ServiceTier = fastServiceTier
	}
	return selection, nil
}

// NearestSupportedFloor returns the highest catalog-advertised effort strictly
// below rejected. It supports the one-time backend effort-quirk retry and never
// returns the rejected level itself.
func NearestSupportedFloor(model catalog.Model, rejected string) (string, error) {
	rejectedRank, ok := effortRank(rejected)
	if !ok {
		return "", fmt.Errorf("cannot floor unknown effort %q", rejected)
	}
	bestRank := -1
	best := ""
	for _, level := range model.SupportedReasoningLevels {
		rank, known := effortRank(level.Effort)
		if known && rank < rejectedRank && rank > bestRank {
			bestRank = rank
			best = level.Effort
		}
	}
	if best == "" {
		return "", fmt.Errorf("model %q has no supported effort below %q", model.Slug, rejected)
	}
	return best, nil
}

func parseID(value string) (parsedID, error) {
	if value == "" {
		return parsedID{}, errors.New("model ID must not be empty")
	}
	if strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return parsedID{}, errors.New("model ID must not contain whitespace")
	}
	parts := strings.Split(value, ":")
	if len(parts) > 3 {
		return parsedID{}, errors.New("expected slug[:effort][:fast]")
	}
	for _, part := range parts {
		if part == "" {
			return parsedID{}, errors.New("model ID components must not be empty")
		}
	}
	parsed := parsedID{slug: parts[0]}
	if len(parts) == 1 {
		return parsed, nil
	}
	if len(parts) == 2 {
		if parts[1] == fastSuffix {
			parsed.fast = true
		} else {
			parsed.effort = parts[1]
		}
		return parsed, nil
	}
	if parts[1] == fastSuffix || parts[2] != fastSuffix {
		return parsedID{}, errors.New("fast must be the final suffix after an effort")
	}
	parsed.effort = parts[1]
	parsed.fast = true
	return parsed, nil
}

func supportsEffort(model catalog.Model, effort string) bool {
	for _, level := range model.SupportedReasoningLevels {
		if level.Effort == effort {
			return true
		}
	}
	return false
}

func validateFast(model catalog.Model) error {
	hasFast := false
	for _, tier := range model.AdditionalSpeedTiers {
		if tier == fastSuffix {
			hasFast = true
			break
		}
	}
	hasPriority := false
	for _, tier := range model.ServiceTiers {
		if tier.ID == fastServiceTier {
			hasPriority = true
			break
		}
	}
	if !hasFast || !hasPriority {
		return errors.New("fast requires advertised fast speed and priority service tiers")
	}
	return nil
}

func effortForBudget(model catalog.Model, budget int) (string, error) {
	target := "ultra"
	switch {
	case budget <= lowBudgetMax:
		target = "low"
	case budget <= mediumBudgetMax:
		target = "medium"
	case budget <= highBudgetMax:
		target = "high"
	case budget <= xhighBudgetMax:
		target = "xhigh"
	case budget <= maxBudgetMax:
		target = "max"
	}
	targetRank, _ := effortRank(target)
	bestRank := -1
	lowestRank := len(thinkingBudgetTierOrder)
	best := ""
	lowest := ""
	for _, level := range model.SupportedReasoningLevels {
		rank, known := effortRank(level.Effort)
		if !known {
			continue
		}
		if rank < lowestRank {
			lowestRank = rank
			lowest = level.Effort
		}
		if rank <= targetRank && rank > bestRank {
			bestRank = rank
			best = level.Effort
		}
	}
	if best != "" {
		return best, nil
	}
	if lowest != "" {
		return lowest, nil
	}
	return "", fmt.Errorf("model %q has no rankable reasoning efforts for thinking budget", model.Slug)
}

func effortRank(effort string) (int, bool) {
	for rank, candidate := range thinkingBudgetTierOrder {
		if candidate == effort {
			return rank, true
		}
	}
	return 0, false
}
