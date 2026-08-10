package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/model"
)

func TestReasoningSignatureRoundTripAndBounds(t *testing.T) {
	replay := ReasoningReplay{ID: "rs_1", EncryptedContent: "gAAAAopaque"}
	signature, ok := EncodeReasoningSignature(replay)
	if !ok || !strings.HasPrefix(signature, "clodex:codex:v1:") {
		t.Fatalf("signature = %q, ok = %v", signature, ok)
	}
	if got, ok := DecodeReasoningSignature(signature); !ok || got != replay {
		t.Fatalf("decoded = %#v, ok = %v, want %#v", got, ok, replay)
	}
	for _, malformed := range []string{
		"anthropic-signature",
		"clodex:codex:v1:not-base64",
		"clodex:codex:v1:cnNfMQ:",
		"clodex:codex:v1::opaque",
	} {
		if got, ok := DecodeReasoningSignature(malformed); ok {
			t.Errorf("DecodeReasoningSignature(%q) = %#v, true", malformed, got)
		}
	}
	if _, ok := EncodeReasoningSignature(ReasoningReplay{ID: strings.Repeat("i", 4097), EncryptedContent: "x"}); ok {
		t.Fatal("oversize ID was encoded")
	}
	if _, ok := EncodeReasoningSignature(ReasoningReplay{ID: "rs", EncryptedContent: strings.Repeat("x", 8<<20+1)}); ok {
		t.Fatal("oversize encrypted content was encoded")
	}
}

func TestTranslateRequestFullHistoryGolden(t *testing.T) {
	signature, ok := EncodeReasoningSignature(ReasoningReplay{ID: "rs_1", EncryptedContent: "encrypted-reasoning"})
	if !ok {
		t.Fatal("EncodeReasoningSignature failed")
	}
	req := decodeRequest(t, `{
		"model":"claude-opus-4-1","max_tokens":4096,
		"system":[{"type":"text","text":"Rule one."},{"type":"text","text":"Rule two."}],
		"messages":[
			{"role":"user","content":[
				{"type":"text","text":"before"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}},
				{"type":"tool_result","tool_use_id":"call_0","is_error":true,"content":[
					{"type":"text","text":"bad"},
					{"type":"image","source":{"type":"url","url":"https://example.com/result.webp"}},
					{"type":"text","text":"after"}
				]},
				{"type":"text","text":"continue"}
			]},
			{"role":"assistant","content":[
				{"type":"text","text":"checking"},
				{"type":"thinking","thinking":"I checked.","signature":"`+signature+`"},
				{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"go"}},
				{"type":"tool_use","id":"call_2","name":"lookup","input":{"q":"rust"}},
				{"type":"text","text":"done"}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"call_1","content":"Go"},
				{"type":"tool_result","tool_use_id":"call_2","content":[{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"AQID"}}]}
			]}
		],
		"tools":[{"name":"lookup","description":"Look up a language.","input_schema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}]
	}`)

	result := translateOK(t, req, normalSelection(), Options{PromptCacheKey: "thread-1"})
	got, err := json.Marshal(result.Request)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt-5.4-mini","instructions":"Rule one.\n\nRule two.","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"before"},{"type":"input_image","image_url":"data:image/png;base64,AQID","detail":"auto"}]},{"type":"function_call_output","call_id":"call_0","output":[{"type":"input_text","text":"[tool execution error]"},{"type":"input_text","text":"bad"},{"type":"input_image","image_url":"https://example.com/result.webp","detail":"auto"},{"type":"input_text","text":"after"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"I checked."}],"encrypted_content":"encrypted-reasoning"},{"type":"function_call","name":"lookup","arguments":"{\"q\":\"go\"}","call_id":"call_1"},{"type":"function_call","name":"lookup","arguments":"{\"q\":\"rust\"}","call_id":"call_2"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"function_call_output","call_id":"call_1","output":"Go"},{"type":"function_call_output","call_id":"call_2","output":[{"type":"input_image","image_url":"data:image/jpeg;base64,AQID","detail":"auto"}]}],"tools":[{"type":"function","name":"lookup","description":"Look up a language.","strict":false,"parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}],"tool_choice":"auto","parallel_tool_calls":true,"reasoning":{"effort":"low","summary":"auto"},"store":false,"stream":true,"include":["reasoning.encrypted_content"],"prompt_cache_key":"thread-1"}`
	if string(got) != want {
		t.Fatalf("translated request mismatch:\n got: %s\nwant: %s", got, want)
	}
	if len(result.Warnings) != 1 || result.Warnings[0].Kind != WarningMaxTokensUnsupported || result.Warnings[0].Count != 1 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestTranslateCountTokensRequestDoesNotInventMaxTokenWarning(t *testing.T) {
	req, err := anthropic.DecodeCountTokensRequest(strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"count me"}],"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	result := translateOK(t, req, normalSelection(), Options{})
	if got := warningCount(result.Warnings, WarningMaxTokensUnsupported); got != 0 {
		t.Fatalf("max_tokens warning count = %d, want 0", got)
	}
}

func TestTranslateRequestSignatureOnlyAndRedactedThinking(t *testing.T) {
	signature, _ := EncodeReasoningSignature(ReasoningReplay{ID: "rs_only", EncryptedContent: "opaque-one"})
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"`+signature+`"},
			{"type":"redacted_thinking","data":"opaque-two"},
			{"type":"thinking","thinking":"visible","signature":"foreign-signature"}
		]}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	got, _ := json.Marshal(result.Request)
	if !strings.Contains(string(got), `{"type":"reasoning","summary":[],"encrypted_content":"opaque-one"}`) {
		t.Fatalf("signature-only reasoning absent: %s", got)
	}
	if !strings.Contains(string(got), `{"type":"reasoning","summary":[],"encrypted_content":"opaque-two"}`) {
		t.Fatalf("redacted reasoning absent: %s", got)
	}
	if !strings.Contains(string(got), `{"type":"reasoning","summary":[{"type":"summary_text","text":"visible"}],"encrypted_content":null}`) {
		t.Fatalf("foreign-signature visible reasoning absent: %s", got)
	}
	if warningCount(result.Warnings, WarningForeignThinkingSignature) != 1 {
		t.Fatalf("warnings = %#v", result.Warnings)
	}
}

