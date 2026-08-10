package anthropic

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeRequestNormalizesStringContentAndSystem(t *testing.T) {
	req := decodeOK(t, `{
		"model":"claude-opus-4-1",
		"max_tokens":1024,
		"system":"be precise",
		"messages":[{"role":"user","content":"hello"}]
	}`)

	if req.Model != "claude-opus-4-1" || req.MaxTokens != 1024 {
		t.Fatalf("request identity = model %q, max_tokens %d", req.Model, req.MaxTokens)
	}
	if len(req.System) != 1 || req.System[0].Type != "text" || req.System[0].Text == nil || req.System[0].Text.Text != "be precise" {
		t.Fatalf("system = %#v", req.System)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" || len(req.Messages[0].Content) != 1 {
		t.Fatalf("messages = %#v", req.Messages)
	}
	block := req.Messages[0].Content[0]
	if block.Type != "text" || block.Text == nil || block.Text.Text != "hello" {
		t.Fatalf("message content = %#v", block)
	}
}

func TestDecodeCountTokensRequestMatchesClaudeCodeShapeWithoutMaxTokens(t *testing.T) {
	req, err := DecodeCountTokensRequest(strings.NewReader(`{
		"model":"clodex-custom-model",
		"messages":[{"role":"user","content":"foo"}],
		"tools":[]
	}`))
	if err != nil {
		t.Fatalf("DecodeCountTokensRequest() error = %v", err)
	}
	if req.Model != "clodex-custom-model" || req.MaxTokens != 0 || len(req.Messages) != 1 || req.Messages[0].Content[0].Text.Text != "foo" {
		t.Fatalf("count request = %#v", req)
	}

	for _, body := range []string{
		`{"model":"m","max_tokens":null,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`,
	} {
		if _, err := DecodeCountTokensRequest(strings.NewReader(body)); err == nil {
			t.Fatalf("DecodeCountTokensRequest(%s) error = nil", body)
		}
	}
}

func TestDecodeRequestPreservesOrderedContentBlockUnion(t *testing.T) {
	req := decodeOK(t, `{
		"model":"claude-opus-4-1",
		"max_tokens":4096,
		"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"before","future_text_field":true},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}},
			{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}},
			{"type":"tool_result","tool_use_id":"toolu_1","is_error":false,"content":[
				{"type":"text","text":"result"},
				{"type":"image","source":{"type":"url","url":"http://example.com/result.webp"}}
			]}
		]},{"role":"assistant","content":[
			{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a"},"cache_control":{"type":"ephemeral"}},
			{"type":"thinking","thinking":"","signature":"encrypted-signature"},
			{"type":"redacted_thinking","data":"opaque"},
			{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"x"}},
			{"type":"future_block","value":7}
		]}]
	}`)

	if len(req.System) != 1 || req.System[0].Text == nil || req.System[0].Text.CacheControl == nil || req.System[0].Text.CacheControl.TTL != "1h" {
		t.Fatalf("system blocks = %#v", req.System)
	}
	blocks := append(append([]ContentBlock{}, req.Messages[0].Content...), req.Messages[1].Content...)
	wantTypes := []string{"text", "image", "image", "tool_result", "tool_use", "thinking", "redacted_thinking", "server_tool_use", "future_block"}
	if len(blocks) != len(wantTypes) {
		t.Fatalf("block count = %d, want %d", len(blocks), len(wantTypes))
	}
	for i, want := range wantTypes {
		if blocks[i].Type != want {
			t.Errorf("block %d type = %q, want %q", i, blocks[i].Type, want)
		}
		if !json.Valid(blocks[i].Raw) {
			t.Errorf("block %d raw JSON invalid: %q", i, blocks[i].Raw)
		}
	}
	if blocks[0].Text == nil || !strings.Contains(string(blocks[0].Raw), "future_text_field") {
		t.Fatalf("text/raw = %#v / %s", blocks[0].Text, blocks[0].Raw)
	}
	if got := blocks[1].Image; got == nil || got.Source.Type != "base64" || got.Source.MediaType != "image/png" || got.Source.Data != "AQID" {
		t.Fatalf("base64 image = %#v", got)
	}
	if got := blocks[2].Image; got == nil || got.Source.Type != "url" || got.Source.URL != "https://example.com/a.png" {
		t.Fatalf("URL image = %#v", got)
	}
	result := blocks[3].ToolResult
	if result == nil || result.ToolUseID != "toolu_1" || result.IsError == nil || *result.IsError || len(result.Content) != 2 || result.Content[0].Text == nil || result.Content[1].Image == nil {
		t.Fatalf("tool result = %#v", result)
	}
	if got := blocks[4].ToolUse; got == nil || got.ID != "toolu_1" || got.Name != "read" || string(got.Input) != `{"path":"a"}` || got.CacheControl == nil {
		t.Fatalf("tool use = %#v", got)
	}
	if got := blocks[5].Thinking; got == nil || got.Thinking != "" || got.Signature != "encrypted-signature" {
		t.Fatalf("thinking = %#v", got)
	}
	if got := blocks[6].RedactedThinking; got == nil || got.Data != "opaque" {
		t.Fatalf("redacted thinking = %#v", got)
	}
	for _, i := range []int{7, 8} {
		if !blocks[i].Unknown() || len(blocks[i].Raw) == 0 {
			t.Fatalf("unknown block %d = %#v", i, blocks[i])
		}
	}
}

func TestDecodeRequestAcceptsToolResultStringAndOmittedContent(t *testing.T) {
	req := decodeOK(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"a","content":"plain"},
			{"type":"tool_result","tool_use_id":"b"}
		]}]
	}`)
	first := req.Messages[0].Content[0].ToolResult
	second := req.Messages[0].Content[1].ToolResult
	if first == nil || len(first.Content) != 1 || first.Content[0].Text == nil || first.Content[0].Text.Text != "plain" {
		t.Fatalf("string tool result = %#v", first)
	}
	if second == nil || second.Content != nil {
		t.Fatalf("omitted tool result = %#v", second)
	}
}

func TestDecodeRequestAcceptsSupportedImageMediaWithoutByteCap(t *testing.T) {
	for _, mediaType := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		t.Run(mediaType, func(t *testing.T) {
			body := requestWithBlock(`{"type":"image","source":{"type":"base64","media_type":"` + mediaType + `","data":"AQID"}}`)
			if got := decodeOK(t, body).Messages[0].Content[0].Image.Source.MediaType; got != mediaType {
				t.Fatalf("media type = %q", got)
			}
		})
	}

	large := base64.StdEncoding.EncodeToString(make([]byte, 2<<20))
	body := requestWithBlock(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + large + `"}}`)
	if got := decodeOK(t, body).Messages[0].Content[0].Image.Source.Data; len(got) != len(large) {
		t.Fatalf("large image data length = %d, want %d", len(got), len(large))
	}
}

func TestDecodeRequestRejectsMalformedImageSources(t *testing.T) {
	tests := []struct {
		name  string
		block string
	}{
		{"missing source type", `{"type":"image","source":{"media_type":"image/png","data":"AQID"}}`},
		{"unsupported source type", `{"type":"image","source":{"type":"file","file_id":"file_1"}}`},
		{"missing media type", `{"type":"image","source":{"type":"base64","data":"AQID"}}`},
		{"unsupported media type", `{"type":"image","source":{"type":"base64","media_type":"image/bmp","data":"AQID"}}`},
		{"missing base64 data", `{"type":"image","source":{"type":"base64","media_type":"image/png"}}`},
		{"invalid alphabet", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%%%"}}`},
		{"missing padding", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YQ"}}`},
		{"noncanonical trailing bits", `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AB=="}}`},
		{"missing URL", `{"type":"image","source":{"type":"url"}}`},
		{"relative URL", `{"type":"image","source":{"type":"url","url":"/image.png"}}`},
		{"FTP URL", `{"type":"image","source":{"type":"url","url":"ftp://example.com/a.png"}}`},
		{"URL missing host", `{"type":"image","source":{"type":"url","url":"https:///a.png"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeInvalid(t, requestWithBlock(tc.block))
		})
	}
}

func TestDecodeRequestAcceptsHTTPAndHTTPSImages(t *testing.T) {
	for _, imageURL := range []string{"http://example.com/a.png", "https://example.com/a.png?x=1#fragment"} {
		t.Run(imageURL, func(t *testing.T) {
			body := requestWithBlock(`{"type":"image","source":{"type":"url","url":"` + imageURL + `"}}`)
			if got := decodeOK(t, body).Messages[0].Content[0].Image.Source.URL; got != imageURL {
				t.Fatalf("URL = %q", got)
			}
		})
	}
}

func TestDecodeRequestToolsToolChoiceThinkingAndControls(t *testing.T) {
	tests := []struct {
		name        string
		choice      string
		wantType    string
		wantName    string
		wantDisable *bool
	}{
		{"auto", `{"type":"auto"}`, "auto", "", nil},
		{"any parallel disabled", `{"type":"any","disable_parallel_tool_use":true}`, "any", "", boolPtr(true)},
		{"named tool", `{"type":"tool","name":"read","disable_parallel_tool_use":false}`, "tool", "read", boolPtr(false)},
		{"none", `{"type":"none"}`, "none", "", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{
				"model":"m","max_tokens":999999999999999999,
				"messages":[{"role":"user","content":"go"}],
				"tools":[
					{"name":"read","description":"read file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]},"cache_control":{"type":"ephemeral","ttl":"5m"},"future":1},
					{"type":"web_search_20250305","name":"web_search","max_uses":3}
				],
				"tool_choice":` + tc.choice + `,
				"thinking":{"type":"enabled","budget_tokens":2048},
				"stop_sequences":["END","STOP"],
				"temperature":0,"top_p":1,"stream":true,
				"metadata":{"user_id":"opaque-user","future":"kept"},
				"cache_control":{"type":"ephemeral"},
				"future_top_level":{"a":1}
			}`
			req := decodeOK(t, body)
			if req.MaxTokens != 999999999999999999 || req.ToolChoice == nil || req.ToolChoice.Type != tc.wantType || req.ToolChoice.Name != tc.wantName {
				t.Fatalf("controls = max=%d choice=%#v", req.MaxTokens, req.ToolChoice)
			}
			if (req.ToolChoice.DisableParallelToolUse == nil) != (tc.wantDisable == nil) || tc.wantDisable != nil && *req.ToolChoice.DisableParallelToolUse != *tc.wantDisable {
				t.Fatalf("disable_parallel_tool_use = %#v", req.ToolChoice.DisableParallelToolUse)
			}
			if len(req.Tools) != 2 || req.Tools[0].Unknown || req.Tools[0].Name != "read" || req.Tools[0].CacheControl == nil || !json.Valid(req.Tools[0].InputSchema) {
				t.Fatalf("tools = %#v", req.Tools)
			}
			if !req.Tools[1].Unknown || req.Tools[1].Type != "web_search_20250305" || !json.Valid(req.Tools[1].Raw) {
				t.Fatalf("server tool = %#v", req.Tools[1])
			}
			if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 2048 {
				t.Fatalf("thinking = %#v", req.Thinking)
			}
			if len(req.StopSequences) != 2 || req.Temperature == nil || *req.Temperature != 0 || req.TopP == nil || *req.TopP != 1 || !req.Stream {
				t.Fatalf("sampling/stream controls = %#v", req)
			}
			if req.Metadata == nil || req.Metadata.UserID != "opaque-user" || !strings.Contains(string(req.Metadata.Raw), "future") || req.CacheControl == nil {
				t.Fatalf("metadata/cache = %#v / %#v", req.Metadata, req.CacheControl)
			}
			if _, ok := req.Extra["future_top_level"]; !ok {
				t.Fatalf("extra fields = %#v", req.Extra)
			}
		})
	}
}

func TestDecodeRequestAcceptsThinkingConfigVariants(t *testing.T) {
	tests := []struct {
		name, raw, wantType, wantDisplay string
		wantBudget                       int64
	}{
		{"enabled", `{"type":"enabled","budget_tokens":1024,"display":"omitted"}`, "enabled", "omitted", 1024},
		{"disabled", `{"type":"disabled"}`, "disabled", "", 0},
		{"adaptive", `{"type":"adaptive","display":"summarized"}`, "adaptive", "summarized", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],"thinking":` + tc.raw + `}`
			got := decodeOK(t, body).Thinking
			if got == nil || got.Type != tc.wantType || got.Display != tc.wantDisplay || got.BudgetTokens != tc.wantBudget {
				t.Fatalf("thinking = %#v", got)
			}
		})
	}
}

func TestDecodeRequestValidatesRequiredFieldsAndUnionTypes(t *testing.T) {
	valid := `"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}]`
	tests := []struct {
		name, body string
	}{
		{"malformed JSON", `{"model":`},
		{"trailing JSON", `{` + valid + `} {}`},
		{"root must be object", `[]`},
		{"model required", `{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`},
		{"max tokens required", `{"model":"m","messages":[{"role":"user","content":"x"}]}`},
		{"negative max tokens", `{"model":"m","max_tokens":-1,"messages":[{"role":"user","content":"x"}]}`},
		{"zero max tokens", `{"model":"m","max_tokens":0,"messages":[{"role":"user","content":"x"}]}`},
		{"messages required", `{"model":"m","max_tokens":1}`},
		{"messages nonempty", `{"model":"m","max_tokens":1,"messages":[]}`},
		{"role required", `{"model":"m","max_tokens":1,"messages":[{"content":"x"}]}`},
		{"role valid", `{"model":"m","max_tokens":1,"messages":[{"role":"tool","content":"x"}]}`},
		{"message content required", `{"model":"m","max_tokens":1,"messages":[{"role":"user"}]}`},
		{"block type required", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"text":"x"}]}]}`},
		{"text required", `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"text"}]}]}`},
		{"tool use fields required", requestWithBlock(`{"type":"tool_use","name":"read","input":{}}`)},
		{"tool use input object", `{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"u","name":"read","input":[]}]}]}`},
		{"tool result ID required", requestWithBlock(`{"type":"tool_result","content":"x"}`)},
		{"tool result null content", requestWithBlock(`{"type":"tool_result","tool_use_id":"u","content":null}`)},
		{"thinking fields required", requestWithBlock(`{"type":"thinking","thinking":"x"}`)},
		{"redacted data required", requestWithBlock(`{"type":"redacted_thinking"}`)},
		{"tool name required", `{` + valid + `,"tools":[{"input_schema":{"type":"object"}}]}`},
		{"tool schema required", `{` + valid + `,"tools":[{"name":"read"}]}`},
		{"tool schema object", `{` + valid + `,"tools":[{"name":"read","input_schema":[]}]}`},
		{"tool choice type required", `{` + valid + `,"tool_choice":{}}`},
		{"tool choice type valid", `{` + valid + `,"tool_choice":{"type":"future"}}`},
		{"named tool choice name required", `{` + valid + `,"tool_choice":{"type":"tool"}}`},
		{"thinking type required", `{` + valid + `,"thinking":{}}`},
		{"thinking type valid", `{` + valid + `,"thinking":{"type":"future"}}`},
		{"enabled thinking budget required", `{` + valid + `,"thinking":{"type":"enabled"}}`},
		{"enabled thinking positive budget", `{` + valid + `,"thinking":{"type":"enabled","budget_tokens":0}}`},
		{"thinking display valid", `{` + valid + `,"thinking":{"type":"adaptive","display":"full"}}`},
		{"temperature range", `{` + valid + `,"temperature":1.1}`},
		{"top p range", `{` + valid + `,"top_p":-0.1}`},
		{"metadata object", `{` + valid + `,"metadata":"x"}`},
		{"cache type", `{` + valid + `,"cache_control":{"type":"forever"}}`},
		{"cache ttl", `{` + valid + `,"cache_control":{"type":"ephemeral","ttl":"2h"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeInvalid(t, tc.body)
		})
	}
}

func TestDecodeRequestRejectsNullScalars(t *testing.T) {
	valid := `"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}]`
	tests := []struct {
		name, body string
	}{
		{"max tokens", `{"model":"m","max_tokens":null,"messages":[{"role":"user","content":"x"}]}`},
		{"text", requestWithBlock(`{"type":"text","text":null}`)},
		{"image data", requestWithBlock(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":null}}`)},
		{"thinking", requestWithBlock(`{"type":"thinking","thinking":null,"signature":"s"}`)},
		{"stream", `{` + valid + `,"stream":null}`},
		{"temperature", `{` + valid + `,"temperature":null}`},
		{"metadata user", `{` + valid + `,"metadata":{"user_id":null}}`},
		{"tool description", `{` + valid + `,"tools":[{"name":"read","description":null,"input_schema":{"type":"object"}}]}`},
		{"tool choice parallel flag", `{` + valid + `,"tool_choice":{"type":"auto","disable_parallel_tool_use":null}}`},
		{"tool result error flag", requestWithBlock(`{"type":"tool_result","tool_use_id":"u","is_error":null}`)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			decodeInvalid(t, tc.body)
		})
	}
}

func TestDecodeRequestAcceptsUnknownFieldsAndEmptyRequiredStrings(t *testing.T) {
	body := `{
		"model":"m","max_tokens":1,"unknown":true,
		"messages":[{"role":"assistant","future":1,"content":[
			{"type":"text","text":""},
			{"type":"thinking","thinking":"","signature":"opaque"},
			{"type":"redacted_thinking","data":"opaque"}
		]}]
	}`
	req := decodeOK(t, body)
	if req.MaxTokens != 1 || len(req.Messages[0].Content) != 3 {
		t.Fatalf("request = %#v", req)
	}
}

func TestDecodeRequestRejectsKnownBlocksInInvalidRolesAndPlacements(t *testing.T) {
	tests := []string{
		`{"model":"m","max_tokens":1,"system":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}],"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_use","id":"u","name":"n","input":{}}]}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"thinking","thinking":"x","signature":"s"}]}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"tool_result","tool_use_id":"u","content":"x"}]}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"u","content":[{"type":"tool_use","id":"nested","name":"n","input":{}}]}]}]}`,
	}
	for _, body := range tests {
		decodeInvalid(t, body)
	}
}

func TestDecodeRequestRejectsEmptyOpaqueAndImagePayloads(t *testing.T) {
	for _, body := range []string{
		requestWithBlock(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":""}}`),
		`{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":""}]}]}`,
		`{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":""}]}]}`,
	} {
		decodeInvalid(t, body)
	}
}

func TestDecodeRequestCacheControlAtKnownLegalLocations(t *testing.T) {
	body := `{
		"model":"m","max_tokens":1,"cache_control":{"type":"ephemeral","ttl":"1h"},
		"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":[
			{"type":"text","text":"t","cache_control":{"type":"ephemeral"}},
			{"type":"image","source":{"type":"url","url":"https://example.com/a"},"cache_control":{"type":"ephemeral"}},
			{"type":"tool_result","tool_use_id":"u","content":"r","cache_control":{"type":"ephemeral"}}
		]},{"role":"assistant","content":[
			{"type":"tool_use","id":"u","name":"n","input":{},"cache_control":{"type":"ephemeral"}}
		]}],
		"tools":[{"name":"n","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}]
	}`
	req := decodeOK(t, body)
	userBlocks := req.Messages[0].Content
	assistantBlocks := req.Messages[1].Content
	if req.CacheControl == nil || req.System[0].Text.CacheControl == nil || userBlocks[0].Text.CacheControl == nil || userBlocks[1].Image.CacheControl == nil || userBlocks[2].ToolResult.CacheControl == nil || assistantBlocks[0].ToolUse.CacheControl == nil || req.Tools[0].CacheControl == nil {
		t.Fatalf("cache controls not retained: %#v", req)
	}
}

func decodeOK(t *testing.T, body string) *MessageRequest {
	t.Helper()
	req, err := DecodeRequest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("DecodeRequest() error = %v", err)
	}
	return req
}

func decodeInvalid(t *testing.T, body string) *RequestError {
	t.Helper()
	_, err := DecodeRequest(strings.NewReader(body))
	if err == nil {
		t.Fatal("DecodeRequest() returned nil error")
	}
	var requestErr *RequestError
	if !errors.As(err, &requestErr) {
		t.Fatalf("error type = %T, want *RequestError: %v", err, err)
	}
	if requestErr.StatusCode != 400 || requestErr.Response.Type != "error" || requestErr.Response.Error.Type != "invalid_request_error" || requestErr.Response.Error.Message == "" {
		t.Fatalf("request error = %#v", requestErr)
	}
	return requestErr
}

func requestWithBlock(block string) string {
	return `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[` + block + `]}]}`
}

func boolPtr(v bool) *bool { return &v }

func TestDecodeAcceptsInArraySystemMessage(t *testing.T) {
	// Claude Code 2.1.x (headless -p with agent definitions present) sends a
	// role:"system" entry INSIDE messages[] — the agent-types listing. The
	// real Anthropic API accepts it; rejecting it hard-fails every such
	// session through the proxy.
	body := `{"model":"m","max_tokens":1,"messages":[
		{"role":"user","content":"hi"},
		{"role":"system","content":[{"type":"text","text":"Available agent types..."}]},
		{"role":"assistant","content":"ok"}
	]}`
	request, err := DecodeRequest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("DecodeRequest error = %v", err)
	}
	if len(request.Messages) != 3 || request.Messages[1].Role != "system" {
		t.Fatalf("messages = %+v, want system entry preserved at index 1", request.Messages)
	}
	if request.Messages[1].Content[0].Text == nil {
		t.Fatalf("system message content = %+v, want text block", request.Messages[1].Content)
	}
}

func TestDecodeRejectsToolBlocksInSystemMessage(t *testing.T) {
	body := `{"model":"m","max_tokens":1,"messages":[
		{"role":"system","content":[{"type":"tool_use","id":"t","name":"n","input":{}}]}
	]}`
	if _, err := DecodeRequest(strings.NewReader(body)); err == nil {
		t.Fatal("DecodeRequest accepted tool_use in system content, want error")
	}
}
