package anthropic

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var knownRequestFields = map[string]struct{}{
	"model": {}, "max_tokens": {}, "messages": {}, "system": {}, "tools": {},
	"tool_choice": {}, "thinking": {}, "stop_sequences": {}, "temperature": {},
	"top_p": {}, "stream": {}, "metadata": {}, "cache_control": {},
}

// DecodeRequest decodes and validates one Anthropic Messages request. Unknown
// JSON fields and unknown/server content variants remain available for later
// translation warnings.
func DecodeRequest(r io.Reader) (*MessageRequest, error) {
	return decodeRequest(r, true)
}

// DecodeCountTokensRequest decodes the token-counting request shape emitted by
// Claude Code. It is identical to Messages input except max_tokens is optional.
func DecodeCountTokensRequest(r io.Reader) (*MessageRequest, error) {
	return decodeRequest(r, false)
}

func decodeRequest(r io.Reader, requireMaxTokens bool) (*MessageRequest, error) {
	decoder := json.NewDecoder(r)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		return nil, invalidRequest("body must contain valid JSON: %v", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, invalidRequest("body must contain exactly one JSON value")
		}
		return nil, invalidRequest("body has invalid trailing data: %v", err)
	}

	object, err := decodeObject(raw, "request")
	if err != nil {
		return nil, err
	}
	req := &MessageRequest{Raw: cloneRaw(raw), Extra: make(map[string]json.RawMessage)}

	if req.Model, err = requiredNonemptyString(object, "model", "model"); err != nil {
		return nil, err
	}
	maxTokensRaw, ok := object["max_tokens"]
	if !ok && requireMaxTokens {
		return nil, invalidRequest("max_tokens is required")
	}
	if ok {
		if isNull(maxTokensRaw) || json.Unmarshal(maxTokensRaw, &req.MaxTokens) != nil {
			return nil, invalidRequest("max_tokens must be an integer")
		}
		if req.MaxTokens <= 0 {
			return nil, invalidRequest("max_tokens must be positive")
		}
	}

	messagesRaw, ok := object["messages"]
	if !ok {
		return nil, invalidRequest("messages is required")
	}
	var messageValues []json.RawMessage
	if isNull(messagesRaw) || json.Unmarshal(messagesRaw, &messageValues) != nil {
		return nil, invalidRequest("messages must be an array")
	}
	if len(messageValues) == 0 {
		return nil, invalidRequest("messages must not be empty")
	}
	req.Messages = make([]Message, len(messageValues))
	for i, value := range messageValues {
		message, decodeErr := decodeMessage(value, fmt.Sprintf("messages[%d]", i))
		if decodeErr != nil {
			return nil, decodeErr
		}
		req.Messages[i] = message
	}

	if systemRaw, present := object["system"]; present {
		if req.System, err = decodeContent(systemRaw, "system"); err != nil {
			return nil, err
		}
		if len(req.System) == 0 {
			return nil, invalidRequest("system must not be empty")
		}
		if err := validateContentPlacement(req.System, "system", "system"); err != nil {
			return nil, err
		}
	}
	if toolsRaw, present := object["tools"]; present {
		if req.Tools, err = decodeTools(toolsRaw); err != nil {
			return nil, err
		}
	}
	if choiceRaw, present := object["tool_choice"]; present {
		if req.ToolChoice, err = decodeToolChoice(choiceRaw); err != nil {
			return nil, err
		}
	}
	if thinkingRaw, present := object["thinking"]; present {
		if req.Thinking, err = decodeThinkingConfig(thinkingRaw); err != nil {
			return nil, err
		}
	}
	if stopRaw, present := object["stop_sequences"]; present {
		var elements []json.RawMessage
		if isNull(stopRaw) || json.Unmarshal(stopRaw, &elements) != nil {
			return nil, invalidRequest("stop_sequences must be an array of strings")
		}
		req.StopSequences = make([]string, len(elements))
		for i, element := range elements {
			if isNull(element) || json.Unmarshal(element, &req.StopSequences[i]) != nil || req.StopSequences[i] == "" {
				return nil, invalidRequest("stop_sequences must be an array of strings")
			}
		}
	}
	if temperatureRaw, present := object["temperature"]; present {
		if req.Temperature, err = decodeProbability(temperatureRaw, "temperature"); err != nil {
			return nil, err
		}
	}
	if topPRaw, present := object["top_p"]; present {
		if req.TopP, err = decodeProbability(topPRaw, "top_p"); err != nil {
			return nil, err
		}
	}
	if streamRaw, present := object["stream"]; present {
		if isNull(streamRaw) || json.Unmarshal(streamRaw, &req.Stream) != nil {
			return nil, invalidRequest("stream must be a boolean")
		}
	}
	if metadataRaw, present := object["metadata"]; present {
		if req.Metadata, err = decodeMetadata(metadataRaw); err != nil {
			return nil, err
		}
	}
	if req.CacheControl, err = optionalCacheControl(object, "cache_control", "cache_control"); err != nil {
		return nil, err
	}
	for key, value := range object {
		if _, known := knownRequestFields[key]; !known {
			req.Extra[key] = cloneRaw(value)
		}
	}
	return req, nil
}

