package codexwire

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestRequestMarshalJSONGolden(t *testing.T) {
	req := Request{
		Model:             "gpt-5.4-mini",
		Instructions:      "Answer precisely.",
		ParallelToolCalls: true,
		Reasoning: &Reasoning{
			Effort:  "low",
			Summary: "auto",
		},
		Tools:          []Tool{},
		Stream:         true,
		ServiceTier:    "priority",
		PromptCacheKey: "thread-123",
	}

	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt-5.4-mini","instructions":"Answer precisely.","input":[],"tools":[],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"low","summary":"auto"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"service_tier":"priority","prompt_cache_key":"thread-123"}`
	if string(got) != want {
		t.Fatalf("request JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestRequestMarshalJSONResponsesLite(t *testing.T) {
	tool := FunctionTool{
		Name:        "lookup",
		Description: "Look up a value.",
		Strict:      false,
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	}
	req := Request{
		Model: "gpt-5.6-sol",
		Input: []InputItem{
			AdditionalTools{Role: "developer", Tools: []Tool{tool}},
			Message{Role: "developer", Content: []ContentItem{
				InputText{Text: "Use the provided tool."},
			}},
			Message{Role: "user", Content: []ContentItem{
				InputText{Text: "Find it."},
			}},
		},
		Reasoning: &Reasoning{
			Effort:  "max",
			Summary: "auto",
			Context: ReasoningContextAllTurns,
		},
		Stream:         true,
		PromptCacheKey: "thread-lite",
	}

	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"lookup","description":"Look up a value.","strict":false,"parameters":{"type":"object","properties":{}}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"Use the provided tool."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"Find it."}]}],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"max","summary":"auto","context":"all_turns"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"thread-lite"}`
	if string(got) != want {
		t.Fatalf("Responses Lite request JSON mismatch:\n got (%d): %q\nwant (%d): %q", len(got), got, len(want), want)
	}
}

func TestRequestMarshalJSONPreservesFullHistory(t *testing.T) {
	encrypted := "encrypted-reasoning"
	req := Request{
		Model:        "gpt-5.4",
		Instructions: "Use tools when needed.",
		Input: []InputItem{
			Message{Role: "user", Content: []ContentItem{
				InputText{Text: "Inspect this."},
				InputImage{ImageURL: "https://example.test/image.png", Detail: ImageDetailHigh},
			}},
			Message{Role: "assistant", Content: []ContentItem{
				OutputText{Text: "I will inspect it."},
			}},
			ReasoningItem{
				Summary:          []ReasoningSummary{SummaryText{Text: "Checked image."}},
				Content:          []ReasoningContent{ReasoningText{Text: "Visual evidence."}},
				EncryptedContent: &encrypted,
			},
			FunctionCall{
				Name:      "lookup",
				Arguments: `{"query":"release"}`,
				CallID:    "call-1",
			},
			FunctionCallOutput{
				CallID: "call-1",
				Output: FunctionOutputText("Go 1.26"),
			},
		},
		Tools: []Tool{FunctionTool{
			Name:        "lookup",
			Description: "Look up a release.",
			Strict:      true,
			Parameters:  json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`),
		}},
		ParallelToolCalls: true,
		Reasoning: &Reasoning{
			Effort:  "high",
			Summary: "detailed",
		},
		Stream:         false,
		PromptCacheKey: "thread-456",
	}

	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt-5.4","instructions":"Use tools when needed.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect this."},{"type":"input_image","image_url":"https://example.test/image.png","detail":"high"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I will inspect it."}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"Checked image."}],"content":[{"type":"reasoning_text","text":"Visual evidence."}],"encrypted_content":"encrypted-reasoning"},{"type":"function_call","name":"lookup","arguments":"{\"query\":\"release\"}","call_id":"call-1"},{"type":"function_call_output","call_id":"call-1","output":"Go 1.26"}],"tools":[{"type":"function","name":"lookup","description":"Look up a release.","strict":true,"parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"high","summary":"detailed"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"thread-456"}`
	if string(got) != want {
		t.Fatalf("full-history request JSON mismatch:\n got: %s\nwant: %s", got, want)
	}
}

