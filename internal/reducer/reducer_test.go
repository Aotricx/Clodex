package reducer

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Aotricx/Clodex/internal/codexstream"
	"github.com/Aotricx/Clodex/internal/translate"
)

func TestReducerPreservesInterleavedContentOrder(t *testing.T) {
	events := []codexstream.Event{
		event(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[]}}`),
		event(`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"think"}`),
		event(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"think"}]}}`),
		event(`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}`),
		event(`{"type":"response.function_call_arguments.delta","output_index":1,"call_id":"call_1","delta":"{\"q\":"}`),
		event(`{"type":"response.function_call_arguments.delta","output_index":1,"call_id":"call_1","delta":"\"one\"}"}`),
		event(`{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"one\"}"}}`),
		event(`{"type":"response.output_item.added","output_index":2,"item":{"id":"rs_2","type":"reasoning","encrypted_content":"opaque-2","summary":[]}}`),
		event(`{"type":"response.reasoning_summary_text.done","output_index":2,"summary_index":0,"text":"between"}`),
		event(`{"type":"response.output_item.done","output_index":2,"item":{"id":"rs_2","type":"reasoning","encrypted_content":"opaque-2","summary":[{"type":"summary_text","text":"between"}]}}`),
		event(`{"type":"response.output_item.added","output_index":3,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"lookup","arguments":""}}`),
		event(`{"type":"response.function_call_arguments.done","output_index":3,"call_id":"call_2","arguments":"{\"q\":\"two\"}"}`),
		event(`{"type":"response.output_item.done","output_index":3,"item":{"id":"fc_2","type":"function_call","call_id":"call_2","name":"lookup","arguments":"{\"q\":\"two\"}"}}`),
		event(`{"type":"response.output_text.done","output_index":4,"content_index":0,"text":"answer"}`),
		event(`{"type":"response.output_item.done","output_index":4,"item":{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`),
		event(completed(`{"id":"resp_1","usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":3},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":19}}`)),
	}

	r := New()
	var got []Event
	for _, upstream := range events {
		produced, err := r.Push(upstream)
		if err != nil {
			t.Fatalf("Push %s: %v", upstream.Type, err)
		}
		got = append(got, produced...)
	}

	sig1, _ := translate.EncodeReasoningSignature(translate.ReasoningReplay{ID: "rs_1", EncryptedContent: "opaque"})
	sig2, _ := translate.EncodeReasoningSignature(translate.ReasoningReplay{ID: "rs_2", EncryptedContent: "opaque-2"})
	want := []Event{
		{Kind: KindContentStart, Index: 0, Block: ContentBlock{Type: BlockThinking}},
		{Kind: KindContentDelta, Index: 0, Delta: Delta{Type: DeltaThinking, Text: "think"}},
		{Kind: KindContentDelta, Index: 0, Delta: Delta{Type: DeltaSignature, Text: sig1}},
		{Kind: KindContentStop, Index: 0},
		{Kind: KindContentStart, Index: 1, Block: ContentBlock{Type: BlockToolUse, ID: "call_1", Name: "lookup"}},
		{Kind: KindContentDelta, Index: 1, Delta: Delta{Type: DeltaInputJSON, Text: `{"q":`}},
		{Kind: KindContentDelta, Index: 1, Delta: Delta{Type: DeltaInputJSON, Text: `"one"}`}},
		{Kind: KindContentStop, Index: 1},
		{Kind: KindContentStart, Index: 2, Block: ContentBlock{Type: BlockThinking}},
		{Kind: KindContentDelta, Index: 2, Delta: Delta{Type: DeltaThinking, Text: "between"}},
		{Kind: KindContentDelta, Index: 2, Delta: Delta{Type: DeltaSignature, Text: sig2}},
		{Kind: KindContentStop, Index: 2},
		{Kind: KindContentStart, Index: 3, Block: ContentBlock{Type: BlockToolUse, ID: "call_2", Name: "lookup"}},
		{Kind: KindContentDelta, Index: 3, Delta: Delta{Type: DeltaInputJSON, Text: `{"q":"two"}`}},
		{Kind: KindContentStop, Index: 3},
		{Kind: KindContentStart, Index: 4, Block: ContentBlock{Type: BlockText}},
		{Kind: KindContentDelta, Index: 4, Delta: Delta{Type: DeltaText, Text: "answer"}},
		{Kind: KindContentStop, Index: 4},
		{Kind: KindTerminal, Terminal: &Terminal{StopReason: StopToolUse, ResponseID: "resp_1", Usage: Usage{InputTokens: 7, OutputTokens: 9, CacheReadInputTokens: 3}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events mismatch\n got: %#v\nwant: %#v", got, want)
	}

	result, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	if result.StopReason != StopToolUse || result.Usage.OutputTokens != 9 || result.Usage.CacheReadInputTokens != 3 {
		t.Fatalf("result terminal = %#v", result)
	}
	if len(result.Content) != 5 || result.Content[0].Thinking != "think" || result.Content[0].Signature != sig1 || string(result.Content[1].Input) != `{"q":"one"}` || result.Content[4].Text != "answer" {
		t.Fatalf("result content = %#v", result.Content)
	}
}

func TestReducerPreservesParallelFunctionArgumentStreams(t *testing.T) {
	r := New()
	upstream := []codexstream.Event{
		event(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"a","name":"one","arguments":""}}`),
		event(`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"b","name":"two","arguments":""}}`),
		event(`{"type":"response.function_call_arguments.delta","output_index":1,"call_id":"b","delta":"{\"b\":2}"}`),
		event(`{"type":"response.function_call_arguments.delta","output_index":0,"call_id":"a","delta":"{\"a\":1}"}`),
		event(`{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"b","name":"two","arguments":"{\"b\":2}"}}`),
		event(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"a","name":"one","arguments":"{\"a\":1}"}}`),
		event(completed(`{"id":"r","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)),
	}
	var got []Event
	for _, item := range upstream {
		out, err := r.Push(item)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out...)
	}
	wantKindsAndIndexes := [][2]int{
		{int(KindContentStart), 0}, {int(KindContentStart), 1},
		{int(KindContentDelta), 1}, {int(KindContentDelta), 0},
		{int(KindContentStop), 1}, {int(KindContentStop), 0}, {int(KindTerminal), 0},
	}
	if len(got) != len(wantKindsAndIndexes) {
		t.Fatalf("events = %#v", got)
	}
	for i, want := range wantKindsAndIndexes {
		if int(got[i].Kind) != want[0] || got[i].Index != want[1] {
			t.Fatalf("event %d = %#v, want kind/index %v", i, got[i], want)
		}
	}
}

func TestReducerMapsRefusalWebSearchAndUnknownServerTool(t *testing.T) {
	t.Run("refusal", func(t *testing.T) {
		r := New()
		input := []codexstream.Event{
			event(`{"type":"response.refusal.delta","output_index":0,"content_index":0,"delta":"cannot"}`),
			event(`{"type":"response.refusal.done","output_index":0,"content_index":0,"refusal":"cannot"}`),
			event(completed(`{"id":"r","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)),
		}
		for _, item := range input {
			if _, err := r.Push(item); err != nil {
				t.Fatal(err)
			}
		}
		result, err := r.Result()
		if err != nil || result.StopReason != StopRefusal || len(result.Content) != 1 || result.Content[0].Text != "cannot" {
			t.Fatalf("result = %#v, %v", result, err)
		}
	})

	t.Run("web search", func(t *testing.T) {
		r := New()
		input := []codexstream.Event{
			event(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"in_progress"}}`),
			event(`{"type":"response.web_search_call.searching","item_id":"ws_1","output_index":0}`),
			event(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"golang"}}}`),
			event(completed(`{"id":"r","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)),
		}
		var got []Event
		for _, item := range input {
			out, err := r.Push(item)
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, out...)
		}
		result, err := r.Result()
		if err != nil || len(result.Content) != 1 || result.Content[0].Type != BlockServerToolUse || result.Content[0].ID != "srvtoolu_ws_1" || string(result.Content[0].Input) != `{"query":"golang"}` {
			t.Fatalf("result = %#v, %v", result, err)
		}
		if got[len(got)-1].Terminal.StopReason != StopEndTurn {
			t.Fatalf("terminal = %#v", got[len(got)-1])
		}
	})

	t.Run("unknown server tool", func(t *testing.T) {
		r := New()
		input := []codexstream.Event{
			event(`{"type":"response.output_item.added","output_index":0,"item":{"id":"srv_1","type":"future_server_tool","status":"in_progress"}}`),
			event(`{"type":"response.output_text.done","output_index":1,"content_index":0,"text":"fallback"}`),
			event(completed(`{"id":"r","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`)),
		}
		var warnings []Warning
		for _, item := range input {
			out, err := r.Push(item)
			if err != nil {
				t.Fatal(err)
			}
			for _, semantic := range out {
				if semantic.Warning != nil {
					warnings = append(warnings, *semantic.Warning)
				}
			}
		}
		result, err := r.Result()
		if err != nil || !reflect.DeepEqual(warnings, []Warning{{Kind: WarningUnknownServerTool, Count: 1}}) || !reflect.DeepEqual(result.Warnings, warnings) {
			t.Fatalf("warnings/result = %#v / %#v, %v", warnings, result, err)
		}
	})
}