func TestTranslateRequestControlsAndWarnings(t *testing.T) {
	tests := []struct {
		name              string
		choice            string
		tools             string
		wantChoice        string
		wantParallel      bool
		wantWarningCounts map[string]int
		wantStopSequences []string
	}{
		{name: "auto", choice: `{"type":"auto"}`, tools: `[{"name":"lookup","input_schema":{"type":"object"}}]`, wantChoice: `"auto"`, wantParallel: true},
		{name: "none", choice: `{"type":"none"}`, tools: `[{"name":"lookup","input_schema":{"type":"object"}}]`, wantChoice: `"none"`, wantParallel: true},
		{name: "any disables parallel", choice: `{"type":"any","disable_parallel_tool_use":true}`, tools: `[{"name":"lookup","input_schema":{"type":"object"}}]`, wantChoice: `"required"`, wantParallel: false},
		{name: "named function", choice: `{"type":"tool","name":"lookup"}`, tools: `[{"name":"lookup","input_schema":{"type":"object"}}]`, wantChoice: `{"type":"function","name":"lookup"}`, wantParallel: true},
		{name: "named web search", choice: `{"type":"tool","name":"web_search"}`, tools: `[{"type":"web_search_20250305","name":"web_search","allowed_domains":["example.com"],"blocked_domains":["spam.test"],"max_uses":3,"user_location":{"type":"approximate","country":"US"}}]`, wantChoice: `{"type":"allowed_tools","mode":"required","tools":[{"type":"web_search"}]}`, wantParallel: true, wantWarningCounts: map[string]int{WarningWebSearchUnmappableField: 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := decodeRequest(t, `{
				"model":"m","max_tokens":123,"messages":[{"role":"user","content":"go"}],
				"tools":`+tc.tools+`,"tool_choice":`+tc.choice+`,
				"temperature":0.2,"top_p":0.8,"metadata":{"user_id":"u"},
				"stop_sequences":["END","STOP"],"future_control":true
			}`)
			result := translateOK(t, req, normalSelection(), Options{})
			wire, _ := json.Marshal(result.Request)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["tool_choice"]) != tc.wantChoice || string(fields["parallel_tool_calls"]) != boolJSON(tc.wantParallel) {
				t.Fatalf("choice/parallel = %s/%s, want %s/%v", fields["tool_choice"], fields["parallel_tool_calls"], tc.wantChoice, tc.wantParallel)
			}
			if tc.name == "named web search" {
				wantTool := `[{"type":"web_search","external_web_access":true,"filters":{"allowed_domains":["example.com"]},"search_content_types":["text","image"]}]`
				if string(fields["tools"]) != wantTool {
					t.Fatalf("web search tools = %s, want %s", fields["tools"], wantTool)
				}
			}
			for _, unsupported := range []string{"max_output_tokens", "temperature", "top_p", "metadata", "stop"} {
				if _, present := fields[unsupported]; present {
					t.Errorf("unsupported field %q present", unsupported)
				}
			}
			for kind, want := range map[string]int{
				WarningMaxTokensUnsupported:   1,
				WarningTemperatureUnsupported: 1,
				WarningTopPUnsupported:        1,
				WarningMetadataUnsupported:    1,
				WarningUnknownRequestField:    1,
			} {
				if got := warningCount(result.Warnings, kind); got != want {
					t.Errorf("warning %s count = %d, want %d (%#v)", kind, got, want, result.Warnings)
				}
			}
			for kind, want := range tc.wantWarningCounts {
				if got := warningCount(result.Warnings, kind); got != want {
					t.Errorf("warning %s count = %d, want %d", kind, got, want)
				}
			}
			if !equalStrings(result.StopSequences, []string{"END", "STOP"}) {
				t.Fatalf("stop sequences = %#v", result.StopSequences)
			}
		})
	}
}

