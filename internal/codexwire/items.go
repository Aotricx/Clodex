package codexwire

import "encoding/json"

// ImageDetail is the Responses input_image detail value.
type ImageDetail string

const (
	ImageDetailAuto     ImageDetail = "auto"
	ImageDetailLow      ImageDetail = "low"
	ImageDetailHigh     ImageDetail = "high"
	ImageDetailOriginal ImageDetail = "original"
)

// ContentItem is message content in request history.
type ContentItem interface {
	contentItem()
}

// ReasoningSummary is one reasoning summary part.
type ReasoningSummary interface {
	reasoningSummary()
}

// ReasoningContent is raw reasoning content retained in request history.
type ReasoningContent interface {
	reasoningContent()
}

// FunctionOutput is the function_call_output output wire value.
type FunctionOutput interface {
	functionOutput()
}

// FunctionOutputContentItem is structured function result content.
type FunctionOutputContentItem interface {
	functionOutputContentItem()
}

// AdditionalTools is the Responses Lite developer-prefix tool item.
type AdditionalTools struct {
	Role  string
	Tools []Tool
}

func (AdditionalTools) inputItem() {}

func (a AdditionalTools) MarshalJSON() ([]byte, error) {
	tools := a.Tools
	if tools == nil {
		tools = []Tool{}
	}
	return json.Marshal(struct {
		Type  string `json:"type"`
		Role  string `json:"role"`
		Tools []Tool `json:"tools"`
	}{
		Type:  "additional_tools",
		Role:  a.Role,
		Tools: tools,
	})
}

// Message is a user, developer, or assistant history message.
type Message struct {
	Role    string
	Content []ContentItem
}

func (Message) inputItem() {}

func (m Message) MarshalJSON() ([]byte, error) {
	content := m.Content
	if content == nil {
		content = []ContentItem{}
	}
	return json.Marshal(struct {
		Type    string        `json:"type"`
		Role    string        `json:"role"`
		Content []ContentItem `json:"content"`
	}{
		Type:    "message",
		Role:    m.Role,
		Content: content,
	})
}

// InputText is user or developer text content.
type InputText struct {
	Text string
}

func (InputText) contentItem() {}

func (InputText) functionOutputContentItem() {}

func (i InputText) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "input_text", Text: i.Text})
}

// InputImage is URL or data-URL image content.
type InputImage struct {
	ImageURL string
	Detail   ImageDetail
}

func (InputImage) contentItem() {}

func (InputImage) functionOutputContentItem() {}

func (i InputImage) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     string      `json:"type"`
		ImageURL string      `json:"image_url"`
		Detail   ImageDetail `json:"detail,omitempty"`
	}{Type: "input_image", ImageURL: i.ImageURL, Detail: i.Detail})
}

// OutputText is assistant text retained in request history.
type OutputText struct {
	Text string
}

func (OutputText) contentItem() {}

func (o OutputText) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "output_text", Text: o.Text})
}

// SummaryText is text within a reasoning item's summary.
type SummaryText struct {
	Text string
}

func (SummaryText) reasoningSummary() {}

func (s SummaryText) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "summary_text", Text: s.Text})
}

// ReasoningText is optional raw reasoning retained by the Responses API.
type ReasoningText struct {
	Text string
}

func (ReasoningText) reasoningContent() {}

func (r ReasoningText) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}{Type: "reasoning_text", Text: r.Text})
}

// ReasoningItem preserves reasoning continuity across stateless requests.
type ReasoningItem struct {
	Summary          []ReasoningSummary
	Content          []ReasoningContent
	EncryptedContent *string
}

func (ReasoningItem) inputItem() {}

func (r ReasoningItem) MarshalJSON() ([]byte, error) {
	summary := r.Summary
	if summary == nil {
		summary = []ReasoningSummary{}
	}
	return json.Marshal(struct {
		Type             string             `json:"type"`
		Summary          []ReasoningSummary `json:"summary"`
		Content          []ReasoningContent `json:"content,omitempty"`
		EncryptedContent *string            `json:"encrypted_content"`
	}{
		Type:             "reasoning",
		Summary:          summary,
		Content:          r.Content,
		EncryptedContent: r.EncryptedContent,
	})
}

// FunctionCall is an assistant function invocation retained in history.
type FunctionCall struct {
	Name      string
	Arguments string
	CallID    string
}

func (FunctionCall) inputItem() {}

func (f FunctionCall) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		CallID    string `json:"call_id"`
	}{
		Type:      "function_call",
		Name:      f.Name,
		Arguments: f.Arguments,
		CallID:    f.CallID,
	})
}

// FunctionCallOutput is a tool result retained in history.
type FunctionCallOutput struct {
	CallID string
	Output FunctionOutput
}

func (FunctionCallOutput) inputItem() {}

func (f FunctionCallOutput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type   string         `json:"type"`
		CallID string         `json:"call_id"`
		Output FunctionOutput `json:"output"`
	}{
		Type:   "function_call_output",
		CallID: f.CallID,
		Output: f.Output,
	})
}

// FunctionOutputText serializes as the plain string function output form.
type FunctionOutputText string

func (FunctionOutputText) functionOutput() {}

// FunctionOutputContent serializes as the ordered structured output array.
type FunctionOutputContent []FunctionOutputContentItem

func (FunctionOutputContent) functionOutput() {}

// EncryptedContent is encrypted structured function output content.
type EncryptedContent struct {
	EncryptedContent string
}

func (EncryptedContent) functionOutputContentItem() {}

func (e EncryptedContent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type             string `json:"type"`
		EncryptedContent string `json:"encrypted_content"`
	}{Type: "encrypted_content", EncryptedContent: e.EncryptedContent})
}
