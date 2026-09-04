package codexstream

import (
	"encoding/json"
	"errors"
	"fmt"
)

var ErrUnexpectedEventType = errors.New("unexpected Codex event type")

// ResponseEvent carries response lifecycle and terminal payloads.
type ResponseEvent struct {
	Type           string          `json:"type"`
	SequenceNumber *int64          `json:"sequence_number,omitempty"`
	Response       json.RawMessage `json:"response,omitempty"`
}

// OutputItemEvent carries an added or completed Responses output item.
type OutputItemEvent struct {
	Type           string          `json:"type"`
	SequenceNumber *int64          `json:"sequence_number,omitempty"`
	OutputIndex    *int            `json:"output_index,omitempty"`
	Item           json.RawMessage `json:"item,omitempty"`
}

// TextEvent carries output text delta/done fields.
type TextEvent struct {
	Type           string `json:"type"`
	SequenceNumber *int64 `json:"sequence_number,omitempty"`
	ItemID         string `json:"item_id,omitempty"`
	OutputIndex    *int   `json:"output_index,omitempty"`
	ContentIndex   *int   `json:"content_index,omitempty"`
	Delta          string `json:"delta,omitempty"`
	Text           string `json:"text,omitempty"`
}

// ReasoningEvent carries summary-part, summary-text, and reasoning-text fields.
type ReasoningEvent struct {
	Type           string          `json:"type"`
	SequenceNumber *int64          `json:"sequence_number,omitempty"`
	ItemID         string          `json:"item_id,omitempty"`
	OutputIndex    *int            `json:"output_index,omitempty"`
	SummaryIndex   *int            `json:"summary_index,omitempty"`
	ContentIndex   *int            `json:"content_index,omitempty"`
	Delta          string          `json:"delta,omitempty"`
	Text           string          `json:"text,omitempty"`
	Part           json.RawMessage `json:"part,omitempty"`
}

// FunctionArgumentsEvent carries streamed or completed function arguments.
type FunctionArgumentsEvent struct {
	Type           string `json:"type"`
	SequenceNumber *int64 `json:"sequence_number,omitempty"`
	ItemID         string `json:"item_id,omitempty"`
	CallID         string `json:"call_id,omitempty"`
	OutputIndex    *int   `json:"output_index,omitempty"`
	Delta          string `json:"delta,omitempty"`
	Arguments      string `json:"arguments,omitempty"`
}

// ContentPartEvent carries message content-part boundaries.
type ContentPartEvent struct {
	Type           string          `json:"type"`
	SequenceNumber *int64          `json:"sequence_number,omitempty"`
	ItemID         string          `json:"item_id,omitempty"`
	OutputIndex    *int            `json:"output_index,omitempty"`
	ContentIndex   *int            `json:"content_index,omitempty"`
	Part           json.RawMessage `json:"part,omitempty"`
}

// RateLimitsEvent preserves the snapshot payload for policy and status handling.
type RateLimitsEvent struct {
	Type       string          `json:"type"`
	RateLimits json.RawMessage `json:"rate_limits,omitempty"`
	Credits    json.RawMessage `json:"credits,omitempty"`
}

// WebSearchEvent carries web-search lifecycle fields.
type WebSearchEvent struct {
	Type           string `json:"type"`
	SequenceNumber *int64 `json:"sequence_number,omitempty"`
	ItemID         string `json:"item_id,omitempty"`
	OutputIndex    *int   `json:"output_index,omitempty"`
}

func (e Event) DecodeResponse() (ResponseEvent, error) {
	var out ResponseEvent
	if err := e.decodeFamily(&out,
		"response.created", "response.in_progress", "response.completed", "response.done",
		"response.incomplete", "response.failed"); err != nil {
		return ResponseEvent{}, err
	}
	return out, nil
}

func (e Event) DecodeOutputItem() (OutputItemEvent, error) {
	var out OutputItemEvent
	if err := e.decodeFamily(&out, "response.output_item.added", "response.output_item.done"); err != nil {
		return OutputItemEvent{}, err
	}
	return out, nil
}

func (e Event) DecodeText() (TextEvent, error) {
	var out TextEvent
	if err := e.decodeFamily(&out, "response.output_text.delta", "response.output_text.done",
		"response.refusal.delta", "response.refusal.done"); err != nil {
		return TextEvent{}, err
	}
	return out, nil
}

func (e Event) DecodeReasoning() (ReasoningEvent, error) {
	var out ReasoningEvent
	if err := e.decodeFamily(&out,
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_text.delta", "response.reasoning_text.done"); err != nil {
		return ReasoningEvent{}, err
	}
	return out, nil
}

func (e Event) DecodeFunctionArguments() (FunctionArgumentsEvent, error) {
	var out FunctionArgumentsEvent
	if err := e.decodeFamily(&out,
		"response.function_call_arguments.delta", "response.function_call_arguments.done",
		"response.custom_tool_call_input.delta", "response.custom_tool_call_input.done"); err != nil {
		return FunctionArgumentsEvent{}, err
	}
	return out, nil
}

func (e Event) DecodeContentPart() (ContentPartEvent, error) {
	var out ContentPartEvent
	if err := e.decodeFamily(&out, "response.content_part.added", "response.content_part.done"); err != nil {
		return ContentPartEvent{}, err
	}
	return out, nil
}

func (e Event) DecodeRateLimits() (RateLimitsEvent, error) {
	var out RateLimitsEvent
	if err := e.decodeFamily(&out, "codex.rate_limits"); err != nil {
		return RateLimitsEvent{}, err
	}
	return out, nil
}

func (e Event) DecodeWebSearch() (WebSearchEvent, error) {
	var out WebSearchEvent
	if err := e.decodeFamily(&out,
		"response.web_search_call.in_progress", "response.web_search_call.searching",
		"response.web_search_call.completed", "response.web_search_call.failed"); err != nil {
		return WebSearchEvent{}, err
	}
	return out, nil
}

func (e Event) decodeFamily(dst any, allowed ...string) error {
	matched := false
	for _, candidate := range allowed {
		if e.Type == candidate {
			matched = true
			break
		}
	}
	if !matched {
		return fmt.Errorf("%w: %q", ErrUnexpectedEventType, e.Type)
	}
	if err := e.Decode(dst); err != nil {
		return err
	}
	return nil
}