func TestTranslateRequestResponsesLiteExactPrefixAndDetailStripping(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,"system":"instructions",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"url","url":"https://example.com/input.png"}},
			{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"image","source":{"type":"url","url":"https://example.com/result.png"}}]}
		]}],
		"tools":[{"name":"read","input_schema":{"type":"object"}}]
	}`)
	selection := normalSelection()
	selection.Model.Slug = "gpt-5.6-sol"
	selection.Model.UseResponsesLite = true
	selection.Effort = "ultra"
	selection.ServiceTier = "priority"
	result := translateOK(t, req, selection, Options{})
	wire, _ := json.Marshal(result.Request)
	wantPrefix := `"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"read","description":"","strict":false,"parameters":{"type":"object"}}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"instructions"}]},`
	if !strings.Contains(string(wire), wantPrefix) {
		t.Fatalf("Responses Lite prefix mismatch: %s", wire)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(wire, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["instructions"]; present {
		t.Fatalf("Responses Lite instructions field present: %s", fields["instructions"])
	}
	if _, present := fields["tools"]; present {
		t.Fatalf("Responses Lite tools field present: %s", fields["tools"])
	}
	if strings.Contains(string(wire), `"detail"`) {
		t.Fatalf("Responses Lite retained image detail: %s", wire)
	}
	if string(fields["parallel_tool_calls"]) != "false" || string(fields["service_tier"]) != `"priority"` {
		t.Fatalf("lite controls = parallel %s tier %s", fields["parallel_tool_calls"], fields["service_tier"])
	}
	if string(fields["reasoning"]) != `{"effort":"max","summary":"auto","context":"all_turns"}` {
		t.Fatalf("lite reasoning = %s", fields["reasoning"])
	}
}

func TestTranslateRequestStableCacheKeyFromCacheControl(t *testing.T) {
	first := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"one"}]
	}`)
	second := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"system":[{"type":"text","text":"stable","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"one"},{"role":"assistant","content":"two"},{"role":"user","content":"three"}]
	}`)
	one := translateOK(t, first, normalSelection(), Options{})
	two := translateOK(t, second, normalSelection(), Options{})
	if one.Request.PromptCacheKey == "" || one.Request.PromptCacheKey != two.Request.PromptCacheKey || !strings.HasPrefix(one.Request.PromptCacheKey, "clodex-cache-v1-") {
		t.Fatalf("cache keys = %q / %q", one.Request.PromptCacheKey, two.Request.PromptCacheKey)
	}
	if one.Accounting.Source != one.Accounting.Mapped+one.Accounting.Warned {
		t.Fatalf("accounting = %#v", one.Accounting)
	}
}