func decodeMessage(raw json.RawMessage, path string) (Message, error) {
	object, err := decodeObject(raw, path)
	if err != nil {
		return Message{}, err
	}
	role, err := requiredNonemptyString(object, "role", path+".role")
	if err != nil {
		return Message{}, err
	}
	// "system" appears INSIDE messages[] from Claude Code 2.1.x (e.g. the
	// agent-types listing in headless mode); the real Anthropic API accepts
	// it, so the proxy must too. Content rules follow the system placement
	// (text only), enforced by validateContentPlacement below.
	if role != "user" && role != "assistant" && role != "system" {
		return Message{}, invalidRequest("%s must be user, assistant, or system", path+".role")
	}
	contentRaw, ok := object["content"]
	if !ok {
		return Message{}, invalidRequest("%s is required", path+".content")
	}
	content, err := decodeContent(contentRaw, path+".content")
	if err != nil {
		return Message{}, err
	}
	if len(content) == 0 {
		return Message{}, invalidRequest("%s must not be empty", path+".content")
	}
	if err := validateContentPlacement(content, role, path+".content"); err != nil {
		return Message{}, err
	}
	return Message{Role: role, Content: content, Raw: cloneRaw(raw)}, nil
}

func decodeContent(raw json.RawMessage, path string) ([]ContentBlock, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, invalidRequest("%s must be a string or content-block array", path)
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, invalidRequest("%s must be a string or content-block array", path)
		}
		blockRaw, _ := json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: text})
		return []ContentBlock{{Type: "text", Text: &TextBlock{Text: text}, Raw: blockRaw}}, nil
	}
	var values []json.RawMessage
	if isNull(raw) || json.Unmarshal(raw, &values) != nil {
		return nil, invalidRequest("%s must be a string or content-block array", path)
	}
	blocks := make([]ContentBlock, len(values))
	for i, value := range values {
		block, err := decodeContentBlock(value, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, err
		}
		blocks[i] = block
	}
	return blocks, nil
}

