package anthropicstream

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/Aotricx/Clodex/internal/reducer"
	"github.com/Aotricx/Clodex/internal/stopscan"
)

type bufferedUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// ApplyStops truncates only text blocks. Thinking/tools cannot participate in
// a cross-block match; content after the matching text is discarded.
func ApplyStops(source reducer.Result, stops []string) (reducer.Result, string, error) {
	result := cloneResult(source)
	if len(stops) == 0 {
		return result, "", nil
	}
	content := make([]reducer.ContentBlock, 0, len(result.Content))
	for _, block := range result.Content {
		if block.Type != reducer.BlockText {
			content = append(content, block)
			continue
		}
		scanner, err := stopscan.New(stops)
		if err != nil {
			return reducer.Result{}, "", err
		}
		feed := scanner.Feed(block.Text)
		finish := scanner.Finish()
		block.Text = feed.Text + finish.Text
		content = append(content, block)
		if scanner.Stopped() {
			result.Content = content
			result.StopReason = reducer.StopStopSequence
			result.StopSequence = scanner.Match()
			return result, scanner.Match(), nil
		}
	}
	result.Content = content
	return result, "", nil
}

// MarshalBuffered renders one Anthropic non-streaming Messages response.
func MarshalBuffered(messageID, model string, result reducer.Result) ([]byte, error) {
	if messageID == "" || model == "" {
		return nil, fmt.Errorf("marshal Anthropic response: message ID and model are required")
	}
	content := make([]json.RawMessage, 0, len(result.Content))
	for index, block := range result.Content {
		raw, err := marshalBlock(block)
		if err != nil {
			return nil, fmt.Errorf("marshal Anthropic content block %d: %w", index, err)
		}
		content = append(content, raw)
	}
	var sequence *string
	if result.StopSequence != "" {
		value := result.StopSequence
		sequence = &value
	}
	payload := struct {
		ID           string             `json:"id"`
		Type         string             `json:"type"`
		Role         string             `json:"role"`
		Model        string             `json:"model"`
		Content      []json.RawMessage  `json:"content"`
		StopReason   reducer.StopReason `json:"stop_reason"`
		StopSequence *string            `json:"stop_sequence"`
		Usage        bufferedUsage      `json:"usage"`
	}{
		ID: messageID, Type: "message", Role: "assistant", Model: model,
		Content: content, StopReason: result.StopReason, StopSequence: sequence,
		Usage: mapUsage(result.Usage),
	}
	return json.Marshal(payload)
}

func marshalBlock(block reducer.ContentBlock) (json.RawMessage, error) {
	var value any
	switch block.Type {
	case reducer.BlockThinking:
		value = struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{Type: "thinking", Thinking: block.Thinking, Signature: block.Signature}
	case reducer.BlockText:
		value = struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: block.Text}
	case reducer.BlockToolUse, reducer.BlockServerToolUse:
		input := bytes.TrimSpace(block.Input)
		if len(input) == 0 {
			input = []byte(`{}`)
		}
		if !json.Valid(input) || input[0] != '{' {
			return nil, fmt.Errorf("%s input is not a JSON object", block.Type)
		}
		value = struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{Type: string(block.Type), ID: block.ID, Name: block.Name, Input: input}
	default:
		return nil, fmt.Errorf("unsupported block type %q", block.Type)
	}
	return json.Marshal(value)
}

func mapUsage(usage reducer.Usage) bufferedUsage {
	return bufferedUsage{
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
}

func cloneResult(source reducer.Result) reducer.Result {
	result := source
	result.Content = make([]reducer.ContentBlock, len(source.Content))
	for index, block := range source.Content {
		block.Input = bytes.Clone(block.Input)
		result.Content[index] = block
	}
	result.Warnings = append([]reducer.Warning(nil), source.Warnings...)
	result.RateLimitSnapshots = make([]json.RawMessage, len(source.RateLimitSnapshots))
	for index, raw := range source.RateLimitSnapshots {
		result.RateLimitSnapshots[index] = bytes.Clone(raw)
	}
	return result
}
