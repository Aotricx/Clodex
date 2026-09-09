package anthropicstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Aotricx/Clodex/internal/reducer"
)

func TestEncoderWritesExactAnthropicSSEOrder(t *testing.T) {
	var output bytes.Buffer
	flushes := 0
	encoder, err := New(&output, Options{
		MessageID: "msg_1",
		Model:     "gpt-5.6-sol:xhigh",
		Flush:     func() error { flushes++; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	events := []reducer.Event{
		{Kind: reducer.KindContentStart, Index: 0, Block: reducer.ContentBlock{Type: reducer.BlockThinking}},
		{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaThinking, Text: "plan"}},
		{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaSignature, Text: "clodex:sig"}},
		{Kind: reducer.KindContentStop, Index: 0},
		{Kind: reducer.KindContentStart, Index: 1, Block: reducer.ContentBlock{Type: reducer.BlockToolUse, ID: "call_1", Name: "run"}},
		{Kind: reducer.KindContentDelta, Index: 1, Delta: reducer.Delta{Type: reducer.DeltaInputJSON, Text: `{"cmd":`}},
		{Kind: reducer.KindContentDelta, Index: 1, Delta: reducer.Delta{Type: reducer.DeltaInputJSON, Text: `"go test"}`}},
		{Kind: reducer.KindContentStop, Index: 1},
		{Kind: reducer.KindContentStart, Index: 2, Block: reducer.ContentBlock{Type: reducer.BlockText}},
		{Kind: reducer.KindContentDelta, Index: 2, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "done"}},
		{Kind: reducer.KindContentStop, Index: 2},
		{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{
			StopReason: reducer.StopToolUse,
			Usage:      reducer.Usage{InputTokens: 7, OutputTokens: 9, CacheReadInputTokens: 3, CacheCreationInputTokens: 2},
		}},
	}
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatalf("Encode(%#v): %v", event, err)
		}
	}
	want := strings.Join([]string{
		frame("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"gpt-5.6-sol:xhigh","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`),
		frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"plan"}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"clodex:sig"}}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":0}`),
		frame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"run","input":{}}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":"}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"go test\"}"}}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":1}`),
		frame("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`),
		frame("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"done"}}`),
		frame("content_block_stop", `{"type":"content_block_stop","index":2}`),
		frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":7,"output_tokens":9,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}`),
		frame("message_stop", `{"type":"message_stop"}`),
	}, "")
	if output.String() != want {
		t.Fatalf("SSE mismatch\n got: %s\nwant: %s", output.String(), want)
	}
	if flushes != len(events)+2 { // message_start and terminal's second frame are extra.
		t.Fatalf("flushes = %d, want %d", flushes, len(events)+2)
	}
}

