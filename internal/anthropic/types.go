// Package anthropic defines the validated Anthropic Messages wire model used by
// the proxy boundary.
package anthropic

import (
	"encoding/json"
	"fmt"
)

// MessageRequest is one decoded Anthropic Messages request. Ordered content is
// represented by slices; raw JSON remains available wherever tolerant decoding
// may need a later translation warning.
type MessageRequest struct {
	Model         string
	MaxTokens     int64
	Messages      []Message
	System        []ContentBlock
	Tools         []Tool
	ToolChoice    *ToolChoice
	Thinking      *ThinkingConfig
	StopSequences []string
	Temperature   *float64
	TopP          *float64
	Stream        bool
	Metadata      *Metadata
	CacheControl  *CacheControl
	Extra         map[string]json.RawMessage
	Raw           json.RawMessage
}

// Message is one user or assistant turn.
type Message struct {
	Role    string
	Content []ContentBlock
	Raw     json.RawMessage
}

// ContentBlock is the ordered content union. Unknown and server-side variants
// have only Type and Raw populated so translation can map or warn later.
type ContentBlock struct {
	Type             string
	Text             *TextBlock
	Image            *ImageBlock
	ToolUse          *ToolUseBlock
	ToolResult       *ToolResultBlock
	Thinking         *ThinkingBlock
	RedactedThinking *RedactedThinkingBlock
	Raw              json.RawMessage
}

// Unknown reports whether a block has no locally decoded semantic variant.
func (b ContentBlock) Unknown() bool {
	return b.Text == nil && b.Image == nil && b.ToolUse == nil && b.ToolResult == nil && b.Thinking == nil && b.RedactedThinking == nil
}

type TextBlock struct {
	Text         string
	CacheControl *CacheControl
}

type ImageBlock struct {
	Source       ImageSource
	CacheControl *CacheControl
}

type ImageSource struct {
	Type      string
	MediaType string
	Data      string
	URL       string
}

type ToolUseBlock struct {
	ID           string
	Name         string
	Input        json.RawMessage
	CacheControl *CacheControl
}

type ToolResultBlock struct {
	ToolUseID    string
	Content      []ContentBlock
	IsError      *bool
	CacheControl *CacheControl
}

type ThinkingBlock struct {
	Thinking  string
	Signature string
}

type RedactedThinkingBlock struct {
	Data string
}

// Tool represents either a decoded custom tool or an opaque server tool.
type Tool struct {
	Type         string
	Name         string
	Description  string
	InputSchema  json.RawMessage
	CacheControl *CacheControl
	Unknown      bool
	Raw          json.RawMessage
}

type ToolChoice struct {
	Type                   string
	Name                   string
	DisableParallelToolUse *bool
	Raw                    json.RawMessage
}

type ThinkingConfig struct {
	Type         string
	BudgetTokens int64
	Display      string
	Raw          json.RawMessage
}

type Metadata struct {
	UserID string
	Raw    json.RawMessage
}

type CacheControl struct {
	Type string
	TTL  string
	Raw  json.RawMessage
}

// ErrorResponse is the Anthropic JSON error envelope.
type ErrorResponse struct {
	Type  string      `json:"type"`
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// RequestError is a decoder/validation failure ready for an HTTP handler.
type RequestError struct {
	StatusCode int
	Response   ErrorResponse
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("anthropic request: %s", e.Response.Error.Message)
}
