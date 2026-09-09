package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Aotricx/Clodex/internal/anthropic"
	"github.com/Aotricx/Clodex/internal/catalog"
	"github.com/Aotricx/Clodex/internal/codexwire"
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
	redacted, _ := EncodeReasoningSignature(ReasoningReplay{ID: "rs_redacted", EncryptedContent: "opaque-redacted"})
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"`+signature+`"},
			{"type":"redacted_thinking","data":"opaque-two"},
			{"type":"redacted_thinking","data":"`+redacted+`"},
			{"type":"thinking","thinking":"visible","signature":"foreign-signature"}
		]}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	got, _ := json.Marshal(result.Request)
	if !strings.Contains(string(got), `{"type":"reasoning","summary":[],"encrypted_content":"opaque-one"}`) {
		t.Fatalf("signature-only reasoning absent: %s", got)
	}
	if !strings.Contains(string(got), `{"type":"reasoning","summary":[],"encrypted_content":null}`) {
		t.Fatalf("foreign redacted thinking should omit/null encrypted_content: %s", got)
	}
	if strings.Contains(string(got), `"encrypted_content":"opaque-two"`) {
		t.Fatalf("opaque-two must not be forwarded as encrypted_content: %s", got)
	}
	if !strings.Contains(string(got), `{"type":"reasoning","summary":[],"encrypted_content":"opaque-redacted"}`) {
		t.Fatalf("valid redacted envelope did not round-trip: %s", got)
	}
	if !strings.Contains(string(got), `{"type":"reasoning","summary":[{"type":"summary_text","text":"visible"}],"encrypted_content":null}`) {
		t.Fatalf("foreign-signature visible reasoning absent: %s", got)
	}
	if warningCount(result.Warnings, WarningForeignThinkingSignature) != 2 {
		t.Fatalf("warnings = %#v, want 2 foreign thinking signatures (opaque-two + foreign-signature)", result.Warnings)
	}
}

func TestTranslateRequestThinkingTypeHonesty(t *testing.T) {
	disabled := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"disabled"}
	}`)
	disabledResult := translateOK(t, disabled, normalSelection(), Options{})
	if disabledResult.Request.Reasoning == nil || disabledResult.Request.Reasoning.Effort != "low" {
		t.Fatalf("disabled thinking reasoning = %#v, want selection effort", disabledResult.Request.Reasoning)
	}
	if warningCount(disabledResult.Warnings, WarningThinkingTypeUnsupported) != 1 {
		t.Fatalf("disabled thinking warnings = %#v, want thinking.type_unsupported", disabledResult.Warnings)
	}
	if disabledResult.Accounting.Source != disabledResult.Accounting.Mapped+disabledResult.Accounting.Warned {
		t.Fatalf("disabled thinking accounting = %#v", disabledResult.Accounting)
	}

	adaptive := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"adaptive","display":"summarized"}
	}`)
	adaptiveResult := translateOK(t, adaptive, normalSelection(), Options{})
	if warningCount(adaptiveResult.Warnings, WarningThinkingTypeUnsupported) != 1 {
		t.Fatalf("adaptive thinking warnings = %#v", adaptiveResult.Warnings)
	}

	omitted := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"enabled","budget_tokens":1024,"display":"omitted"}
	}`)
	omittedResult := translateOK(t, omitted, normalSelection(), Options{})
	if warningCount(omittedResult.Warnings, WarningThinkingDisplayUnsupported) != 1 {
		t.Fatalf("omitted display warnings = %#v, want thinking.display_unsupported", omittedResult.Warnings)
	}
	if warningCount(omittedResult.Warnings, WarningThinkingTypeUnsupported) != 0 {
		t.Fatalf("enabled+omitted should not warn type: %#v", omittedResult.Warnings)
	}

	enabled := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
		"thinking":{"type":"enabled","budget_tokens":2048}
	}`)
	enabledResult := translateOK(t, enabled, normalSelection(), Options{})
	if enabledResult.Request.Reasoning == nil || enabledResult.Request.Reasoning.Effort != "low" {
		t.Fatalf("enabled thinking reasoning = %#v", enabledResult.Request.Reasoning)
	}
	if warningCount(enabledResult.Warnings, WarningThinkingTypeUnsupported) != 0 || warningCount(enabledResult.Warnings, WarningThinkingDisplayUnsupported) != 0 {
		t.Fatalf("enabled thinking warnings = %#v, want no thinking unsupported warnings", enabledResult.Warnings)
	}
}