func TestEncoderTerminalOnlyMessageStartUsesKnownUsage(t *testing.T) {
	var output bytes.Buffer
	encoder, err := New(&output, Options{MessageID: "m", Model: "gpt"})
	if err != nil {
		t.Fatal(err)
	}
	usage := reducer.Usage{InputTokens: 7, OutputTokens: 9, CacheCreationInputTokens: 2, CacheReadInputTokens: 3}
	if err := encoder.Encode(reducer.Event{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{
		StopReason: reducer.StopEndTurn,
		Usage:      usage,
	}}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	wantStart := frame("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"gpt","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":9,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}}`)
	wantDelta := frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":7,"output_tokens":9,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}`)
	if !strings.Contains(got, wantStart) {
		t.Fatalf("message_start missing known usage\n got: %s\nwant: %s", got, wantStart)
	}
	if !strings.Contains(got, wantDelta) {
		t.Fatalf("message_delta missing final totals\n got: %s\nwant: %s", got, wantDelta)
	}
}

func TestEncoderStopSequenceAcrossTextDeltas(t *testing.T) {
	var output bytes.Buffer
	encoder, err := New(&output, Options{MessageID: "m", Model: "gpt", StopSequences: []string{"END"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []reducer.Event{
		{Kind: reducer.KindContentStart, Index: 0, Block: reducer.ContentBlock{Type: reducer.BlockText}},
		{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "hello EN"}},
		{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "D ignored"}},
		{Kind: reducer.KindContentStop, Index: 0},
		{Kind: reducer.KindContentStart, Index: 1, Block: reducer.ContentBlock{Type: reducer.BlockToolUse, ID: "late", Name: "late"}},
		{Kind: reducer.KindContentStop, Index: 1},
		{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{StopReason: reducer.StopToolUse, Usage: reducer.Usage{InputTokens: 3, OutputTokens: 4}}},
	} {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	text := output.String()
	if !strings.Contains(text, `"text":"hello "`) || strings.Contains(text, "ignored") || strings.Contains(text, `"id":"late"`) {
		t.Fatalf("truncated stream = %s", text)
	}
	if !strings.Contains(text, `"stop_reason":"stop_sequence","stop_sequence":"END"`) || encoder.MatchedStop() != "END" {
		t.Fatalf("terminal/match = %s / %q", text, encoder.MatchedStop())
	}
	assertFrameOrder(t, text, "message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop")
}

func TestEncoderStopMatchAlignsDroppedBlocksWithApplyStops(t *testing.T) {
	stops := []string{"END"}
	usage := reducer.Usage{InputTokens: 3, OutputTokens: 4}

	t.Run("later open tool is dropped", func(t *testing.T) {
		events := []reducer.Event{
			{Kind: reducer.KindContentStart, Index: 0, Block: reducer.ContentBlock{Type: reducer.BlockText}},
			{Kind: reducer.KindContentStart, Index: 1, Block: reducer.ContentBlock{Type: reducer.BlockToolUse, ID: "late", Name: "lookup"}},
			{Kind: reducer.KindContentDelta, Index: 1, Delta: reducer.Delta{Type: reducer.DeltaInputJSON, Text: `{"q":1}`}},
			{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "hello END extra"}},
			{Kind: reducer.KindContentStop, Index: 0},
			{Kind: reducer.KindContentStop, Index: 1},
			{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{StopReason: reducer.StopToolUse, Usage: usage}},
		}
		stream, truncated, body := encodeAndBuffer(t, stops, events, reducer.Result{
			Content: []reducer.ContentBlock{
				{Type: reducer.BlockText, Text: "hello END extra"},
				{Type: reducer.BlockToolUse, ID: "late", Name: "lookup", Input: json.RawMessage(`{"q":1}`)},
			},
			StopReason: reducer.StopToolUse,
			Usage:      usage,
		})
		if truncated.StopReason != reducer.StopStopSequence || truncated.StopSequence != "END" || len(truncated.Content) != 1 || truncated.Content[0].Text != "hello " {
			t.Fatalf("ApplyStops = %#v", truncated)
		}
		if strings.Contains(string(body), `"id":"late"`) {
			t.Fatalf("buffered kept dropped tool: %s", body)
		}
		if !strings.Contains(stream, `"text":"hello "`) || !strings.Contains(stream, `"stop_reason":"stop_sequence","stop_sequence":"END"`) {
			t.Fatalf("stream = %s", stream)
		}
		if !strings.Contains(stream, frame("content_block_stop", `{"type":"content_block_stop","index":1}`)) {
			t.Fatalf("stream missing content_block_stop for remaining open tool: %s", stream)
		}
		if !strings.Contains(stream, frame("content_block_stop", `{"type":"content_block_stop","index":0}`)) {
			t.Fatalf("stream missing matching text stop: %s", stream)
		}
	})

	t.Run("earlier open tool is kept", func(t *testing.T) {
		events := []reducer.Event{
			{Kind: reducer.KindContentStart, Index: 0, Block: reducer.ContentBlock{Type: reducer.BlockToolUse, ID: "early", Name: "lookup"}},
			{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaInputJSON, Text: `{"q":1}`}},
			{Kind: reducer.KindContentStart, Index: 1, Block: reducer.ContentBlock{Type: reducer.BlockText}},
			{Kind: reducer.KindContentDelta, Index: 1, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "hello END extra"}},
			{Kind: reducer.KindContentStop, Index: 1},
			{Kind: reducer.KindContentStop, Index: 0},
			{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{StopReason: reducer.StopToolUse, Usage: usage}},
		}
		stream, truncated, body := encodeAndBuffer(t, stops, events, reducer.Result{
			Content: []reducer.ContentBlock{
				{Type: reducer.BlockToolUse, ID: "early", Name: "lookup", Input: json.RawMessage(`{"q":1}`)},
				{Type: reducer.BlockText, Text: "hello END extra"},
			},
			StopReason: reducer.StopToolUse,
			Usage:      usage,
		})
		if truncated.StopReason != reducer.StopStopSequence || len(truncated.Content) != 2 || truncated.Content[1].Text != "hello " {
			t.Fatalf("ApplyStops = %#v", truncated)
		}
		if !strings.Contains(string(body), `"id":"early"`) || !strings.Contains(string(body), `"text":"hello "`) {
			t.Fatalf("buffered dropped earlier tool: %s", body)
		}
		if !strings.Contains(stream, frame("content_block_stop", `{"type":"content_block_stop","index":0}`)) {
			t.Fatalf("stream missing earlier tool stop: %s", stream)
		}
		if !strings.Contains(stream, `"stop_reason":"stop_sequence","stop_sequence":"END"`) {
			t.Fatalf("stream = %s", stream)
		}
	})
}

func TestEncoderKeepsEarlierDeltasAfterStopMatch(t *testing.T) {
	var output bytes.Buffer
	encoder, err := New(&output, Options{MessageID: "m", Model: "gpt", StopSequences: []string{"END"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []reducer.Event{
		{Kind: reducer.KindContentStart, Index: 0, Block: reducer.ContentBlock{Type: reducer.BlockToolUse, ID: "early", Name: "lookup"}},
		{Kind: reducer.KindContentStart, Index: 1, Block: reducer.ContentBlock{Type: reducer.BlockText}},
		{Kind: reducer.KindContentDelta, Index: 1, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "hello END extra"}},
		{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaInputJSON, Text: `{"q":1}`}},
		{Kind: reducer.KindContentDelta, Index: 1, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "dropped"}},
		{Kind: reducer.KindContentStop, Index: 1},
		{Kind: reducer.KindContentStop, Index: 0},
		{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{StopReason: reducer.StopEndTurn, Usage: reducer.Usage{InputTokens: 3, OutputTokens: 4}}},
	} {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	got := output.String()
	if !strings.Contains(got, `"partial_json":"{\"q\":1}"`) {
		t.Fatalf("dropped earlier tool delta after match: %s", got)
	}
	if strings.Contains(got, "dropped") {
		t.Fatalf("kept matching-block delta after match: %s", got)
	}
	if !strings.Contains(got, frame("content_block_stop", `{"type":"content_block_stop","index":0}`)) {
		t.Fatalf("missing earlier tool stop: %s", got)
	}
	if !strings.Contains(got, frame("content_block_stop", `{"type":"content_block_stop","index":1}`)) {
		t.Fatalf("missing matching text stop: %s", got)
	}
}

func encodeAndBuffer(t *testing.T, stops []string, events []reducer.Event, result reducer.Result) (string, reducer.Result, []byte) {
	t.Helper()
	var output bytes.Buffer
	encoder, err := New(&output, Options{MessageID: "m", Model: "gpt", StopSequences: stops})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatalf("Encode(%#v): %v", event, err)
		}
	}
	if encoder.MatchedStop() != "END" {
		t.Fatalf("MatchedStop = %q", encoder.MatchedStop())
	}
	truncated, _, err := ApplyStops(result, stops)
	if err != nil {
		t.Fatal(err)
	}
	body, err := MarshalBuffered("m", "gpt", truncated)
	if err != nil {
		t.Fatal(err)
	}
	return output.String(), truncated, body
}

