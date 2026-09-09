package livesmoke

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const liveModel = "gpt-5.6-luna:low"

type anthropicResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func TestLiveProxySmoke(t *testing.T) {
	if os.Getenv("CLODEX_LIVE_TESTS") == "" {
		t.Skip("set CLODEX_LIVE_TESTS=1 and CLODEX_LIVE_BASE_URL to run live proxy smoke tests")
	}
	baseURL := strings.TrimRight(os.Getenv("CLODEX_LIVE_BASE_URL"), "/")
	if baseURL == "" {
		t.Fatal("CLODEX_LIVE_BASE_URL is required when live tests are enabled")
	}
	client := &http.Client{Timeout: 90 * time.Second}

	t.Run("catalog", func(t *testing.T) {
		response, err := client.Get(baseURL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		decodeOK(t, response, &body)
		if len(body.Data) < 7 || body.Data[0].ID == "" {
			t.Fatalf("live model variants=%d first=%q", len(body.Data), firstID(body.Data))
		}
		t.Logf("live models=%d first=%s", len(body.Data), body.Data[0].ID)
	})

	textRequest := map[string]any{
		"model": liveModel, "max_tokens": 64,
		"system":   "Return exactly the requested marker.",
		"messages": []any{map[string]any{"role": "user", "content": "Reply exactly CLODEX_LIVE_OK"}},
	}
	count := countTokens(t, client, baseURL, textRequest)
	var nonstream anthropicResponse
	postJSON(t, client, baseURL+"/v1/messages", textRequest, &nonstream)
	text := responseText(t, nonstream)
	if strings.TrimSpace(text) != "CLODEX_LIVE_OK" || nonstream.StopReason != "end_turn" {
		t.Fatalf("non-stream text=%q stop=%q", text, nonstream.StopReason)
	}
	if difference(count, nonstream.Usage.InputTokens) > 64 {
		t.Fatalf("count_tokens=%d live_input_tokens=%d", count, nonstream.Usage.InputTokens)
	}
	t.Logf("non-stream text=%q count_tokens=%d live_input_tokens=%d output_tokens=%d", text, count, nonstream.Usage.InputTokens, nonstream.Usage.OutputTokens)

	t.Run("stream", func(t *testing.T) {
		request := map[string]any{
			"model": liveModel, "max_tokens": 64, "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "Reply exactly STREAM_LIVE_OK"}},
		}
		events, text := streamRequest(t, client, baseURL+"/v1/messages", request)
		wantEdges := []string{"message_start", "message_stop"}
		if len(events) < 2 || events[0] != wantEdges[0] || events[len(events)-1] != wantEdges[1] || strings.TrimSpace(text) != "STREAM_LIVE_OK" {
			t.Fatalf("stream events=%q text=%q", events, text)
		}
		t.Logf("stream events=%s text=%q", strings.Join(events, ","), text)
	})

	t.Run("tool round trip", func(t *testing.T) {
		tool := map[string]any{
			"name": "echo_value", "description": "Returns the supplied value",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []string{"value"}, "additionalProperties": false},
		}
		prompt := "Use echo_value exactly once with value live-tool. Do not answer without the tool."
		firstRequest := map[string]any{
			"model": liveModel, "max_tokens": 128,
			"messages": []any{map[string]any{"role": "user", "content": prompt}}, "tools": []any{tool}, "tool_choice": map[string]any{"type": "tool", "name": "echo_value"},
		}
		var first anthropicResponse
		postJSON(t, client, baseURL+"/v1/messages", firstRequest, &first)
		toolID, toolInput := responseTool(t, first)
		if first.StopReason != "tool_use" || toolInput != "live-tool" {
			t.Fatalf("first stop=%q tool input=%q", first.StopReason, toolInput)
		}
		secondRequest := map[string]any{
			"model": liveModel, "max_tokens": 128, "tools": []any{tool},
			"messages": []any{
				map[string]any{"role": "user", "content": prompt},
				map[string]any{"role": "assistant", "content": first.Content},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": toolID, "content": "Tool returned: live-tool"}}},
			},
		}
		var second anthropicResponse
		postJSON(t, client, baseURL+"/v1/messages", secondRequest, &second)
		if !strings.Contains(responseText(t, second), "live-tool") || second.StopReason != "end_turn" {
			t.Fatalf("second response=%q stop=%q", responseText(t, second), second.StopReason)
		}
		t.Logf("tool id_present=%t input=%q result=%q", toolID != "", toolInput, responseText(t, second))
	})

	t.Run("parallel tool calls", func(t *testing.T) {
		tool := func(name string) map[string]any {
			return map[string]any{
				"name": name, "description": "Returns the independent " + name + " marker",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}},
			}
		}
		request := map[string]any{
			"model": liveModel, "max_tokens": 256,
			"messages": []any{map[string]any{"role": "user", "content": "Call read_alpha with path alpha and read_beta with path beta together in one response before any results."}},
			"tools":    []any{tool("read_alpha"), tool("read_beta")}, "tool_choice": map[string]any{"type": "any"},
		}
		var response anthropicResponse
		postJSON(t, client, baseURL+"/v1/messages", request, &response)
		names := responseToolNames(t, response)
		if response.StopReason != "tool_use" || len(names) != 2 || !containsString(names, "read_alpha") || !containsString(names, "read_beta") {
			t.Fatalf("parallel stop=%q tools=%v", response.StopReason, names)
		}
		t.Logf("parallel tools=%v in one Anthropic response", names)
	})

	t.Run("image", func(t *testing.T) {
		path := os.Getenv("CLODEX_LIVE_IMAGE")
		if path == "" {
			t.Skip("set CLODEX_LIVE_IMAGE to a local Great Dane image")
		}
		imageBytes, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		request := map[string]any{
			"model": liveModel, "max_tokens": 64,
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": base64.StdEncoding.EncodeToString(imageBytes)}},
				map[string]any{"type": "text", "text": "What dog breed is shown? Answer with the breed and one short confidence phrase."},
			}}},
		}
		count := countTokens(t, client, baseURL, request)
		var response anthropicResponse
		postJSON(t, client, baseURL+"/v1/messages", request, &response)
		text := responseText(t, response)
		if !strings.Contains(strings.ToLower(text), "great dane") {
			t.Fatalf("image answer=%q", text)
		}
		tolerance := max(64, response.Usage.InputTokens/5)
		if difference(count, response.Usage.InputTokens) > tolerance {
			t.Fatalf("image count_tokens=%d live_input_tokens=%d tolerance=%d", count, response.Usage.InputTokens, tolerance)
		}
		t.Logf("image answer=%q count_tokens=%d live_input_tokens=%d", text, count, response.Usage.InputTokens)
	})
}