func TestTranslateRequestWarnsNestedRawExtras(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"thinking":{"type":"enabled","budget_tokens":1024,"future_think":1},
		"tool_choice":{"type":"auto","future_choice":true},
		"messages":[{"role":"user","future":1,"content":"x"}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	if warningCount(result.Warnings, WarningUnknownRequestField) < 3 {
		t.Fatalf("nested Raw extras warnings = %#v, want request.unknown_field for message, thinking, and tool_choice", result.Warnings)
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
		"tools":[{"name":"read","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
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

func TestTranslateRequestDocumentCacheControlEntersPromptCacheKey(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"messages":[{"role":"user","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="},"title":"notes.pdf","cache_control":{"type":"ephemeral"}}]}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	if result.Request.PromptCacheKey == "" || !strings.HasPrefix(result.Request.PromptCacheKey, "clodex-cache-v1-") {
		t.Fatalf("document cache_control prompt cache key = %q", result.Request.PromptCacheKey)
	}
	if result.Accounting.Mapped == 0 {
		t.Fatalf("document cache_control accounting = %#v", result.Accounting)
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
		WarningUnknownContentBlock: 3,
		WarningUnknownContentField: 1,
		WarningUnknownTool:         1,
	} {
		if got := warningCount(result.Warnings, kind); got != want {
			t.Errorf("warning %s = %d, want %d (%#v)", kind, got, want, result.Warnings)
		}
	}
	wire, _ := json.Marshal(result.Request)
	if !strings.Contains(string(wire), `"type":"web_search_call"`) || !strings.Contains(string(wire), `"query":"x"`) {
		t.Fatalf("web_search_call input missing: %s", wire)
	}
	if result.Accounting.Source == 0 || result.Accounting.Source != result.Accounting.Mapped+result.Accounting.Warned {
		t.Fatalf("semantic accounting = %#v", result.Accounting)
	}
}

func TestTranslateRequestInvertsServerToolUseWebSearchID(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"messages":[{"role":"assistant","content":[
			{"type":"server_tool_use","id":"srvtoolu_ws_1","name":"web_search","input":{"query":"golang"}}
		]}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	wire, err := json.Marshal(result.Request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"type":"web_search_call","id":"ws_1"`) {
		t.Fatalf("assistant server_tool_use srvtoolu_ws_1 must emit wire id ws_1: %s", wire)
	}
	if strings.Contains(string(wire), `"id":"srvtoolu_ws_1"`) {
		t.Fatalf("rewritten Anthropic id leaked onto wire: %s", wire)
	}
}

func TestTranslateRequestPassesThroughNonRewrittenWebSearchIDs(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{id: "s", want: `"id":"s"`},
		{id: "ws_1", want: `"id":"ws_1"`},
		{id: "srvtoolu_ws-1", want: `"id":"srvtoolu_ws-1"`},
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			req := decodeRequest(t, `{
				"model":"m","max_tokens":1,
				"messages":[{"role":"assistant","content":[
					{"type":"server_tool_use","id":`+mustJSON(t, tc.id)+`,"name":"web_search","input":{"query":"x"}}
				]}]
			}`)
			result := translateOK(t, req, normalSelection(), Options{})
			wire, err := json.Marshal(result.Request)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(wire), `"type":"web_search_call"`) || !strings.Contains(string(wire), tc.want) {
				t.Fatalf("id %q wire = %s, want %s", tc.id, wire, tc.want)
			}
		})
	}
}

func TestTranslateRequestMapsDocumentPDFToInputFile(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"messages":[{"role":"user","content":[
			{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="},"title":"notes.pdf"},
			{"type":"text","text":"summarize"},
			{"type":"document","source":{"type":"url","url":"https://example.com/spec.pdf"}}
		]}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	wire, err := json.Marshal(result.Request)
	if err != nil {
		t.Fatal(err)
	}
	got := string(wire)
	if warningCount(result.Warnings, WarningUnknownContentBlock) != 0 {
		t.Fatalf("document blocks were dropped as unknown: warnings=%#v wire=%s", result.Warnings, got)
	}
	if !strings.Contains(got, `"type":"input_file"`) || !strings.Contains(got, `"filename":"notes.pdf"`) {
		t.Fatalf("base64 document missing from wire: %s", got)
	}
	if !strings.Contains(got, `"file_data":"data:application/pdf;base64,JVBERi0xLjQ="`) {
		t.Fatalf("base64 document file_data missing: %s", got)
	}
	if !strings.Contains(got, `"file_url":"https://example.com/spec.pdf"`) {
		t.Fatalf("URL document missing from wire: %s", got)
	}
	if !strings.Contains(got, `"type":"input_text","text":"summarize"`) {
		t.Fatalf("sibling text missing: %s", got)
	}
}

func TestTranslateRequestMapsNestedDocumentInToolResult(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"messages":[{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call_1","content":[
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQ="},"title":"tool.pdf"}
			]}
		]}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	wire, err := json.Marshal(result.Request)
	if err != nil {
		t.Fatal(err)
	}
	got := string(wire)
	if warningCount(result.Warnings, WarningUnknownContentBlock) != 0 {
		t.Fatalf("nested document dropped: warnings=%#v wire=%s", result.Warnings, got)
	}
	if !strings.Contains(got, `"type":"input_file"`) || !strings.Contains(got, `"filename":"tool.pdf"`) {
		t.Fatalf("tool_result document missing: %s", got)
	}
}

func TestTranslateRequestMapsWebSearchQueriesWhenQueryAbsent(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"messages":[{"role":"assistant","content":[
			{"type":"server_tool_use","id":"srvtoolu_ws_1","name":"web_search","input":{"queries":["golang","docs"]}}
		]}]
	}`)
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 1 {
		t.Fatalf("decoded messages = %#v", req.Messages)
	}
	block := req.Messages[0].Content[0]
	if got, ok := webSearchQueryFromBlock(t, block); !ok || got != "golang" {
		t.Fatalf("webSearchQuery from decoded block = %q ok=%v raw=%s", got, ok, block.Raw)
	}
	result := translateOK(t, req, normalSelection(), Options{})
	wire, err := json.Marshal(result.Request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wire), `"type":"web_search_call","id":"ws_1"`) {
		t.Fatalf("queries input should map to web_search_call: %s warnings=%#v raw=%s", wire, result.Warnings, block.Raw)
	}
	if !strings.Contains(string(wire), `"query":"golang"`) {
		t.Fatalf("first queries entry should map onto action.query when possible: %s", wire)
	}
}

func webSearchQueryFromBlock(t *testing.T, block anthropic.ContentBlock) (string, bool) {
	t.Helper()
	var object map[string]json.RawMessage
	if json.Unmarshal(block.Raw, &object) != nil {
		return "", false
	}
	var input map[string]json.RawMessage
	if json.Unmarshal(object["input"], &input) != nil {
		return "", false
	}
	return webSearchQuery(input)
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestTranslateRequestMalformedWebSearchServerToolUseStaysUnknown(t *testing.T) {
	for _, block := range []string{
		`{"type":"server_tool_use","id":"s","name":"web_search","input":["not","object"]}`,
		`{"type":"server_tool_use","id":"s","name":"web_search","input":{}}`,
		`{"type":"server_tool_use","id":"s","name":"code_execution","input":{"query":"x"}}`,
	} {
		req := decodeRequest(t, `{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":[`+block+`]}]}`)
		result := translateOK(t, req, normalSelection(), Options{})
		wire, _ := json.Marshal(result.Request)
		if strings.Contains(string(wire), `"type":"web_search_call"`) {
			t.Fatalf("invented web_search_call for %s: %s", block, wire)
		}
		if warningCount(result.Warnings, WarningUnknownContentBlock) != 1 {
			t.Fatalf("warnings = %#v, want unknown content block for %s", result.Warnings, block)
		}
	}
}

func TestTranslateRequestWebSearchCacheControlIsWarned(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","cache_control":{"type":"ephemeral"}}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	if warningCount(result.Warnings, WarningWebSearchUnmappableField) != 1 {
		t.Fatalf("warnings = %#v, want cache_control warned as unmappable", result.Warnings)
	}
}

func TestTranslateRequestWebSearchToolInvalidFieldsAreWarnedNotInvented(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,"messages":[{"role":"user","content":"go"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","external_web_access":"yes","allowed_domains":"example.com","search_content_types":{"text":true}}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	if warningCount(result.Warnings, WarningWebSearchUnmappableField) < 3 {
		t.Fatalf("warnings = %#v, want unmarshal failures warned", result.Warnings)
	}
	wire, _ := json.Marshal(result.Request)
	if strings.Contains(string(wire), `"external_web_access":true`) {
		t.Fatalf("invented external_web_access default after unmarshal failure: %s", wire)
	}
	if strings.Contains(string(wire), `"search_content_types":["text","image"]`) {
		t.Fatalf("invented search_content_types default after unmarshal failure: %s", wire)
	}
	if strings.Contains(string(wire), `"allowed_domains"`) {
		t.Fatalf("invented allowed_domains after unmarshal failure: %s", wire)
	}
}

func TestTranslateRequestCacheControlUnknownFieldsAreWarned(t *testing.T) {
	req := decodeRequest(t, `{
		"model":"m","max_tokens":1,
		"cache_control":{"type":"ephemeral","foo":1},
		"messages":[{"role":"user","content":[{"type":"text","text":"go","cache_control":{"type":"ephemeral","foo":1}}]}]
	}`)
	result := translateOK(t, req, normalSelection(), Options{})
	if warningCount(result.Warnings, WarningUnknownContentField) != 1 {
		t.Fatalf("content cache_control warnings = %#v, want foo warned", result.Warnings)
	}
	if warningCount(result.Warnings, WarningUnknownRequestField) != 1 {
		t.Fatalf("request cache_control warnings = %#v, want foo warned", result.Warnings)
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

// A Lite-marked model must leave the Lite path when the client actually wants
// parallel tool calls, because the backend rejects Lite unless
// parallel_tool_calls is false. Without this the parallel capability would be
// silently unavailable on every Lite model.
func TestTranslateRequestLeavesResponsesLiteForParallelToolCalls(t *testing.T) {
	const body = `{
		"model":"m","max_tokens":1,"system":"instructions",
		"messages":[{"role":"user","content":"go"}],
		"tools":[{"name":"read_alpha","input_schema":{"type":"object"}},{"name":"read_beta","input_schema":{"type":"object"}}]%s
	}`
	liteSelection := func() model.Selection {
		selection := normalSelection()
		selection.Model.Slug = "gpt-5.6-sol"
		selection.Model.UseResponsesLite = true
		selection.Model.SupportsParallelToolCalls = true
		return selection
	}

	t.Run("parallel wanted uses full Responses protocol", func(t *testing.T) {
		result := translateOK(t, decodeRequest(t, fmt.Sprintf(body, "")), liteSelection(), Options{})
		if !result.Request.ParallelToolCalls {
			t.Fatal("ParallelToolCalls = false, want true")
		}
		if result.Request.Instructions != "instructions" || len(result.Request.Tools) != 2 {
			t.Fatalf("instructions=%q tools=%d, want top-level Responses fields", result.Request.Instructions, len(result.Request.Tools))
		}
		// An empty reasoning context is what keeps the Lite request header off.
		if result.Request.Reasoning == nil || result.Request.Reasoning.Context != "" {
			t.Fatalf("reasoning = %#v, want no Lite context", result.Request.Reasoning)
		}
		for _, item := range result.Request.Input {
			if _, isLite := item.(codexwire.AdditionalTools); isLite {
				t.Fatal("input still carries the Lite additional_tools carrier")
			}
		}
	})

	t.Run("parallel disabled stays on Lite", func(t *testing.T) {
		result := translateOK(t, decodeRequest(t, fmt.Sprintf(body, `,"tool_choice":{"type":"auto","disable_parallel_tool_use":true}`)), liteSelection(), Options{})
		if result.Request.ParallelToolCalls || result.Request.Reasoning == nil ||
			result.Request.Reasoning.Context != codexwire.ReasoningContextAllTurns {
			t.Fatalf("expected Lite request, got parallel=%v reasoning=%#v", result.Request.ParallelToolCalls, result.Request.Reasoning)
		}
	})

	t.Run("no tools stays on Lite", func(t *testing.T) {
		result := translateOK(t, decodeRequest(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"go"}]}`), liteSelection(), Options{})
		if result.Request.Reasoning == nil || result.Request.Reasoning.Context != codexwire.ReasoningContextAllTurns {
			t.Fatalf("expected Lite request, got reasoning=%#v", result.Request.Reasoning)
		}
	})
}