func TestEncoderFlushesUnmatchedStopPrefixBeforeBlockStop(t *testing.T) {
	var output bytes.Buffer
	encoder, err := New(&output, Options{MessageID: "m", Model: "gpt", StopSequences: []string{"STOP"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []reducer.Event{
		{Kind: reducer.KindContentStart, Index: 0, Block: reducer.ContentBlock{Type: reducer.BlockText}},
		{Kind: reducer.KindContentDelta, Index: 0, Delta: reducer.Delta{Type: reducer.DeltaText, Text: "ends ST"}},
		{Kind: reducer.KindContentStop, Index: 0},
		{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{StopReason: reducer.StopEndTurn, Usage: reducer.Usage{InputTokens: 1, OutputTokens: 2}}},
	} {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if got := output.String(); !strings.Contains(got, `"text":"ends "`) || !strings.Contains(got, `"text":"ST"`) || encoder.MatchedStop() != "" {
		t.Fatalf("stream = %s", got)
	}
}

func TestEncoderPingEnsuresMessageStartFirst(t *testing.T) {
	var output bytes.Buffer
	encoder, err := New(&output, Options{MessageID: "m", Model: "gpt"})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Ping(); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Ping(); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	start := frame("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"gpt","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}`)
	ping := frame("ping", `{"type":"ping"}`)
	if got != start+ping+ping {
		t.Fatalf("Ping before start =\n got: %s\nwant: %s", got, start+ping+ping)
	}
}

func TestEncoderPingWriteFlushErrorsAndTerminalGuard(t *testing.T) {
	var output bytes.Buffer
	flushErr := errors.New("flush failed")
	encoder, err := New(&output, Options{MessageID: "m", Model: "gpt", Flush: func() error { return flushErr }})
	if err != nil {
		t.Fatal(err)
	}
	if err := encoder.Ping(); !errors.Is(err, flushErr) {
		t.Fatalf("Ping error = %v", err)
	}

	encoder, _ = New(failingWriter{}, Options{MessageID: "m", Model: "gpt"})
	if err := encoder.Ping(); err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("write error = %v", err)
	}

	encoder, _ = New(&output, Options{MessageID: "m2", Model: "gpt"})
	terminal := reducer.Event{Kind: reducer.KindTerminal, Terminal: &reducer.Terminal{StopReason: reducer.StopEndTurn, Usage: reducer.Usage{}}}
	if err := encoder.Encode(terminal); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Ping(); !errors.Is(err, ErrStreamFinished) {
		t.Fatalf("Ping after terminal = %v", err)
	}
	if err := encoder.Encode(terminal); !errors.Is(err, ErrStreamFinished) {
		t.Fatalf("second terminal = %v", err)
	}
}

func TestMarshalBufferedAndApplyStopsUseReducerResult(t *testing.T) {
	result := reducer.Result{
		Content: []reducer.ContentBlock{
			{Type: reducer.BlockThinking, Thinking: "plan", Signature: "sig"},
			{Type: reducer.BlockText, Text: "before END after"},
			{Type: reducer.BlockToolUse, ID: "late", Name: "late", Input: json.RawMessage(`{}`)},
		},
		StopReason: reducer.StopToolUse,
		Usage:      reducer.Usage{InputTokens: 8, OutputTokens: 5, CacheReadInputTokens: 2},
	}
	truncated, match, err := ApplyStops(result, []string{"END"})
	if err != nil {
		t.Fatal(err)
	}
	if match != "END" || truncated.StopReason != reducer.StopStopSequence || truncated.StopSequence != "END" || len(truncated.Content) != 2 || truncated.Content[1].Text != "before " {
		t.Fatalf("truncated = %#v, match=%q", truncated, match)
	}
	body, err := MarshalBuffered("msg_1", "gpt:model", truncated)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"msg_1","type":"message","role":"assistant","model":"gpt:model","content":[{"type":"thinking","thinking":"plan","signature":"sig"},{"type":"text","text":"before "}],"stop_reason":"stop_sequence","stop_sequence":"END","usage":{"input_tokens":8,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":2}}`
	if string(body) != want {
		t.Fatalf("body = %s, want %s", body, want)
	}
}

func frame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func assertFrameOrder(t *testing.T, stream string, events ...string) {
	t.Helper()
	position := -1
	for _, event := range events {
		next := strings.Index(stream[position+1:], "event: "+event+"\n")
		if next < 0 {
			t.Fatalf("event %q absent after byte %d: %s", event, position, stream)
		}
		position += next + 1
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }
