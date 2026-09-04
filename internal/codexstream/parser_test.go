package codexstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Aotricx/Clodex/internal/fixtures"
)

func TestParserHandlesSSEFramingAndFields(t *testing.T) {
	input := strings.Join([]string{
		": upstream heartbeat\r\n",
		"id: turn-7\r\n",
		"retry: 1500\r\n",
		"event: response.output_text.delta\r\n",
		"data: {\"type\":\"response.output_text.delta\",\r\n",
		"data: \"delta\":\"hello\"}\r\n",
		"\r\n",
		"event: response.future.event\n",
		"data: {\"type\":\"response.future.event\",\"value\":1}\n",
		"\n",
		"event: response.completed\n",
		"data: {\"type\":\"response.completed\",\"response\":{}}\n",
		"\n",
	}, "")

	p := New(context.Background(), strings.NewReader(input), Options{})
	first, err := p.Next()
	if err != nil {
		t.Fatalf("Next first: %v", err)
	}
	if first.Type != TypeOutputTextDelta || first.ID != "turn-7" || first.Retry != 1500*time.Millisecond {
		t.Fatalf("first = %#v", first)
	}
	if got, want := string(first.Raw), "{\"type\":\"response.output_text.delta\",\n\"delta\":\"hello\"}"; got != want {
		t.Fatalf("raw = %q, want %q", got, want)
	}
	var decoded struct {
		Delta string `json:"delta"`
	}
	if err := first.Decode(&decoded); err != nil || decoded.Delta != "hello" {
		t.Fatalf("Decode = %#v, %v", decoded, err)
	}
	if !first.Known() || first.Terminal() {
		t.Fatalf("first classification known=%v terminal=%v", first.Known(), first.Terminal())
	}

	unknown, err := p.Next()
	if err != nil {
		t.Fatalf("Next unknown: %v", err)
	}
	if unknown.Type != "response.future.event" || unknown.Known() || unknown.ID != "turn-7" {
		t.Fatalf("unknown = %#v", unknown)
	}

	terminal, err := p.Next()
	if err != nil {
		t.Fatalf("Next terminal: %v", err)
	}
	if terminal.Type != TypeResponseCompleted || !terminal.Terminal() || !p.TerminalSeen() {
		t.Fatalf("terminal = %#v, seen=%v", terminal, p.TerminalSeen())
	}
	if _, err := p.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next EOF = %v, want io.EOF", err)
	}
}

func TestParserInfersTypeFromDataAndIgnoresDataLessFrames(t *testing.T) {
	input := ": comment\n\nevent: ping\n\ndata: {\"type\":\"response.created\",\"response\":{}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n"
	p := New(context.Background(), strings.NewReader(input), Options{})

	event, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != TypeResponseCreated || event.Event != "" {
		t.Fatalf("event = %#v", event)
	}
	if _, err := p.Next(); err != nil {
		t.Fatal(err)
	}
}

func TestParserHandlesArbitrarilyFragmentedReads(t *testing.T) {
	input := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	for size := 1; size <= 11; size++ {
		t.Run(string(rune('a'+size-1)), func(t *testing.T) {
			p := New(context.Background(), &fragmentReader{data: input, max: size}, Options{})
			first, err := p.Next()
			if err != nil || first.Type != TypeOutputTextDelta {
				t.Fatalf("first = %#v, %v", first, err)
			}
			second, err := p.Next()
			if err != nil || second.Type != TypeResponseCompleted {
				t.Fatalf("second = %#v, %v", second, err)
			}
		})
	}
}

func TestParserHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := New(ctx, strings.NewReader("event: response.completed\ndata: {}\n\n"), Options{})
	if _, err := p.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next = %v, want context.Canceled", err)
	}
}

func TestParserTreatsTrailingCommentAfterTerminalAsEOF(t *testing.T) {
	input := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n: keepalive\n"
	p := New(context.Background(), strings.NewReader(input), Options{})
	event, err := p.Next()
	if err != nil || event.Type != TypeResponseCompleted {
		t.Fatalf("terminal = %#v, %v", event, err)
	}
	if _, err := p.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("Next after trailing comment = %v, want io.EOF", err)
	}
}