func postJSON(t testing.TB, client *http.Client, target string, value any, output any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	decodeOK(t, response, output)
}

func decodeOK(t testing.TB, response *http.Response, output any) {
	t.Helper()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("HTTP %d: %s", response.StatusCode, body)
	}
	if err := json.Unmarshal(body, output); err != nil {
		t.Fatalf("decode HTTP %d body: %v: %s", response.StatusCode, err, body)
	}
}

func countTokens(t testing.TB, client *http.Client, baseURL string, request map[string]any) int {
	t.Helper()
	copy := make(map[string]any, len(request))
	for key, value := range request {
		copy[key] = value
	}
	delete(copy, "max_tokens")
	delete(copy, "stream")
	var response struct {
		InputTokens int `json:"input_tokens"`
	}
	postJSON(t, client, baseURL+"/v1/messages/count_tokens", copy, &response)
	if response.InputTokens <= 0 {
		t.Fatalf("count_tokens=%d", response.InputTokens)
	}
	return response.InputTokens
}

func responseText(t testing.TB, response anthropicResponse) string {
	t.Helper()
	var combined strings.Builder
	for _, raw := range response.Content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		if block.Type == "text" {
			combined.WriteString(block.Text)
		}
	}
	return combined.String()
}

func responseTool(t testing.TB, response anthropicResponse) (string, string) {
	t.Helper()
	for _, raw := range response.Content {
		var block struct {
			Type  string `json:"type"`
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input struct {
				Value string `json:"value"`
			} `json:"input"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		if block.Type == "tool_use" && block.Name == "echo_value" {
			return block.ID, block.Input.Value
		}
	}
	t.Fatal("tool_use block missing")
	return "", ""
}

func responseToolNames(t testing.TB, response anthropicResponse) []string {
	t.Helper()
	var names []string
	for _, raw := range response.Content {
		var block struct {
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		if block.Type == "tool_use" {
			names = append(names, block.Name)
		}
	}
	return names
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func streamRequest(t testing.TB, client *http.Client, target string, value any) ([]string, string) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		failure, _ := io.ReadAll(response.Body)
		t.Fatalf("HTTP %d: %s", response.StatusCode, failure)
	}
	var events []string
	var text strings.Builder
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if event, ok := strings.CutPrefix(line, "event: "); ok {
			events = append(events, event)
			continue
		}
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "content_block_delta" && event.Delta.Type == "text_delta" {
			text.WriteString(event.Delta.Text)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events, text.String()
}

func difference(left, right int) int {
	if left < right {
		return right - left
	}
	return left - right
}

func firstID(models []struct {
	ID string `json:"id"`
}) string {
	if len(models) == 0 {
		return ""
	}
	return models[0].ID
}
