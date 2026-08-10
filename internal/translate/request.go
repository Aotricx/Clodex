package translate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"sort"
	"strings"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/model"
)

const (
	WarningMaxTokensUnsupported     = "request.max_tokens_unsupported"
	WarningTemperatureUnsupported   = "request.temperature_unsupported"
	WarningTopPUnsupported          = "request.top_p_unsupported"
	WarningMetadataUnsupported      = "request.metadata_unsupported"
	WarningUnknownRequestField      = "request.unknown_field"
	WarningUnknownContentBlock      = "content.unknown_block"
	WarningUnknownContentField      = "content.unknown_field"
	WarningUnknownImageSourceField  = "content.image.unknown_source_field"
	WarningUnknownTool              = "tool.unknown"
	WarningUnknownToolField         = "tool.unknown_field"
	WarningWebSearchUnmappableField = "tool.web_search.unmappable_field"
	WarningForeignThinkingSignature = "thinking.foreign_signature"
)

// Options carries per-session transport state that is not part of an
// Anthropic request. PromptCacheKey should normally be the stable thread ID.
type Options struct {
	PromptCacheKey string
}

// Warning is a stable translation-warning kind with an aggregate occurrence
// count. Aggregation keeps status output bounded without hiding frequency.
type Warning struct {
	Kind  string
	Count int
}

// Accounting proves every semantic unit observed by the translator was
// either mapped or warning-accounted.
type Accounting struct {
	Source int
	Mapped int
	Warned int
}

// Result contains the concrete upstream request plus controls enforced later
// by the response stream reducer.
type Result struct {
	Request       codexwire.Request
	StopSequences []string
	Warnings      []Warning
	Accounting    Accounting
}

type translator struct {
	selection model.Selection
	options   Options
	warnings  []Warning
	byKind    map[string]int
	account   Accounting
}

// TranslateRequest converts one validated Anthropic request into a stateless
// full-history Codex Responses request. Unsupported controls are omitted from
// the wire and returned as visible warnings.
func TranslateRequest(req *anthropic.MessageRequest, selection model.Selection, options Options) (Result, error) {
	if req == nil {
		return Result{}, fmt.Errorf("translate request: nil Anthropic request")
	}
	if selection.Model.Slug == "" {
		return Result{}, fmt.Errorf("translate request: selected model slug is empty")
	}
	t := &translator{
		selection: selection,
		options:   options,
		byKind:    make(map[string]int),
	}

	t.mapped() // model
	if req.MaxTokens > 0 {
		t.warn(WarningMaxTokensUnsupported)
	}
	if req.Temperature != nil {
		t.warn(WarningTemperatureUnsupported)
	}
	if req.TopP != nil {
		t.warn(WarningTopPUnsupported)
	}
	if req.Metadata != nil {
		t.warn(WarningMetadataUnsupported)
	}
	if len(req.StopSequences) > 0 {
		t.mapped()
	}
	if req.Thinking != nil {
		t.mapped()
	}
	if req.ToolChoice != nil {
		t.mapped()
	}
	if len(req.Extra) > 0 {
		for range req.Extra {
			t.warn(WarningUnknownRequestField)
		}
	}
	t.accountCacheControls(req)

	instructions := t.instructions(req.System)
	tools := t.tools(req.Tools)
	input := t.input(req.Messages)
	toolChoice := t.toolChoice(req.ToolChoice, req.Tools)
	parallel := selection.Model.SupportsParallelToolCalls
	if req.ToolChoice != nil && req.ToolChoice.DisableParallelToolUse != nil && *req.ToolChoice.DisableParallelToolUse {
		parallel = false
	}

	reasoning := (*codexwire.Reasoning)(nil)
	if selection.Effort != "" {
		effort := selection.Effort
		if effort == "ultra" {
			effort = "max"
		}
		reasoning = &codexwire.Reasoning{Effort: effort, Summary: "auto"}
	}

	wire := codexwire.Request{
		Model:             selection.Model.Slug,
		Instructions:      instructions,
		Input:             input,
		Tools:             tools,
		ToolChoice:        toolChoice,
		ParallelToolCalls: parallel,
		Reasoning:         reasoning,
		ServiceTier:       selection.ServiceTier,
		PromptCacheKey:    t.promptCacheKey(req),
	}
	if selection.Model.UseResponsesLite {
		wire.Input = t.responsesLiteInput(instructions, tools, input)
		wire.Instructions = ""
		wire.Tools = nil
		wire.ParallelToolCalls = false
		if wire.Reasoning == nil {
			wire.Reasoning = &codexwire.Reasoning{}
		}
		wire.Reasoning.Context = codexwire.ReasoningContextAllTurns
		stripImageDetails(wire.Input)
	}

	return Result{
		Request:       wire,
		StopSequences: append([]string(nil), req.StopSequences...),
		Warnings:      append([]Warning(nil), t.warnings...),
		Accounting:    t.account,
	}, nil
}

