// Package reducer converts ordered Codex stream events into neutral Anthropic
// message semantics shared by streaming and buffered response renderers.
package reducer

import (
	"encoding/json"
	"errors"
)

var (
	ErrNotTerminal               = errors.New("Codex response is not terminal")
	ErrEmptyCompletion           = errors.New("Codex completed without producing output")
	ErrMissingUsage              = errors.New("Codex terminal response is missing usage")
	ErrUpstreamFailed            = errors.New("Codex response failed")
	ErrContradictoryTerminal     = errors.New("Codex stream contains contradictory terminal events")
	ErrEventAfterTerminal        = errors.New("Codex stream contains an event after termination")
	ErrMalformedContent          = errors.New("Codex stream contains malformed content")
	ErrMissingReasoningSignature = errors.New("Codex reasoning item is missing encrypted content")
)

type Kind int

const (
	KindContentStart Kind = iota + 1
	KindContentDelta
	KindContentStop
	KindWarning
	KindRateLimits
	KindTerminal
)

type BlockType string

const (
	BlockThinking      BlockType = "thinking"
	BlockText          BlockType = "text"
	BlockToolUse       BlockType = "tool_use"
	BlockServerToolUse BlockType = "server_tool_use"
)

type DeltaType string

const (
	DeltaThinking  DeltaType = "thinking_delta"
	DeltaSignature DeltaType = "signature_delta"
	DeltaText      DeltaType = "text_delta"
	DeltaInputJSON DeltaType = "input_json_delta"
)

type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopToolUse      StopReason = "tool_use"
	StopMaxTokens    StopReason = "max_tokens"
	StopStopSequence StopReason = "stop_sequence"
	StopRefusal      StopReason = "refusal"
)

const (
	WarningUnknownServerTool = "response.unknown_server_tool"
	WarningUnknownEvent      = "response.unknown_event"
	WarningWebSearchAction   = "response.web_search.unmappable_action"
)

type ContentBlock struct {
	Type      BlockType
	Thinking  string
	Signature string
	Text      string
	ID        string
	Name      string
	Input     json.RawMessage
}

type Delta struct {
	Type DeltaType
	Text string
}

type Usage struct {
	InputTokens              int64
	OutputTokens             int64
	CacheReadInputTokens     int64
	CacheCreationInputTokens int64
}

type Terminal struct {
	StopReason   StopReason
	ResponseID   string
	Usage        Usage
	StopSequence string
}

type Warning struct {
	Kind  string
	Count int
}

type RateLimitSnapshot struct {
	Raw json.RawMessage
}

type Event struct {
	Kind       Kind
	Index      int
	Block      ContentBlock
	Delta      Delta
	Warning    *Warning
	RateLimits *RateLimitSnapshot
	Terminal   *Terminal
}

type Result struct {
	Content            []ContentBlock
	StopReason         StopReason
	StopSequence       string
	Usage              Usage
	ResponseID         string
	Warnings           []Warning
	RateLimitSnapshots []json.RawMessage
}