func decodeContentBlock(raw json.RawMessage, path string) (ContentBlock, error) {
	object, err := decodeObject(raw, path)
	if err != nil {
		return ContentBlock{}, err
	}
	blockType, err := requiredNonemptyString(object, "type", path+".type")
	if err != nil {
		return ContentBlock{}, err
	}
	decodedType := strings.ToLower(blockType)
	block := ContentBlock{Type: blockType, Raw: cloneRaw(raw)}
	switch decodedType {
	case "text":
		text, err := requiredString(object, "text", path+".text")
		if err != nil {
			return ContentBlock{}, err
		}
		cache, err := optionalCacheControl(object, "cache_control", path+".cache_control")
		if err != nil {
			return ContentBlock{}, err
		}
		block.Text = &TextBlock{Text: text, CacheControl: cache}
	case "image":
		image, err := decodeImageBlock(object, path)
		if err != nil {
			return ContentBlock{}, err
		}
		block.Image = image
	case "document":
		document, err := decodeDocumentBlock(object, path)
		if err != nil {
			return ContentBlock{}, err
		}
		block.Document = document
	case "search_result":
		searchResult, err := decodeSearchResultBlock(object, path)
		if err != nil {
			return ContentBlock{}, err
		}
		block.SearchResult = searchResult
	case "tool_use":
		toolUse, err := decodeToolUseBlock(object, path)
		if err != nil {
			return ContentBlock{}, err
		}
		block.ToolUse = toolUse
	case "tool_result":
		toolResult, err := decodeToolResultBlock(object, path)
		if err != nil {
			return ContentBlock{}, err
		}
		block.ToolResult = toolResult
	case "thinking":
		thinking, err := requiredString(object, "thinking", path+".thinking")
		if err != nil {
			return ContentBlock{}, err
		}
		signature, err := requiredNonemptyString(object, "signature", path+".signature")
		if err != nil {
			return ContentBlock{}, err
		}
		block.Thinking = &ThinkingBlock{Thinking: thinking, Signature: signature}
	case "redacted_thinking":
		data, err := requiredNonemptyString(object, "data", path+".data")
		if err != nil {
			return ContentBlock{}, err
		}
		block.RedactedThinking = &RedactedThinkingBlock{Data: data}
	}
	if !block.Unknown() {
		block.Type = decodedType
	}
	return block, nil
}

func decodeImageBlock(object map[string]json.RawMessage, path string) (*ImageBlock, error) {
	source, err := decodeBase64OrURLSource(object, path, supportedImageMediaType)
	if err != nil {
		return nil, err
	}
	cache, err := optionalCacheControl(object, "cache_control", path+".cache_control")
	if err != nil {
		return nil, err
	}
	return &ImageBlock{Source: ImageSource(source), CacheControl: cache}, nil
}

func decodeDocumentBlock(object map[string]json.RawMessage, path string) (*DocumentBlock, error) {
	source, err := decodeBase64OrURLSource(object, path, supportedDocumentMediaType)
	if err != nil {
		return nil, err
	}
	document := &DocumentBlock{Source: source}
	if titleRaw, present := object["title"]; present && !isNull(titleRaw) {
		if json.Unmarshal(titleRaw, &document.Title) != nil {
			return nil, invalidRequest("%s.title must be a string", path)
		}
	}
	document.CacheControl, err = optionalCacheControl(object, "cache_control", path+".cache_control")
	if err != nil {
		return nil, err
	}
	return document, nil
}

func decodeSearchResultBlock(object map[string]json.RawMessage, path string) (*SearchResultBlock, error) {
	source, err := requiredNonemptyString(object, "source", path+".source")
	if err != nil {
		return nil, err
	}
	title, err := requiredString(object, "title", path+".title")
	if err != nil {
		return nil, err
	}
	contentRaw, ok := object["content"]
	if !ok {
		return nil, invalidRequest("%s.content is required", path)
	}
	content, err := decodeContent(contentRaw, path+".content")
	if err != nil {
		return nil, err
	}
	if err := validateContentPlacement(content, "search_result", path+".content"); err != nil {
		return nil, err
	}
	cache, err := optionalCacheControl(object, "cache_control", path+".cache_control")
	if err != nil {
		return nil, err
	}
	return &SearchResultBlock{Source: source, Title: title, Content: content, CacheControl: cache}, nil
}