func (t *translator) mapped() {
	t.account.Source++
	t.account.Mapped++
}

func (t *translator) warn(kind string) {
	t.account.Source++
	t.account.Warned++
	if index, exists := t.byKind[kind]; exists {
		t.warnings[index].Count++
		return
	}
	t.byKind[kind] = len(t.warnings)
	t.warnings = append(t.warnings, Warning{Kind: kind, Count: 1})
}

func (t *translator) instructions(blocks []anthropic.ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text == nil {
			t.warn(WarningUnknownContentBlock)
			continue
		}
		t.mapped()
		t.warnUnknownBlockFields(block.Raw, "type", "text", "cache_control")
		parts = append(parts, block.Text.Text)
	}
	return strings.Join(parts, "\n\n")
}

func (t *translator) tools(source []anthropic.Tool) []codexwire.Tool {
	if source == nil {
		return nil
	}
	out := make([]codexwire.Tool, 0, len(source))
	for _, tool := range source {
		if !tool.Unknown {
			t.mapped()
			t.warnUnknownToolFields(tool.Raw, "type", "name", "description", "input_schema", "cache_control")
			out = append(out, codexwire.FunctionTool{
				Name:        tool.Name,
				Description: tool.Description,
				Strict:      false,
				Parameters:  cloneRaw(tool.InputSchema),
			})
			continue
		}
		if isWebSearchType(tool.Type) {
			t.mapped()
			out = append(out, t.webSearchTool(tool))
			continue
		}
		t.warn(WarningUnknownTool)
	}
	return out
}

func (t *translator) webSearchTool(tool anthropic.Tool) codexwire.Tool {
	var object map[string]json.RawMessage
	_ = json.Unmarshal(tool.Raw, &object)
	externalWebAccess := true
	result := codexwire.WebSearchTool{
		ExternalWebAccess:  &externalWebAccess,
		SearchContentTypes: []string{"text", "image"},
	}
	if raw, ok := object["external_web_access"]; ok {
		var value bool
		if json.Unmarshal(raw, &value) == nil {
			result.ExternalWebAccess = &value
		}
	}
	if raw, ok := object["allowed_domains"]; ok {
		_ = json.Unmarshal(raw, &result.AllowedDomains)
	}
	if raw, ok := object["search_content_types"]; ok {
		_ = json.Unmarshal(raw, &result.SearchContentTypes)
	}
	known := stringSet("type", "name", "external_web_access", "allowed_domains", "search_content_types", "cache_control")
	keys := sortedKeys(object)
	for _, key := range keys {
		if _, ok := known[key]; !ok {
			t.warn(WarningWebSearchUnmappableField)
		}
	}
	return result
}

func isWebSearchType(value string) bool {
	return strings.HasPrefix(value, "web_search_") || value == "web_search"
}

func (t *translator) toolChoice(choice *anthropic.ToolChoice, tools []anthropic.Tool) codexwire.ToolChoice {
	if choice == nil {
		return codexwire.ToolChoiceAuto
	}
	switch choice.Type {
	case "none":
		return codexwire.ToolChoiceNone
	case "any":
		return codexwire.ToolChoiceRequired
	case "tool":
		if namedWebSearch(tools, choice.Name) {
			return codexwire.WebSearchToolChoice{}
		}
		return codexwire.FunctionToolChoice{Name: choice.Name}
	default:
		return codexwire.ToolChoiceAuto
	}
}

func namedWebSearch(tools []anthropic.Tool, name string) bool {
	for _, tool := range tools {
		if !isWebSearchType(tool.Type) {
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(tool.Raw, &object) != nil {
			continue
		}
		var toolName string
		if json.Unmarshal(object["name"], &toolName) == nil && toolName == name {
			return true
		}
	}
	return false
}

func (t *translator) input(messages []anthropic.Message) []codexwire.InputItem {
	out := make([]codexwire.InputItem, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "user":
			t.userMessage(&out, message.Content)
		case "system":
			t.systemMessage(&out, message.Content)
		default:
			t.assistantMessage(&out, message.Content)
		}
	}
	return out
}

// systemMessage maps an in-array system entry (Claude Code 2.1.x agent-types
// listing and the like) onto the developer role — the same channel top-level
// system instructions ride — preserving its position in the conversation.
// Decode already restricts system content to text blocks.
func (t *translator) systemMessage(out *[]codexwire.InputItem, blocks []anthropic.ContentBlock) {
	parts := make([]codexwire.ContentItem, 0, len(blocks))
	for _, block := range blocks {
		if block.Text == nil {
			t.warn(WarningUnknownContentBlock)
			continue
		}
		t.mapped()
		t.warnUnknownBlockFields(block.Raw, "type", "text", "cache_control")
		parts = append(parts, codexwire.InputText{Text: block.Text.Text})
	}
	if len(parts) > 0 {
		*out = append(*out, codexwire.Message{Role: "developer", Content: parts})
	}
}

func (t *translator) userMessage(out *[]codexwire.InputItem, blocks []anthropic.ContentBlock) {
	parts := make([]codexwire.ContentItem, 0, len(blocks))
	flush := func() {
		if len(parts) == 0 {
			return
		}
		*out = append(*out, codexwire.Message{Role: "user", Content: parts})
		parts = nil
	}
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			t.mapped()
			t.warnUnknownBlockFields(block.Raw, "type", "text", "cache_control")
			parts = append(parts, codexwire.InputText{Text: block.Text.Text})
		case block.Image != nil:
			t.mapped()
			t.warnImageUnknownFields(block)
			parts = append(parts, t.image(block.Image))
		case block.ToolResult != nil:
			t.mapped()
			t.warnUnknownBlockFields(block.Raw, "type", "tool_use_id", "content", "is_error", "cache_control")
			flush()
			*out = append(*out, codexwire.FunctionCallOutput{
				CallID: block.ToolResult.ToolUseID,
				Output: t.toolResult(block.ToolResult),
			})
		default:
			t.warn(WarningUnknownContentBlock)
		}
	}
	flush()
}

