// Package codexwire defines the concrete ChatGPT Codex Responses request wire.
package codexwire

import "encoding/json"

const (
	reasoningEncryptedContent = "reasoning.encrypted_content"
)

// InputItem is one ordered item in the stateless Responses history replay.
type InputItem interface {
	inputItem()
}

// Tool is one tool definition sent to the Responses endpoint.
type Tool interface {
	tool()
}

// ToolChoice is one live-proven Responses tool-selection form.
type ToolChoice interface {
	toolChoice()
}

// ToolChoiceMode is a string tool choice accepted by the Codex backend.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
)

func (ToolChoiceMode) toolChoice() {}

// FunctionToolChoice forces one named function tool.
type FunctionToolChoice struct {
	Name string
}

func (FunctionToolChoice) toolChoice() {}

func (choice FunctionToolChoice) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}{Type: "function", Name: choice.Name})
}

// WebSearchToolChoice forces the registered backend web-search tool. This
// exact allowed_tools shape is live-proven against the ChatGPT Codex endpoint.
type WebSearchToolChoice struct{}

func (WebSearchToolChoice) toolChoice() {}

func (WebSearchToolChoice) MarshalJSON() ([]byte, error) {
	return []byte(`{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}`), nil
}

// ReasoningContext controls which turns Responses Lite may reason across.
type ReasoningContext string

const ReasoningContextAllTurns ReasoningContext = "all_turns"

// Reasoning controls backend reasoning effort and summary generation.
type Reasoning struct {
	Effort  string           `json:"effort,omitempty"`
	Summary string           `json:"summary,omitempty"`
	Context ReasoningContext `json:"context,omitempty"`
}

// Request is the one concrete ChatGPT Codex Responses request. Store and tool
// choice are fixed by MarshalJSON to their pinned Codex values.
type Request struct {
	Model             string
	Instructions      string
	Input             []InputItem
	Tools             []Tool
	ToolChoice        ToolChoice
	ParallelToolCalls bool
	Reasoning         *Reasoning
	// Stream is retained as input compatibility but deliberately ignored:
	// Clodex always consumes upstream SSE, including for buffered Anthropic calls.
	Stream         bool
	ServiceTier    string
	PromptCacheKey string
	Text           *TextControls
	ClientMetadata map[string]string
}

// MarshalJSON fixes ChatGPT-only invariants and preserves pinned field order.
func (r Request) MarshalJSON() ([]byte, error) {
	input := r.Input
	if input == nil {
		input = []InputItem{}
	}
	include := []string{}
	if r.Reasoning != nil {
		include = append(include, reasoningEncryptedContent)
	}
	toolChoice := r.ToolChoice
	if toolChoice == nil {
		toolChoice = ToolChoiceAuto
	}

	wire := struct {
		Model             string             `json:"model"`
		Instructions      string             `json:"instructions,omitempty"`
		Input             []InputItem        `json:"input"`
		Tools             *[]Tool            `json:"tools,omitempty"`
		ToolChoice        ToolChoice         `json:"tool_choice"`
		ParallelToolCalls bool               `json:"parallel_tool_calls"`
		Reasoning         *Reasoning         `json:"reasoning"`
		Store             bool               `json:"store"`
		Stream            bool               `json:"stream"`
		Include           []string           `json:"include"`
		ServiceTier       string             `json:"service_tier,omitempty"`
		PromptCacheKey    string             `json:"prompt_cache_key,omitempty"`
		Text              *TextControls      `json:"text,omitempty"`
		ClientMetadata    *map[string]string `json:"client_metadata,omitempty"`
	}{
		Model:             r.Model,
		Instructions:      r.Instructions,
		Input:             input,
		ToolChoice:        toolChoice,
		ParallelToolCalls: r.ParallelToolCalls,
		Reasoning:         r.Reasoning,
		Store:             false,
		Stream:            true,
		Include:           include,
		ServiceTier:       r.ServiceTier,
		PromptCacheKey:    r.PromptCacheKey,
		Text:              r.Text,
	}
	if r.Tools != nil {
		wire.Tools = &r.Tools
	}
	if r.ClientMetadata != nil {
		wire.ClientMetadata = &r.ClientMetadata
	}
	return json.Marshal(wire)
}