func TestParserDistinguishesTruncatedAndTerminalLessEOF(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "truncated frame", input: "event: response.output_text.delta\ndata: {}", want: ErrTruncatedFrame},
		{name: "no terminal", input: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n", want: ErrEOFWithoutTerminal},
		{name: "empty", input: "", want: ErrEOFWithoutTerminal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(context.Background(), strings.NewReader(tt.input), Options{})
			for {
				_, err := p.Next()
				if err != nil {
					if !errors.Is(err, tt.want) {
						t.Fatalf("Next = %v, want %v", err, tt.want)
					}
					return
				}
			}
		})
	}
}

func TestParserRejectsMalformedEvents(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  error
	}{
		{name: "invalid JSON", input: []byte("event: response.created\ndata: {nope}\n\n"), want: ErrMalformedEvent},
		{name: "invalid UTF-8", input: []byte{'d', 'a', 't', 'a', ':', ' ', 0xff, '\n', '\n'}, want: ErrMalformedEvent},
		{name: "missing type", input: []byte("data: {\"value\":1}\n\n"), want: ErrMalformedEvent},
		{name: "null payload", input: []byte("event: ping\ndata: null\n\n"), want: ErrMalformedEvent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := New(context.Background(), bytes.NewReader(tt.input), Options{})
			if _, err := p.Next(); !errors.Is(err, tt.want) {
				t.Fatalf("Next = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestParserPreservesOuterEventWithoutRejectingGenericEventNames(t *testing.T) {
	input := "event: message\ndata: {\"type\":\"response.future.event\",\"value\":1}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"
	p := New(context.Background(), strings.NewReader(input), Options{})
	event, err := p.Next()
	if err != nil {
		t.Fatal(err)
	}
	if event.Event != "message" || event.Type != "response.future.event" || event.Known() {
		t.Fatalf("event = %#v", event)
	}
}

func TestParserAcceptsLargeEventsAndRejectsConfiguredOversize(t *testing.T) {
	delta := strings.Repeat("x", 2<<20)
	large := "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"" + delta + "\"}\n\n"
	p := New(context.Background(), strings.NewReader(large), Options{})
	event, err := p.Next()
	if err != nil {
		t.Fatalf("large Next: %v", err)
	}
	args, err := event.DecodeFunctionArguments()
	if err != nil || args.Delta != delta {
		t.Fatalf("large args len=%d err=%v", len(args.Delta), err)
	}

	p = New(context.Background(), strings.NewReader(large), Options{MaxEventBytes: 1024})
	if _, err := p.Next(); !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("oversize Next = %v, want ErrEventTooLarge", err)
	}
}

func TestParserReadsEveryCommittedGoldenFixture(t *testing.T) {
	root := filepath.Join("..", "fixtures", "testdata")
	manifest, err := fixtures.Load(root)
	if err != nil {
		t.Fatalf("Load fixtures: %v", err)
	}
	for _, fixture := range manifest.Fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			p := New(context.Background(), bytes.NewReader(data), Options{})
			var got []Event
			for {
				event, err := p.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				got = append(got, event)
			}
			if len(got) != len(fixture.Events) {
				t.Fatalf("events = %d, want %d", len(got), len(fixture.Events))
			}
			for i := range got {
				if got[i].Type != fixture.Events[i].Type || !bytes.Equal(got[i].Raw, fixture.Events[i].Data) {
					t.Fatalf("event %d = %q %s, want %q %s", i, got[i].Type, got[i].Raw, fixture.Events[i].Type, fixture.Events[i].Data)
				}
				if !got[i].Known() {
					t.Fatalf("fixture event %d type %q is not pinned", i, got[i].Type)
				}
			}
		})
	}
}

