// Package failure classifies real Codex failures for the Anthropic boundary.
package failure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/redact"
)

const MaxMessageBytes = 8 * 1024

// Failure is one upstream error with its real status, safe reason, retry
// classification, and Retry-After value intact.
type Failure struct {
	StatusCode int
	Type       string
	Message    string
	RetryAfter string
	Retryable  bool
}

// AnthropicResponse returns the standard Anthropic error envelope.
func (failure Failure) AnthropicResponse() anthropic.ErrorResponse {
	return anthropic.ErrorResponse{
		Type: "error",
		Error: anthropic.ErrorDetail{
			Type:    failure.Type,
			Message: failure.Message,
		},
	}
}

// FromHTTP maps an upstream HTTP failure to an Anthropic-shaped error.
// Quota and usage-cap bodies are remapped off HTTP 429 so clients that
// retry 429/5xx do not loop forever. Throttle-only 429 stays retryable.
func FromHTTP(status int, body []byte, headers http.Header) Failure {
	message, overloaded, quota := extractMessage(body)
	if message == "" {
		message = fmt.Sprintf("Codex upstream returned HTTP %d", status)
		if statusText := http.StatusText(status); statusText != "" {
			message += " " + statusText
		}
	}
	message = boundMessage(redact.Text(message))

	statusCode := status
	errorType := typeForStatus(status, overloaded)
	retryable := retryableStatus(status)
	if quota {
		statusCode = http.StatusForbidden
		errorType = "permission_error"
		retryable = false
	}
	retryAfter := ""
	if headers != nil {
		retryAfter = strings.TrimSpace(headers.Get("Retry-After"))
	}
	return Failure{
		StatusCode: statusCode,
		Type:       errorType,
		Message:    message,
		RetryAfter: retryAfter,
		Retryable:  retryable,
	}
}

func typeForStatus(status int, overloaded bool) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound, http.StatusGone:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		if overloaded {
			return "overloaded_error"
		}
		return "api_error"
	}
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusConflict ||
		status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests ||
		status >= 500 && status <= 599
}

func extractMessage(body []byte) (string, bool, bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return "", false, false
	}
	var document any
	if json.Unmarshal(trimmed, &document) != nil {
		text := string(trimmed)
		return text, false, quotaSignal(text)
	}
	overloaded := containsCode(document, "overloaded_error", 0)
	quota := containsQuotaSignal(document, 0)
	for _, path := range [][]string{
		{"detail"}, {"message"}, {"error"}, {"error", "message"}, {"error", "detail"},
		{"response", "error", "message"}, {"response", "error", "detail"},
		{"error", "code"}, {"code"},
	} {
		if value := stringAt(document, path); value != "" {
			return value, overloaded, quota || quotaSignal(value)
		}
	}
	if text, ok := document.(string); ok {
		return text, overloaded, quota || quotaSignal(text)
	}
	return string(trimmed), overloaded, quota || quotaSignal(string(trimmed))
}

func stringAt(document any, path []string) string {
	current := document
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current, ok = object[part]
		if !ok {
			return ""
		}
	}
	value, _ := current.(string)
	return strings.TrimSpace(value)
}

func quotaSignal(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "quota") ||
		strings.Contains(lower, "usage cap") ||
		strings.Contains(lower, "usage_cap")
}

func containsQuotaSignal(value any, depth int) bool {
	if depth > 8 {
		return false
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "type" || key == "code" {
				if text, ok := child.(string); ok && quotaSignal(text) {
					return true
				}
			}
			if containsQuotaSignal(child, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsQuotaSignal(child, depth+1) {
				return true
			}
		}
	}
	return false
}

func containsCode(value any, want string, depth int) bool {
	if depth > 8 {
		return false
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if (key == "type" || key == "code") && child == want {
				return true
			}
			if containsCode(child, want, depth+1) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsCode(child, want, depth+1) {
				return true
			}
		}
	}
	return false
}

func boundMessage(message string) string {
	message = strings.ToValidUTF8(strings.TrimSpace(message), "�")
	if len(message) <= MaxMessageBytes {
		return message
	}
	message = message[:MaxMessageBytes]
	for !utf8.ValidString(message) {
		message = message[:len(message)-1]
	}
	return message
}