func decodeBase64OrURLSource(object map[string]json.RawMessage, path string, supportedMedia func(string) bool) (DocumentSource, error) {
	sourceRaw, ok := object["source"]
	if !ok {
		return DocumentSource{}, invalidRequest("%s.source is required", path)
	}
	sourceObject, err := decodeObject(sourceRaw, path+".source")
	if err != nil {
		return DocumentSource{}, err
	}
	sourceType, err := requiredNonemptyString(sourceObject, "type", path+".source.type")
	if err != nil {
		return DocumentSource{}, err
	}
	source := DocumentSource{Type: sourceType}
	switch sourceType {
	case "base64":
		source.MediaType, err = requiredNonemptyString(sourceObject, "media_type", path+".source.media_type")
		if err != nil {
			return DocumentSource{}, err
		}
		if !supportedMedia(source.MediaType) {
			return DocumentSource{}, invalidRequest("%s.source.media_type is unsupported", path)
		}
		source.Data, err = requiredNonemptyString(sourceObject, "data", path+".source.data")
		if err != nil {
			return DocumentSource{}, err
		}
		if strings.ContainsAny(source.Data, " \t\r\n") {
			return DocumentSource{}, invalidRequest("%s.source.data must be strict base64", path)
		}
		if _, err := base64.StdEncoding.Strict().DecodeString(source.Data); err != nil {
			return DocumentSource{}, invalidRequest("%s.source.data must be strict base64: %v", path, err)
		}
	case "url":
		source.URL, err = requiredNonemptyString(sourceObject, "url", path+".source.url")
		if err != nil {
			return DocumentSource{}, err
		}
		parsed, parseErr := url.Parse(source.URL)
		if parseErr != nil || !(strings.EqualFold(parsed.Scheme, "http") || strings.EqualFold(parsed.Scheme, "https")) || parsed.Host == "" || parsed.Hostname() == "" {
			return DocumentSource{}, invalidRequest("%s.source.url must be an absolute HTTP or HTTPS URL", path)
		}
	default:
		return DocumentSource{}, invalidRequest("%s.source.type %q is unsupported", path, sourceType)
	}
	return source, nil
}

func decodeToolUseBlock(object map[string]json.RawMessage, path string) (*ToolUseBlock, error) {
	id, err := requiredNonemptyString(object, "id", path+".id")
	if err != nil {
		return nil, err
	}
	name, err := requiredNonemptyString(object, "name", path+".name")
	if err != nil {
		return nil, err
	}
	input, ok := object["input"]
	if !ok {
		return nil, invalidRequest("%s.input is required", path)
	}
	if _, err := decodeObject(input, path+".input"); err != nil {
		return nil, err
	}
	cache, err := optionalCacheControl(object, "cache_control", path+".cache_control")
	if err != nil {
		return nil, err
	}
	return &ToolUseBlock{ID: id, Name: name, Input: cloneRaw(input), CacheControl: cache}, nil
}

func decodeToolResultBlock(object map[string]json.RawMessage, path string) (*ToolResultBlock, error) {
	id, err := requiredNonemptyString(object, "tool_use_id", path+".tool_use_id")
	if err != nil {
		return nil, err
	}
	result := &ToolResultBlock{ToolUseID: id}
	if isErrorRaw, present := object["is_error"]; present {
		var isError bool
		if isNull(isErrorRaw) || json.Unmarshal(isErrorRaw, &isError) != nil {
			return nil, invalidRequest("%s.is_error must be a boolean", path)
		}
		result.IsError = &isError
	}
	if contentRaw, present := object["content"]; present {
		if isNull(contentRaw) {
			return nil, invalidRequest("%s.content must be a string or content-block array", path)
		}
		result.Content, err = decodeContent(contentRaw, path+".content")
		if err != nil {
			return nil, err
		}
		if err := validateContentPlacement(result.Content, "tool_result", path+".content"); err != nil {
			return nil, err
		}
	}
	result.CacheControl, err = optionalCacheControl(object, "cache_control", path+".cache_control")
	if err != nil {
		return nil, err
	}
	return result, nil
}