func (t *translator) assistantMessage(out *[]codexwire.InputItem, blocks []anthropic.ContentBlock) {
	parts := make([]codexwire.ContentItem, 0, len(blocks))
	flush := func() {
		if len(parts) == 0 {
			return
		}
		*out = append(*out, codexwire.Message{Role: "assistant", Content: parts})
		parts = nil
	}
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			t.mapped()
			t.warnUnknownBlockFields(block.Raw, "type", "text", "cache_control")
			parts = append(parts, codexwire.OutputText{Text: block.Text.Text})
		case block.ToolUse != nil:
			t.mapped()
			t.warnUnknownBlockFields(block.Raw, "type", "id", "name", "input", "cache_control")
			flush()
			*out = append(*out, codexwire.FunctionCall{
				Name:      block.ToolUse.Name,
				Arguments: compactJSON(block.ToolUse.Input),
				CallID:    block.ToolUse.ID,
			})
		case block.Thinking != nil:
			t.mapped()
			t.warnUnknownBlockFields(block.Raw, "type", "thinking", "signature")
			flush()
			reasoning := codexwire.ReasoningItem{}
			if block.Thinking.Thinking != "" {
				reasoning.Summary = []codexwire.ReasoningSummary{codexwire.SummaryText{Text: block.Thinking.Thinking}}
			}
			if replay, ok := DecodeReasoningSignature(block.Thinking.Signature); ok {
				reasoning.EncryptedContent = stringPtr(replay.EncryptedContent)
			} else {
				t.warn(WarningForeignThinkingSignature)
			}
			*out = append(*out, reasoning)
		case block.RedactedThinking != nil:
			t.mapped()
			t.warnUnknownBlockFields(block.Raw, "type", "data")
			flush()
			*out = append(*out, codexwire.ReasoningItem{EncryptedContent: stringPtr(block.RedactedThinking.Data)})
		default:
			t.warn(WarningUnknownContentBlock)
		}
	}
	flush()
}

func (t *translator) toolResult(result *anthropic.ToolResultBlock) codexwire.FunctionOutput {
	items := make([]codexwire.FunctionOutputContentItem, 0, len(result.Content)+1)
	if result.IsError != nil && *result.IsError {
		items = append(items, codexwire.InputText{Text: "[tool execution error]"})
	}
	for _, block := range result.Content {
		switch {
		case block.Text != nil:
			t.mapped()
			t.warnUnknownBlockFields(block.Raw, "type", "text", "cache_control")
			items = append(items, codexwire.InputText{Text: block.Text.Text})
		case block.Image != nil:
			t.mapped()
			t.warnImageUnknownFields(block)
			items = append(items, t.image(block.Image))
		default:
			t.warn(WarningUnknownContentBlock)
		}
	}
	if len(items) == 0 {
		return codexwire.FunctionOutputText("")
	}
	if len(items) == 1 {
		if text, ok := items[0].(codexwire.InputText); ok {
			return codexwire.FunctionOutputText(text.Text)
		}
	}
	return codexwire.FunctionOutputContent(items)
}

func (t *translator) image(image *anthropic.ImageBlock) codexwire.InputImage {
	imageURL := image.Source.URL
	if image.Source.Type == "base64" {
		imageURL = "data:" + image.Source.MediaType + ";base64," + image.Source.Data
	}
	return codexwire.InputImage{ImageURL: imageURL, Detail: codexwire.ImageDetailAuto}
}

func (t *translator) responsesLiteInput(instructions string, tools []codexwire.Tool, input []codexwire.InputItem) []codexwire.InputItem {
	prefix := make([]codexwire.InputItem, 0, len(input)+2)
	prefix = append(prefix, codexwire.AdditionalTools{Role: "developer", Tools: tools})
	if instructions != "" {
		prefix = append(prefix, codexwire.Message{Role: "developer", Content: []codexwire.ContentItem{
			codexwire.InputText{Text: instructions},
		}})
	}
	return append(prefix, input...)
}

func stripImageDetails(items []codexwire.InputItem) {
	for _, item := range items {
		switch value := item.(type) {
		case codexwire.Message:
			for index, content := range value.Content {
				if image, ok := content.(codexwire.InputImage); ok {
					image.Detail = ""
					value.Content[index] = image
				}
			}
		case codexwire.FunctionCallOutput:
			content, ok := value.Output.(codexwire.FunctionOutputContent)
			if !ok {
				continue
			}
			for index, part := range content {
				if image, ok := part.(codexwire.InputImage); ok {
					image.Detail = ""
					content[index] = image
				}
			}
		}
	}
}