func TestReducerMapsWebSearchQueryArrayAndSanitizesServerToolID(t *testing.T) {
	r := New()
	input := []codexstream.Event{
		event(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ws-1/x","type":"web_search_call"}}`),
		event(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ws-1/x","type":"web_search_call","action":{"type":"search","queries":["first","second"]}}}`),
		event(completed(`{"id":"r","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)),
	}
	for _, item := range input {
		if _, err := r.Push(item); err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.Result()
	if err != nil || len(result.Content) != 1 || result.Content[0].ID != "srvtoolu_ws_1_x" || string(result.Content[0].Input) != `{"query":"first"}` || !reflect.DeepEqual(result.Warnings, []Warning{{Kind: WarningWebSearchAction, Count: 1}}) {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestReducerStripsUnknownContentPartWithWarning(t *testing.T) {
	r := New()
	input := []codexstream.Event{
		event(`{"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"future_server_result","value":1}}`),
		event(`{"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"future_server_result","value":1}}`),
		event(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","content":[{"type":"future_server_result","value":1}]}}`),
		event(`{"type":"response.output_text.done","output_index":1,"content_index":0,"text":"fallback"}`),
		event(completed(`{"id":"r","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)),
	}
	var got []Event
	for _, item := range input {
		out, err := r.Push(item)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out...)
	}
	result, err := r.Result()
	if err != nil || len(result.Content) != 1 || result.Content[0].Text != "fallback" || !reflect.DeepEqual(result.Warnings, []Warning{{Kind: WarningUnknownServerTool, Count: 1}}) {
		t.Fatalf("result = %#v, %v", result, err)
	}
	for _, item := range got {
		if (item.Kind == KindContentStart || item.Kind == KindContentStop || item.Kind == KindContentDelta) && item.Index != 0 {
			t.Fatalf("unknown content allocated block: %#v", got)
		}
	}
}

func TestReducerTerminalTable(t *testing.T) {
	tests := []struct {
		name       string
		content    []codexstream.Event
		terminal   codexstream.Event
		wantReason StopReason
		wantErr    error
	}{
		{
			name:       "completed end turn",
			content:    []codexstream.Event{event(`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ok"}`)},
			terminal:   event(completed(`{"id":"c","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`)),
			wantReason: StopEndTurn,
		},
		{
			name:       "done end turn",
			content:    []codexstream.Event{event(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"ok"}`)},
			terminal:   event(`{"type":"response.done","response":{"id":"d","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`),
			wantReason: StopEndTurn,
		},
		{
			name: "tool use",
			content: []codexstream.Event{
				event(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"run","arguments":""}}`),
				event(`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{}"}`),
				event(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c","name":"run","arguments":"{}"}}`),
			},
			terminal:   event(completed(`{"id":"t","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)),
			wantReason: StopToolUse,
		},
		{
			name:       "incomplete max output",
			content:    []codexstream.Event{event(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`)},
			terminal:   event(`{"type":"response.incomplete","response":{"id":"i","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12}}}`),
			wantReason: StopMaxTokens,
		},
		{
			name:       "empty incomplete remains nonretryable",
			terminal:   event(`{"type":"response.incomplete","response":{"id":"i","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":5,"output_tokens":0,"total_tokens":5}}}`),
			wantReason: StopMaxTokens,
		},
		{
			name:     "empty completed",
			terminal: event(completed(`{"id":"e","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}`)),
			wantErr:  ErrEmptyCompletion,
		},
		{
			name:     "empty done",
			terminal: event(`{"type":"response.done","response":{"id":"e","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`),
			wantErr:  ErrEmptyCompletion,
		},
		{
			name:     "semantic missing usage",
			content:  []codexstream.Event{event(`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ok"}`)},
			terminal: event(completed(`{"id":"m","usage":{}}`)),
			wantErr:  ErrMissingUsage,
		},
		{
			name:     "failed",
			terminal: event(`{"type":"response.failed","response":{"id":"f","error":{"message":"backend exploded"}}}`),
			wantErr:  ErrUpstreamFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			for _, item := range tt.content {
				if _, err := r.Push(item); err != nil {
					t.Fatalf("content Push: %v", err)
				}
			}
			out, err := r.Push(tt.terminal)
			if tt.wantErr != nil {
				requireError(t, err, tt.wantErr)
				if _, resultErr := r.Result(); !errors.Is(resultErr, tt.wantErr) {
					t.Fatalf("Result error = %v, want %v", resultErr, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(out) == 0 || out[len(out)-1].Kind != KindTerminal || out[len(out)-1].Terminal.StopReason != tt.wantReason {
				t.Fatalf("terminal events = %#v", out)
			}
			result, err := r.Result()
			if err != nil || result.StopReason != tt.wantReason {
				t.Fatalf("Result = %#v, %v", result, err)
			}
		})
	}
}

func TestReducerIncompleteMissingUsageIsMaxTokens(t *testing.T) {
	tests := []struct {
		name     string
		content  []codexstream.Event
		terminal string
	}{
		{
			name:     "empty usage object",
			terminal: `{"type":"response.incomplete","response":{"id":"i","incomplete_details":{"reason":"max_output_tokens"},"usage":{}}}`,
		},
		{
			name:     "omitted usage",
			terminal: `{"type":"response.incomplete","response":{"id":"i","incomplete_details":{"reason":"max_output_tokens"}}}`,
		},
		{
			name:     "semantic text with empty usage",
			content:  []codexstream.Event{event(`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`)},
			terminal: `{"type":"response.incomplete","response":{"id":"i","incomplete_details":{"reason":"max_output_tokens"},"usage":{}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New()
			for _, item := range tt.content {
				if _, err := r.Push(item); err != nil {
					t.Fatalf("content Push: %v", err)
				}
			}
			out, err := r.Push(event(tt.terminal))
			if err != nil {
				t.Fatalf("Push incomplete: %v", err)
			}
			if len(out) == 0 || out[len(out)-1].Kind != KindTerminal || out[len(out)-1].Terminal.StopReason != StopMaxTokens {
				t.Fatalf("terminal events = %#v", out)
			}
			result, err := r.Result()
			if err != nil {
				t.Fatal(err)
			}
			if result.StopReason != StopMaxTokens || result.Usage != (Usage{}) {
				t.Fatalf("Result = %#v, want StopMaxTokens and zero usage", result)
			}
		})
	}
}

func TestReducerUsageUsesUpstreamTotalsWithoutDoubleCountingReasoning(t *testing.T) {
	r := New()
	input := []codexstream.Event{
		event(`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ok"}`),
		event(completed(`{"id":"r","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":7},"output_tokens":30,"output_tokens_details":{"reasoning_tokens":20},"total_tokens":130}}`)),
	}
	for _, item := range input {
		if _, err := r.Push(item); err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{InputTokens: 60, OutputTokens: 30, CacheReadInputTokens: 40, CacheCreationInputTokens: 7}
	if result.Usage != want {
		t.Fatalf("usage = %#v, want %#v", result.Usage, want)
	}
}

func TestReducerSignatureOnlyReasoningIsSemanticOutput(t *testing.T) {
	r := New()
	input := []codexstream.Event{
		event(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs","type":"reasoning","encrypted_content":"opaque","summary":[]}}`),
		event(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs","type":"reasoning","encrypted_content":"opaque","summary":[]}}`),
		event(completed(`{"id":"r","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)),
	}
	for _, item := range input {
		if _, err := r.Push(item); err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.Result()
	if err != nil || len(result.Content) != 1 || result.Content[0].Thinking != "" || result.Content[0].Signature == "" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestReducerSeparatesMultipleReasoningSummaryParts(t *testing.T) {
	r := New()
	input := []codexstream.Event{
		event(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs","type":"reasoning","encrypted_content":"opaque","summary":[]}}`),
		event(`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":0,"delta":"first"}`),
		event(`{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":""}}`),
		event(`{"type":"response.reasoning_summary_text.delta","output_index":0,"summary_index":1,"delta":"second"}`),
		event(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs","type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`),
		event(completed(`{"id":"r","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)),
	}
	var got []Event
	for _, item := range input {
		out, err := r.Push(item)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, out...)
	}
	result, err := r.Result()
	if err != nil || len(result.Content) != 1 || result.Content[0].Thinking != "first\n\nsecond" {
		t.Fatalf("result = %#v, %v", result, err)
	}
	foundSeparator := false
	for _, item := range got {
		if item.Kind == KindContentDelta && item.Delta == (Delta{Type: DeltaThinking, Text: "\n\n"}) {
			foundSeparator = true
		}
	}
	if !foundSeparator {
		t.Fatalf("summary separator absent: %#v", got)
	}
}

func TestReducerSeparatesBufferedReasoningSummaryParts(t *testing.T) {
	r := New()
	input := []codexstream.Event{
		event(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs","type":"reasoning","encrypted_content":"opaque","summary":[{"type":"summary_text","text":"first"},{"type":"summary_text","text":"second"}]}}`),
		event(completed(`{"id":"r","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)),
	}
	for _, item := range input {
		if _, err := r.Push(item); err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.Result()
	if err != nil || len(result.Content) != 1 || result.Content[0].Thinking != "first\n\nsecond" {
		t.Fatalf("result = %#v, %v", result, err)
	}
}

func TestReducerDuplicateTerminalRules(t *testing.T) {
	r := New()
	response := `{"id":"r","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
	for _, item := range []codexstream.Event{
		event(`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ok"}`),
		event(completed(response)),
	} {
		if _, err := r.Push(item); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := r.Push(event(`{"type":"response.done","response":` + response + `}`)); err != nil || len(out) != 0 {
		t.Fatalf("equivalent duplicate = %#v, %v", out, err)
	}
	contradictory := event(`{"type":"response.done","response":{"id":"r","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
	if _, err := r.Push(contradictory); !errors.Is(err, ErrContradictoryTerminal) {
		t.Fatalf("contradictory duplicate = %v", err)
	}
	if _, err := r.Push(event(`{"type":"response.output_text.delta","output_index":0,"delta":"late"}`)); !errors.Is(err, ErrEventAfterTerminal) {
		t.Fatalf("event after terminal = %v", err)
	}
}

func TestReducerPushAfterFinalErrKeepsReturningThatError(t *testing.T) {
	r := New()
	terminal := event(completed(`{"id":"e","usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}`))
	_, err := r.Push(terminal)
	requireError(t, err, ErrEmptyCompletion)

	out, err := r.Push(terminal)
	if len(out) != 0 || !errors.Is(err, ErrEmptyCompletion) {
		t.Fatalf("duplicate terminal after finalErr = %#v, %v", out, err)
	}
	out, err = r.Push(event(`{"type":"response.output_text.delta","output_index":0,"delta":"late"}`))
	if len(out) != 0 || !errors.Is(err, ErrEmptyCompletion) {
		t.Fatalf("event after finalErr = %#v, %v", out, err)
	}
}

func TestReducerPreservesInformationalRateLimitSnapshot(t *testing.T) {
	r := New()
	snapshot := event(`{"type":"codex.rate_limits","rate_limits":{"limit_reached":true},"credits":{"has_credits":true}}`)
	out, err := r.Push(snapshot)
	if err != nil || len(out) != 1 || out[0].Kind != KindRateLimits || !json.Valid(out[0].RateLimits.Raw) {
		t.Fatalf("snapshot = %#v, %v", out, err)
	}
	if _, err := r.Result(); !errors.Is(err, ErrNotTerminal) {
		t.Fatalf("snapshot Result = %v, want ErrNotTerminal", err)
	}
	for _, item := range []codexstream.Event{
		event(`{"type":"response.output_text.done","output_index":0,"content_index":0,"text":"ok"}`),
		event(completed(`{"id":"r","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)),
	} {
		if _, err := r.Push(item); err != nil {
			t.Fatal(err)
		}
	}
	result, err := r.Result()
	if err != nil || len(result.RateLimitSnapshots) != 1 || !reflect.DeepEqual(result.RateLimitSnapshots[0], snapshot.Raw) {
		t.Fatalf("result snapshots = %#v, %v", result.RateLimitSnapshots, err)
	}
}

func TestReducerFailedTerminalDominatesUnfinishedContent(t *testing.T) {
	r := New()
	if _, err := r.Push(event(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs","type":"reasoning","summary":[]}}`)); err != nil {
		t.Fatal(err)
	}
	_, err := r.Push(event(`{"type":"response.failed","response":{"id":"r","error":{"message":"real upstream failure"}}}`))
	if !errors.Is(err, ErrUpstreamFailed) || !strings.Contains(err.Error(), "real upstream failure") {
		t.Fatalf("failed terminal = %v, want upstream failure", err)
	}
}

func event(raw string) codexstream.Event {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(raw), &header); err != nil {
		panic(err)
	}
	return codexstream.Event{Type: header.Type, Raw: json.RawMessage(raw)}
}

func completed(response string) string {
	return `{"type":"response.completed","response":` + response + `}`
}

func requireError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("error = %v, want %v", got, want)
	}
}