func decodeTools(raw json.RawMessage) ([]Tool, error) {
	var values []json.RawMessage
	if isNull(raw) || json.Unmarshal(raw, &values) != nil {
		return nil, invalidRequest("tools must be an array")
	}
	tools := make([]Tool, len(values))
	for i, value := range values {
		path := fmt.Sprintf("tools[%d]", i)
		object, err := decodeObject(value, path)
		if err != nil {
			return nil, err
		}
		tool := Tool{Raw: cloneRaw(value)}
		if typeRaw, present := object["type"]; present {
			if err := json.Unmarshal(typeRaw, &tool.Type); err != nil || tool.Type == "" {
				return nil, invalidRequest("%s.type must be a nonempty string", path)
			}
		}
		if tool.Type != "" && tool.Type != "custom" {
			tool.Unknown = true
			tools[i] = tool
			continue
		}
		if tool.Name, err = requiredNonemptyString(object, "name", path+".name"); err != nil {
			return nil, err
		}
		if descriptionRaw, present := object["description"]; present {
			if isNull(descriptionRaw) || json.Unmarshal(descriptionRaw, &tool.Description) != nil {
				return nil, invalidRequest("%s.description must be a string", path)
			}
		}
		schema, present := object["input_schema"]
		if !present {
			return nil, invalidRequest("%s.input_schema is required", path)
		}
		if _, err := decodeObject(schema, path+".input_schema"); err != nil {
			return nil, err
		}
		tool.InputSchema = cloneRaw(schema)
		tool.CacheControl, err = optionalCacheControl(object, "cache_control", path+".cache_control")
		if err != nil {
			return nil, err
		}
		tools[i] = tool
	}
	return tools, nil
}

func decodeToolChoice(raw json.RawMessage) (*ToolChoice, error) {
	object, err := decodeObject(raw, "tool_choice")
	if err != nil {
		return nil, err
	}
	choiceType, err := requiredNonemptyString(object, "type", "tool_choice.type")
	if err != nil {
		return nil, err
	}
	if choiceType != "auto" && choiceType != "any" && choiceType != "tool" && choiceType != "none" {
		return nil, invalidRequest("tool_choice.type %q is unsupported", choiceType)
	}
	choice := &ToolChoice{Type: choiceType, Raw: cloneRaw(raw)}
	if choiceType == "tool" {
		if choice.Name, err = requiredNonemptyString(object, "name", "tool_choice.name"); err != nil {
			return nil, err
		}
	}
	if disableRaw, present := object["disable_parallel_tool_use"]; present {
		var disable bool
		if isNull(disableRaw) || json.Unmarshal(disableRaw, &disable) != nil {
			return nil, invalidRequest("tool_choice.disable_parallel_tool_use must be a boolean")
		}
		choice.DisableParallelToolUse = &disable
	}
	return choice, nil
}

func decodeThinkingConfig(raw json.RawMessage) (*ThinkingConfig, error) {
	object, err := decodeObject(raw, "thinking")
	if err != nil {
		return nil, err
	}
	thinkingType, err := requiredNonemptyString(object, "type", "thinking.type")
	if err != nil {
		return nil, err
	}
	if thinkingType != "enabled" && thinkingType != "disabled" && thinkingType != "adaptive" {
		return nil, invalidRequest("thinking.type %q is unsupported", thinkingType)
	}
	thinking := &ThinkingConfig{Type: thinkingType, Raw: cloneRaw(raw)}
	if thinkingType == "enabled" {
		budgetRaw, present := object["budget_tokens"]
		if !present || json.Unmarshal(budgetRaw, &thinking.BudgetTokens) != nil {
			return nil, invalidRequest("thinking.budget_tokens is required and must be an integer")
		}
		if thinking.BudgetTokens <= 0 {
			return nil, invalidRequest("thinking.budget_tokens must be positive")
		}
	}
	if displayRaw, present := object["display"]; present {
		if err := json.Unmarshal(displayRaw, &thinking.Display); err != nil || thinking.Display != "summarized" && thinking.Display != "omitted" {
			return nil, invalidRequest("thinking.display must be summarized or omitted")
		}
	}
	return thinking, nil
}