func TestTranslateRequestUnknownSemanticItemsAreWarningAccounted(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,"future_top":true,
		"system":[{"type":"future_system","value":1}],
		"messages":[
			{"role":"user","content":[{"type":"future_user","value":2},{"type":"text","text":"go","future_text":true}]},
			{"role":"assistant","content":[{"type":"server_tool_use","id":"s","name":"web_search","input":{"query":"x"}},{"type":"future_assistant","value":3}]}
		],
		"tools":[{"type":"future_tool_20260101","name":"future"}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	for kind, want := range map[string]int{
		WarningUnknownRequestField: 1,
		WarningUnknownContentBlock: 4,
		WarningUnknownContentField: 1,
		WarningUnknownTool:         1,
	} {
		if got := warningCount(result.Warnings, kind); got != want {
			t.Errorf("warning %s = %d, want %d (%#v)", kind, got, want, result.Warnings)
		}
	}
	if result.Accounting.Source == 0 || result.Accounting.Source != result.Accounting.Mapped+result.Accounting.Warned {
		t.Fatalf("semantic accounting = %#v", result.Accounting)
	}
}

func decodeRequest(t *testing.T, body string) *anthropic.MessageRequest {
	t.Helper()
	req, err := anthropic.DecodeRequest(strings.NewReader(body))
	if err != nil {
		t.Fatalf("DecodeRequest() error = %v", err)
	}
	return req
}

func normalSelection() model.Selection {
	return model.Selection{
		Model: catalog.Model{
			Slug:                        "gpt-5.4-mini",
			SupportsParallelToolCalls:   true,
			SupportsImageDetailOriginal: true,
		},
		Effort: "low",
	}
}

func translateOK(t *testing.T, req *anthropic.MessageRequest, selection model.Selection, options Options) Result {
	t.Helper()
	result, err := TranslateRequest(req, selection, options)
	if err != nil {
		t.Fatalf("TranslateRequest() error = %v", err)
	}
	return result
}

func warningCount(warnings []Warning, kind string) int {
	for _, warning := range warnings {
		if warning.Kind == kind {
			return warning.Count
		}
	}
	return 0
}

func boolJSON(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestTranslateInArraySystemMessageBecomesDeveloperMessage(t *testing.T) {
	// Claude Code 2.1.x can place a role:"system" entry inside messages[]
	// (agent-types listing in headless mode). It maps to the same channel as
	// top-level system text — a developer-role message — at its position.
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"system","content":[{"type":"text","text":"Available agent types: reviewer."}]},
			{"role":"assistant","content":"ok"}
		]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	wire, _ := json.Marshal(result.Request)
	want := `{"type":"message","role":"developer","content":[{"type":"input_text","text":"Available agent types: reviewer."}]}`
	text := string(wire)
	if !strings.Contains(text, want) {
		t.Fatalf("developer message missing: %s", text)
	}
	userIdx := strings.Index(text, `"role":"user"`)
	devIdx := strings.Index(text, want)
	assistantIdx := strings.Index(text, `"role":"assistant"`)
	if !(userIdx < devIdx && devIdx < assistantIdx) {
		t.Fatalf("order wrong: user=%d dev=%d assistant=%d in %s", userIdx, devIdx, assistantIdx, text)
	}
	for _, warning := range result.Warnings {
		if warning.Kind == WarningUnknownContentBlock {
			t.Fatalf("warnings = %+v, want no unknown-content-block warning", result.Warnings)
		}
	}
}