func (t *translator) warnUnknownBlockFields(raw json.RawMessage, known ...string) {
	t.warnUnknownFields(raw, WarningUnknownContentField, known...)
}

func (t *translator) warnUnknownToolFields(raw json.RawMessage, known ...string) {
	t.warnUnknownFields(raw, WarningUnknownToolField, known...)
}

func (t *translator) warnUnknownFields(raw json.RawMessage, kind string, known ...string) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return
	}
	knownSet := stringSet(known...)
	for key := range object {
		if _, ok := knownSet[key]; !ok {
			t.warn(kind)
		}
	}
}

func (t *translator) warnImageUnknownFields(block anthropic.ContentBlock) {
	t.warnUnknownBlockFields(block.Raw, "type", "source", "cache_control")
	var object map[string]json.RawMessage
	if json.Unmarshal(block.Raw, &object) != nil {
		return
	}
	var source map[string]json.RawMessage
	if json.Unmarshal(object["source"], &source) != nil {
		return
	}
	known := stringSet("type", "media_type", "data", "url")
	for key := range source {
		if _, ok := known[key]; !ok {
			t.warn(WarningUnknownImageSourceField)
		}
	}
}

func (t *translator) accountCacheControls(req *anthropic.MessageRequest) {
	if req.CacheControl != nil {
		t.mapped()
	}
	for _, block := range req.System {
		if blockCacheControl(block) != nil {
			t.mapped()
		}
	}
	for _, tool := range req.Tools {
		if tool.CacheControl != nil {
			t.mapped()
		}
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if blockCacheControl(block) != nil {
				t.mapped()
			}
			if block.ToolResult != nil {
				for _, nested := range block.ToolResult.Content {
					if blockCacheControl(nested) != nil {
						t.mapped()
					}
				}
			}
		}
	}
}

func blockCacheControl(block anthropic.ContentBlock) *anthropic.CacheControl {
	switch {
	case block.Text != nil:
		return block.Text.CacheControl
	case block.Image != nil:
		return block.Image.CacheControl
	case block.ToolUse != nil:
		return block.ToolUse.CacheControl
	case block.ToolResult != nil:
		return block.ToolResult.CacheControl
	default:
		return nil
	}
}

func (t *translator) promptCacheKey(req *anthropic.MessageRequest) string {
	if t.options.PromptCacheKey != "" {
		return t.options.PromptCacheKey
	}
	digest := sha256.New()
	hasCache := false
	write := func(raw json.RawMessage) {
		hasCache = true
		writeHashPart(digest, raw)
	}
	if req.CacheControl != nil {
		hasCache = true
		writeHashPart(digest, []byte(req.Model))
		for _, block := range req.System {
			writeHashPart(digest, block.Raw)
		}
		for _, tool := range req.Tools {
			writeHashPart(digest, tool.Raw)
		}
		if len(req.Messages) > 0 {
			for _, block := range req.Messages[0].Content {
				writeHashPart(digest, block.Raw)
			}
		}
	}
	for _, block := range req.System {
		if blockCacheControl(block) != nil {
			write(block.Raw)
		}
	}
	for _, tool := range req.Tools {
		if tool.CacheControl != nil {
			write(tool.Raw)
		}
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if blockCacheControl(block) != nil {
				write(block.Raw)
			}
			if block.ToolResult != nil {
				for _, nested := range block.ToolResult.Content {
					if blockCacheControl(nested) != nil {
						write(nested.Raw)
					}
				}
			}
		}
	}
	if !hasCache {
		return ""
	}
	sum := digest.Sum(nil)
	return "clodex-cache-v1-" + hex.EncodeToString(sum[:16])
}

func writeHashPart(digest hash.Hash, value []byte) {
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(value)
}

func compactJSON(raw json.RawMessage) string {
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		return string(raw)
	}
	return buffer.String()
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func stringPtr(value string) *string { return &value }

func stringSet(values ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