func decodeMetadata(raw json.RawMessage) (*Metadata, error) {
	object, err := decodeObject(raw, "metadata")
	if err != nil {
		return nil, err
	}
	metadata := &Metadata{Raw: cloneRaw(raw)}
	if userIDRaw, present := object["user_id"]; present {
		if isNull(userIDRaw) || json.Unmarshal(userIDRaw, &metadata.UserID) != nil {
			return nil, invalidRequest("metadata.user_id must be a string")
		}
	}
	return metadata, nil
}

func optionalCacheControl(object map[string]json.RawMessage, field, path string) (*CacheControl, error) {
	raw, present := object[field]
	if !present || isNull(raw) {
		return nil, nil
	}
	cacheObject, err := decodeObject(raw, path)
	if err != nil {
		return nil, err
	}
	cacheType, err := requiredNonemptyString(cacheObject, "type", path+".type")
	if err != nil {
		return nil, err
	}
	if cacheType != "ephemeral" {
		return nil, invalidRequest("%s.type must be ephemeral", path)
	}
	cache := &CacheControl{Type: cacheType, Raw: cloneRaw(raw)}
	if ttlRaw, present := cacheObject["ttl"]; present {
		if err := json.Unmarshal(ttlRaw, &cache.TTL); err != nil || cache.TTL != "5m" && cache.TTL != "1h" {
			return nil, invalidRequest("%s.ttl must be 5m or 1h", path)
		}
	}
	return cache, nil
}

func decodeProbability(raw json.RawMessage, path string) (*float64, error) {
	var value float64
	if isNull(raw) || json.Unmarshal(raw, &value) != nil || value < 0 || value > 1 {
		return nil, invalidRequest("%s must be a number from 0 through 1", path)
	}
	return &value, nil
}

func validateContentPlacement(blocks []ContentBlock, placement, path string) error {
	for i, block := range blocks {
		if block.Unknown() {
			continue
		}
		allowed := false
		switch placement {
		case "system":
			allowed = block.Text != nil
		case "user":
			allowed = block.Text != nil || block.Image != nil || block.Document != nil || block.SearchResult != nil || block.ToolResult != nil
		case "assistant":
			allowed = block.Text != nil || block.ToolUse != nil || block.Thinking != nil || block.RedactedThinking != nil
		case "tool_result":
			allowed = block.Text != nil || block.Image != nil || block.Document != nil || block.SearchResult != nil
		case "search_result":
			allowed = block.Text != nil
		}
		if !allowed {
			return invalidRequest("%s[%d] type %q is not valid in %s content", path, i, block.Type, placement)
		}
	}
	return nil
}

func requiredNonemptyString(object map[string]json.RawMessage, field, path string) (string, error) {
	value, err := requiredString(object, field, path)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", invalidRequest("%s must not be empty", path)
	}
	return value, nil
}

func requiredString(object map[string]json.RawMessage, field, path string) (string, error) {
	raw, present := object[field]
	if !present {
		return "", invalidRequest("%s is required", path)
	}
	var value string
	if isNull(raw) || json.Unmarshal(raw, &value) != nil {
		return "", invalidRequest("%s must be a string", path)
	}
	return value, nil
}

func decodeObject(raw json.RawMessage, path string) (map[string]json.RawMessage, error) {
	if isNull(raw) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return nil, invalidRequest("%s must be an object", path)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, invalidRequest("%s must be an object: %v", path, err)
	}
	return object, nil
}

func supportedImageMediaType(mediaType string) bool {
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return true
	default:
		return false
	}
}

func supportedDocumentMediaType(mediaType string) bool {
	return mediaType == "application/pdf"
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func invalidRequest(format string, args ...any) *RequestError {
	return &RequestError{
		StatusCode: http.StatusBadRequest,
		Response: ErrorResponse{
			Type: "error",
			Error: ErrorDetail{
				Type:    "invalid_request_error",
				Message: fmt.Sprintf(format, args...),
			},
		},
	}
}