func TestTypedDecodeHelpersCoverPinnedEventFamilies(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, event Event)
	}{
		{
			name:  "response",
			input: `{"type":"response.in_progress","sequence_number":1,"response":{"id":"r1"}}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeResponse()
				if err != nil || got.SequenceNumber == nil || *got.SequenceNumber != 1 || string(got.Response) != `{"id":"r1"}` {
					t.Fatalf("DecodeResponse = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "output item",
			input: `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call"}}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeOutputItem()
				if err != nil || got.OutputIndex == nil || *got.OutputIndex != 2 || string(got.Item) != `{"type":"function_call"}` {
					t.Fatalf("DecodeOutputItem = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "text",
			input: `{"type":"response.output_text.delta","item_id":"i1","output_index":0,"content_index":1,"delta":"hi"}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeText()
				if err != nil || got.Delta != "hi" || got.ItemID != "i1" {
					t.Fatalf("DecodeText = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "reasoning",
			input: `{"type":"response.reasoning_summary_text.done","item_id":"i1","summary_index":3,"text":"summary"}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeReasoning()
				if err != nil || got.Text != "summary" || got.SummaryIndex == nil || *got.SummaryIndex != 3 {
					t.Fatalf("DecodeReasoning = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "function args",
			input: `{"type":"response.function_call_arguments.done","call_id":"c1","arguments":"{}"}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeFunctionArguments()
				if err != nil || got.CallID != "c1" || got.Arguments != "{}" {
					t.Fatalf("DecodeFunctionArguments = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "content part",
			input: `{"type":"response.content_part.added","content_index":1,"part":{"type":"output_text"}}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeContentPart()
				if err != nil || got.ContentIndex == nil || *got.ContentIndex != 1 || string(got.Part) != `{"type":"output_text"}` {
					t.Fatalf("DecodeContentPart = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "rate limits",
			input: `{"type":"codex.rate_limits","rate_limits":{"limit_reached":true},"credits":{"has_credits":true}}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeRateLimits()
				if err != nil || string(got.RateLimits) != `{"limit_reached":true}` || string(got.Credits) != `{"has_credits":true}` {
					t.Fatalf("DecodeRateLimits = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "refusal delta",
			input: `{"type":"response.refusal.delta","item_id":"i1","output_index":0,"content_index":0,"delta":"cannot"}`,
			check: func(t *testing.T, event Event) {
				if !event.Known() {
					t.Fatal("response.refusal.delta should be a known event type")
				}
				got, err := event.DecodeText()
				if err != nil || got.Delta != "cannot" || got.ItemID != "i1" {
					t.Fatalf("DecodeText refusal delta = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "refusal done",
			input: `{"type":"response.refusal.done","item_id":"i1","output_index":0,"content_index":0,"text":"cannot"}`,
			check: func(t *testing.T, event Event) {
				if !event.Known() {
					t.Fatal("response.refusal.done should be a known event type")
				}
				got, err := event.DecodeText()
				if err != nil || got.Text != "cannot" || got.ItemID != "i1" {
					t.Fatalf("DecodeText refusal done = %#v, %v", got, err)
				}
			},
		},
		{
			name:  "web search",
			input: `{"type":"response.web_search_call.searching","item_id":"w1","output_index":4}`,
			check: func(t *testing.T, event Event) {
				got, err := event.DecodeWebSearch()
				if err != nil || got.ItemID != "w1" || got.OutputIndex == nil || *got.OutputIndex != 4 {
					t.Fatalf("DecodeWebSearch = %#v, %v", got, err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var envelope struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(tt.input), &envelope); err != nil {
				t.Fatal(err)
			}
			tt.check(t, Event{Type: envelope.Type, Raw: json.RawMessage(tt.input)})
		})
	}

	wrong := Event{Type: TypeResponseCompleted, Raw: json.RawMessage(`{"type":"response.completed"}`)}
	if _, err := wrong.DecodeText(); !errors.Is(err, ErrUnexpectedEventType) {
		t.Fatalf("wrong DecodeText = %v, want ErrUnexpectedEventType", err)
	}
}

func TestParserCancellationWinsWhenReadCancelsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &cancelingReader{cancel: cancel, data: []byte("event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")}
	p := New(ctx, r, Options{})
	if _, err := p.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next = %v, want context.Canceled", err)
	}
}

type fragmentReader struct {
	data []byte
	max  int
}

type cancelingReader struct {
	cancel context.CancelFunc
	data   []byte
	done   bool
}

func (r *cancelingReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	r.cancel()
	return copy(p, r.data), nil
}

func (r *fragmentReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.max, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}
