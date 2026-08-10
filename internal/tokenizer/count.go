// Package tokenizer counts model-visible translated Codex request content.
package tokenizer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"

	"github.com/Aotricx/Clodex/internal/codexwire"
	tiktoken "github.com/tiktoken-go/tokenizer"
)

const (
	// Codex 0.144.6 uses this conservative estimate only when an inline image's
	// dimensions cannot be decoded locally.
	codexResizedImageTokens = 1844
	codexOriginalPatchSize  = 32
	codexOriginalMaxPatches = 10_000
)

var errUninitialized = errors.New("tokenizer: uninitialized counter")

// Counter is an offline o200k_base request counter. A Counter is safe for
// concurrent use.
type Counter struct {
	codec tiktoken.Codec
}

// New constructs a counter from the dependency's compiled o200k_base
// vocabulary. Construction performs no file or network access.
func New() (*Counter, error) {
	codec, err := tiktoken.Get(tiktoken.O200kBase)
	if err != nil {
		return nil, fmt.Errorf("tokenizer: load o200k_base: %w", err)
	}
	return &Counter{codec: codec}, nil
}

// CountRequest counts only translated model-visible semantics: instructions,
// ordered input items, and tools. Transport/model controls, including the
// prompt cache key produced from Anthropic cache_control markers, add no tokens.
//
// The item boundary and image rules adapt the pinned Codex 0.144.6 estimator:
// codex-rs/core/src/context_manager/history.rs:171-184,519-575,580-688 and
// codex-rs/utils/string/src/truncate.rs:71-83. Unlike that coarse byte counter,
// textual content and serialized structure use exact o200k_base tokenization.
func (c *Counter) CountRequest(request codexwire.Request) (int, error) {
	if c == nil || c.codec == nil {
		return 0, errUninitialized
	}

	total, err := c.countText(request.Instructions)
	if err != nil {
		return 0, fmt.Errorf("tokenizer: count instructions: %w", err)
	}
	for index, item := range request.Input {
		count, err := c.countInputItem(item)
		if err != nil {
			return 0, fmt.Errorf("tokenizer: count input item %d: %w", index, err)
		}
		total, err = addTokens(total, count)
		if err != nil {
			return 0, err
		}
	}
	for index, tool := range request.Tools {
		count, err := c.countJSON(tool)
		if err != nil {
			return 0, fmt.Errorf("tokenizer: count tool %d: %w", index, err)
		}
		total, err = addTokens(total, count)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func (c *Counter) countInputItem(item codexwire.InputItem) (int, error) {
	if encodedLength, ok := encryptedReasoningLength(item); ok {
		return encodedReasoningTokens(encodedLength), nil
	}

	sanitized, estimate := sanitizeInputItem(item)
	structural, err := c.countJSON(sanitized)
	if err != nil {
		return 0, err
	}
	return addTokens(structural, estimate)
}

func (c *Counter) countJSON(value any) (int, error) {
	serialized, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("marshal model-visible structure: %w", err)
	}
	return c.countText(string(serialized))
}

func (c *Counter) countText(text string) (int, error) {
	count, err := c.codec.Count(text)
	if err != nil {
		return 0, fmt.Errorf("o200k_base: %w", err)
	}
	return count, nil
}

func encryptedReasoningLength(item codexwire.InputItem) (int, bool) {
	switch reasoning := item.(type) {
	case codexwire.ReasoningItem:
		if reasoning.EncryptedContent != nil {
			return len(*reasoning.EncryptedContent), true
		}
	case *codexwire.ReasoningItem:
		if reasoning != nil && reasoning.EncryptedContent != nil {
			return len(*reasoning.EncryptedContent), true
		}
	}
	return 0, false
}

func encodedReasoningTokens(encodedLength int) int {
	// Codex estimates decoded model-visible bytes as base64 length * 3/4,
	// less a fixed 650-byte encryption overhead, then ceil(bytes/4).
	decodedBytes := saturatingMultiply(encodedLength, 3) / 4
	if decodedBytes <= 650 {
		return 0
	}
	return divideCeil(decodedBytes-650, 4)
}

func sanitizeInputItem(item codexwire.InputItem) (any, int) {
	switch typed := item.(type) {
	case codexwire.Message:
		return sanitizeMessage(typed)
	case *codexwire.Message:
		if typed == nil {
			return typed, 0
		}
		return sanitizeMessage(*typed)
	case codexwire.FunctionCallOutput:
		return sanitizeFunctionCallOutput(typed)
	case *codexwire.FunctionCallOutput:
		if typed == nil {
			return typed, 0
		}
		return sanitizeFunctionCallOutput(*typed)
	default:
		return item, 0
	}
}

func sanitizeMessage(message codexwire.Message) (codexwire.Message, int) {
	if message.Content == nil {
		return message, 0
	}
	content := make([]codexwire.ContentItem, len(message.Content))
	tokens := 0
	for index, item := range message.Content {
		content[index], tokens = sanitizeContentItem(item, tokens)
	}
	message.Content = content
	return message, tokens
}

func sanitizeContentItem(item codexwire.ContentItem, tokens int) (codexwire.ContentItem, int) {
	switch imageItem := item.(type) {
	case codexwire.InputImage:
		imageItem, estimate := sanitizeImage(imageItem)
		return imageItem, saturatingAdd(tokens, estimate)
	case *codexwire.InputImage:
		if imageItem == nil {
			return imageItem, tokens
		}
		imageCopy, estimate := sanitizeImage(*imageItem)
		return imageCopy, saturatingAdd(tokens, estimate)
	default:
		return item, tokens
	}
}

func sanitizeFunctionCallOutput(output codexwire.FunctionCallOutput) (codexwire.FunctionCallOutput, int) {
	switch content := output.Output.(type) {
	case codexwire.FunctionOutputContent:
		sanitized, tokens := sanitizeFunctionOutputContent(content)
		output.Output = sanitized
		return output, tokens
	case *codexwire.FunctionOutputContent:
		if content == nil {
			return output, 0
		}
		sanitized, tokens := sanitizeFunctionOutputContent(*content)
		output.Output = sanitized
		return output, tokens
	default:
		return output, 0
	}
}

func sanitizeFunctionOutputContent(content codexwire.FunctionOutputContent) (codexwire.FunctionOutputContent, int) {
	if content == nil {
		return nil, 0
	}
	sanitized := make(codexwire.FunctionOutputContent, len(content))
	tokens := 0
	for index, item := range content {
		switch typed := item.(type) {
		case codexwire.InputImage:
			imageItem, estimate := sanitizeImage(typed)
			sanitized[index] = imageItem
			tokens = saturatingAdd(tokens, estimate)
		case *codexwire.InputImage:
			if typed == nil {
				sanitized[index] = typed
				continue
			}
			imageItem, estimate := sanitizeImage(*typed)
			sanitized[index] = imageItem
			tokens = saturatingAdd(tokens, estimate)
		case codexwire.EncryptedContent:
			sanitized[index] = codexwire.EncryptedContent{}
			tokens = saturatingAdd(tokens, encodedFunctionOutputTokens(len(typed.EncryptedContent)))
		case *codexwire.EncryptedContent:
			if typed == nil {
				sanitized[index] = typed
				continue
			}
			sanitized[index] = codexwire.EncryptedContent{}
			tokens = saturatingAdd(tokens, encodedFunctionOutputTokens(len(typed.EncryptedContent)))
		default:
			sanitized[index] = item
		}
	}
	return sanitized, tokens
}

func encodedFunctionOutputTokens(encodedLength int) int {
	// Codex replaces encrypted function output with ceil(encoded bytes * 9/16)
	// model-visible bytes before the same ceil(bytes/4) conversion.
	visibleBytes := divideCeil(saturatingMultiply(encodedLength, 9), 16)
	return divideCeil(visibleBytes, 4)
}

func sanitizeImage(imageItem codexwire.InputImage) (codexwire.InputImage, int) {
	prefix, payload, ok := inlineBase64Image(imageItem.ImageURL)
	if !ok {
		// Pinned Codex applies no image-body estimate to HTTP(S) URLs; their
		// serialized URL remains exact o200k text.
		return imageItem, 0
	}
	imageItem.ImageURL = prefix
	// The backend's live usage accounting follows the same 32px patch geometry
	// for decodable inline images at auto and original detail. Using dimensions
	// keeps count_tokens close to real usage while remaining offline and
	// deterministic. Retain Codex's conservative resized fallback for malformed
	// or unsupported image encodings.
	if patches, ok := imagePatches(payload); ok {
		return imageItem, patches
	}
	return imageItem, codexResizedImageTokens
}

func inlineBase64Image(url string) (prefix string, payload string, ok bool) {
	if len(url) < len("data:") || !strings.EqualFold(url[:len("data:")], "data:") {
		return "", "", false
	}
	comma := strings.IndexByte(url, ',')
	if comma < 0 {
		return "", "", false
	}
	parts := strings.Split(url[len("data:"):comma], ";")
	if len(parts) == 0 || !strings.HasPrefix(strings.ToLower(parts[0]), "image/") {
		return "", "", false
	}
	hasBase64 := false
	for _, parameter := range parts[1:] {
		if strings.EqualFold(parameter, "base64") {
			hasBase64 = true
			break
		}
	}
	if !hasBase64 {
		return "", "", false
	}
	return url[:comma+1], url[comma+1:], true
}

func imagePatches(payload string) (int, bool) {
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(payload)
	}
	if err != nil {
		return 0, false
	}
	configuration, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || configuration.Width <= 0 || configuration.Height <= 0 {
		return 0, false
	}
	patchesWide := divideCeil(configuration.Width, codexOriginalPatchSize)
	patchesHigh := divideCeil(configuration.Height, codexOriginalPatchSize)
	patches := saturatingMultiply(patchesWide, patchesHigh)
	return min(patches, codexOriginalMaxPatches), true
}

func addTokens(total, count int) (int, error) {
	if count < 0 || total > math.MaxInt-count {
		return 0, errors.New("tokenizer: token count overflow")
	}
	return total + count, nil
}

func saturatingAdd(left, right int) int {
	if right > 0 && left > math.MaxInt-right {
		return math.MaxInt
	}
	return left + right
}

func saturatingMultiply(value, multiplier int) int {
	if value > 0 && multiplier > math.MaxInt/value {
		return math.MaxInt
	}
	return value * multiplier
}

func divideCeil(value, divisor int) int {
	return value/divisor + min(value%divisor, 1)
}