func TestRequestMarshalJSONToolChoicesAndFixedStreamingTransport(t *testing.T) {
	tests := []struct {
		name   string
		choice ToolChoice
		want   string
	}{
		{name: "default auto", want: `"auto"`},
		{name: "none", choice: ToolChoiceNone, want: `"none"`},
		{name: "required", choice: ToolChoiceRequired, want: `"required"`},
		{name: "named function", choice: FunctionToolChoice{Name: "lookup"}, want: `{"type":"function","name":"lookup"}`},
		{name: "named web search", choice: WebSearchToolChoice{}, want: `{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(Request{Model: "gpt-5.4-mini", ToolChoice: tc.choice, Stream: false})
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(got, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["tool_choice"]) != tc.want {
				t.Fatalf("tool_choice = %s, want %s", fields["tool_choice"], tc.want)
			}
			if string(fields["stream"]) != "true" {
				t.Fatalf("stream = %s, want true", fields["stream"])
			}
		})
	}
}

func TestRequestMarshalJSONWebSearchCall(t *testing.T) {
	got, err := json.Marshal(WebSearchCall{
		ID:     "ws_1",
		Status: "completed",
		Action: WebSearchAction{Type: "search", Query: "golang"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"golang"}}`
	if string(got) != want {
		t.Fatalf("web_search_call JSON = %s, want %s", got, want)
	}
}

func TestWebSearchToolMarshalJSON(t *testing.T) {
	tool := WebSearchTool{
		ExternalWebAccess:  boolPtr(true),
		AllowedDomains:     []string{"example.com"},
		SearchContentTypes: []string{"text", "image"},
	}
	got, err := json.Marshal(tool)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"web_search","external_web_access":true,"filters":{"allowed_domains":["example.com"]},"search_content_types":["text","image"]}`
	if string(got) != want {
		t.Fatalf("web search JSON = %s, want %s", got, want)
	}
}

func TestFunctionCallOutputMarshalJSONForms(t *testing.T) {
	tests := []struct {
		name   string
		output FunctionOutput
		want   string
	}{
		{
			name:   "plain text",
			output: FunctionOutputText("ok"),
			want:   `{"type":"function_call_output","call_id":"call-1","output":"ok"}`,
		},
		{
			name: "ordered structured content",
			output: FunctionOutputContent{
				InputText{Text: "before"},
				InputImage{ImageURL: "data:image/png;base64,AAAA", Detail: ImageDetailLow},
				EncryptedContent{EncryptedContent: "ciphertext"},
				InputText{Text: "after"},
			},
			want: `{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"low"},{"type":"encrypted_content","encrypted_content":"ciphertext"},{"type":"input_text","text":"after"}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(FunctionCallOutput{CallID: "call-1", Output: tc.output})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("function output JSON mismatch:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func boolPtr(value bool) *bool { return &value }

func TestRequestMarshalJSONProvenOutputControls(t *testing.T) {
	req := Request{
		Model:  "gpt-5.4",
		Input:  []InputItem{},
		Tools:  []Tool{},
		Stream: true,
		Text: &TextControls{
			Verbosity: VerbosityHigh,
			Format: &JSONSchemaFormat{
				Strict: true,
				Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`),
			},
		},
		ClientMetadata: map[string]string{
			"thread_id":  "thread-789",
			"session_id": "session-789",
		},
	}

	got, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt-5.4","input":[],"tools":[],"tool_choice":"auto","parallel_tool_calls":false,"reasoning":null,"store":false,"stream":true,"include":[],"text":{"verbosity":"high","format":{"type":"json_schema","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false},"name":"codex_output_schema"}},"client_metadata":{"session_id":"session-789","thread_id":"thread-789"}}`
	if string(got) != want {
		t.Fatalf("output-control request JSON mismatch:\n got: %s\nwant: %s", got, want)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatal(err)
	}
	for _, unsupported := range []string{"temperature", "top_p", "stop", "max_output_tokens", "metadata", "previous_response_id"} {
		if _, ok := fields[unsupported]; ok {
			t.Errorf("unsupported field %q was serialized", unsupported)
		}
	}
	wantFields := []string{"client_metadata", "include", "input", "model", "parallel_tool_calls", "reasoning", "store", "stream", "text", "tool_choice", "tools"}
	gotFields := make([]string, 0, len(fields))
	for field := range fields {
		gotFields = append(gotFields, field)
	}
	slices.Sort(gotFields)
	if !slices.Equal(gotFields, wantFields) {
		t.Fatalf("request fields = %v, want %v", gotFields, wantFields)
	}
}
