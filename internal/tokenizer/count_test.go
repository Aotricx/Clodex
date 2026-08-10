package tokenizer

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Aotricx/Clodex/internal/codexwire"
)

const (
	resizedImageTokens = 1844
	onePixelPNG        = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="
)

func TestCountRequestCapturedSemanticInputs(t *testing.T) {
	t.Parallel()
	counter := newTestCounter(t)
	encryptedThinking := strings.Repeat("x", 2000)

	tests := []struct {
		name string
		req  codexwire.Request
		want int
	}{
		{name: "empty request", req: codexwire.Request{}, want: 0},
		{name: "system instructions", req: codexwire.Request{Instructions: "Rule one.\n\nRule two."}, want: 6},
		{
			name: "text",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.Message{
				Role: "user", Content: []codexwire.ContentItem{codexwire.InputText{Text: "hello"}},
			}}},
			want: 22,
		},
		{
			name: "tool and schema",
			req: codexwire.Request{Tools: []codexwire.Tool{codexwire.FunctionTool{
				Name:        "lookup",
				Description: "Look up a language.",
				Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
			}}},
			want: 40,
		},
		{
			name: "tool use",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.FunctionCall{
				Name: "lookup", Arguments: `{"q":"go"}`, CallID: "call_1",
			}}},
			want: 26,
		},
		{
			name: "tool result",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.FunctionCallOutput{
				CallID: "call_1", Output: codexwire.FunctionOutputText("Go"),
			}}},
			want: 18,
		},
		{
			name: "visible thinking",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.ReasoningItem{
				Summary: []codexwire.ReasoningSummary{codexwire.SummaryText{Text: "visible"}},
			}}},
			want: 24,
		},
		{
			name: "encrypted thinking",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.ReasoningItem{
				Summary:          []codexwire.ReasoningSummary{codexwire.SummaryText{Text: "hidden by encrypted estimate"}},
				EncryptedContent: &encryptedThinking,
			}}},
			want: 213,
		},
		{
			name: "base64 image",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.Message{
				Role: "user", Content: []codexwire.ContentItem{codexwire.InputImage{
					ImageURL: "data:image/png;base64,AQID", Detail: codexwire.ImageDetailAuto,
				}},
			}}},
			want: 32 + resizedImageTokens,
		},
		{
			name: "tool result base64 image",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.FunctionCallOutput{
				CallID: "call_2",
				Output: codexwire.FunctionOutputContent{codexwire.InputImage{
					ImageURL: "data:image/jpeg;base64,AQID", Detail: codexwire.ImageDetailAuto,
				}},
			}}},
			want: 37 + resizedImageTokens,
		},
		{
			name: "URL image",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.Message{
				Role: "user", Content: []codexwire.ContentItem{codexwire.InputImage{
					ImageURL: "https://example.com/input.png", Detail: codexwire.ImageDetailAuto,
				}},
			}}},
			want: 32,
		},
		{
			name: "Unicode",
			req: codexwire.Request{Input: []codexwire.InputItem{codexwire.Message{
				Role: "user", Content: []codexwire.ContentItem{codexwire.InputText{Text: "你好, café 👋🏽 — مرحبا"}},
			}}},
			want: 31,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := counter.CountRequest(test.req)
			if err != nil {
				t.Fatalf("CountRequest() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("CountRequest() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestCountRequestImagePayloadIsNotTokenized(t *testing.T) {
	t.Parallel()
	counter := newTestCounter(t)

	countImage := func(t *testing.T, payload string, detail codexwire.ImageDetail) int {
		t.Helper()
		got, err := counter.CountRequest(codexwire.Request{Input: []codexwire.InputItem{codexwire.Message{
			Role: "user", Content: []codexwire.ContentItem{codexwire.InputImage{
				ImageURL: "data:image/png;base64," + payload, Detail: detail,
			}},
		}}})
		if err != nil {
			t.Fatalf("CountRequest() error = %v", err)
		}
		return got
	}

	if got := countImage(t, "AQID", codexwire.ImageDetailAuto); got != 32+resizedImageTokens {
		t.Fatalf("short payload count = %d, want %d", got, 32+resizedImageTokens)
	}
	if got := countImage(t, strings.Repeat("A", 1<<20), codexwire.ImageDetailAuto); got != 32+resizedImageTokens {
		t.Fatalf("large payload count = %d, want %d", got, 32+resizedImageTokens)
	}
	if got := countImage(t, onePixelPNG, codexwire.ImageDetailAuto); got != 33 {
		t.Fatalf("one-pixel auto image count = %d, want 33", got)
	}
	if got := countImage(t, onePixelPNG, codexwire.ImageDetailOriginal); got != 33 {
		t.Fatalf("one-pixel original image count = %d, want 33", got)
	}
}

func TestCountRequestCacheMarkersDoNotAddTokens(t *testing.T) {
	t.Parallel()
	counter := newTestCounter(t)
	request := codexwire.Request{
		Instructions:   "stable",
		Input:          []codexwire.InputItem{codexwire.Message{Role: "user", Content: []codexwire.ContentItem{codexwire.InputText{Text: "same"}}}},
		PromptCacheKey: "cache-control-fingerprint",
	}

	got, err := counter.CountRequest(request)
	if err != nil {
		t.Fatalf("CountRequest() error = %v", err)
	}
	request.PromptCacheKey = ""
	want, err := counter.CountRequest(request)
	if err != nil {
		t.Fatalf("CountRequest() without cache key error = %v", err)
	}
	if got != want || got != 23 {
		t.Fatalf("cache-key counts = %d/%d, want both 23", got, want)
	}
}

func TestCountRequestDeterministicConcurrent(t *testing.T) {
	t.Parallel()
	counter := newTestCounter(t)
	request := capturedRequest()
	const want = 2075

	for range 100 {
		got, err := counter.CountRequest(request)
		if err != nil || got != want {
			t.Fatalf("serial CountRequest() = %d, %v; want %d, nil", got, err, want)
		}
	}

	const workers = 16
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for worker := range workers {
		group.Go(func() {
			for iteration := range 100 {
				got, err := counter.CountRequest(request)
				if err != nil || got != want {
					errs <- fmt.Errorf("worker %d iteration %d: got %d, %v; want %d, nil", worker, iteration, got, err, want)
					return
				}
			}
		})
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestNewOfflineSubprocess(t *testing.T) {
	if os.Getenv("CLODEX_TOKENIZER_OFFLINE_HELPER") == "1" {
		counter, err := New()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		got, err := counter.CountRequest(codexwire.Request{Input: []codexwire.InputItem{codexwire.Message{
			Role: "user", Content: []codexwire.ContentItem{codexwire.InputText{Text: "hello"}},
		}}})
		if err != nil || got != 22 {
			fmt.Fprintf(os.Stderr, "CountRequest() = %d, %v; want 22, nil\n", got, err)
			os.Exit(3)
		}
		return
	}

	emptyHome := t.TempDir()
	command := exec.Command(os.Args[0], "-test.run=^TestNewOfflineSubprocess$")
	command.Env = []string{
		"CLODEX_TOKENIZER_OFFLINE_HELPER=1",
		"HOME=" + emptyHome,
		"XDG_CACHE_HOME=" + filepath.Join(emptyHome, "cache"),
		"HTTP_PROXY=http://127.0.0.1:1",
		"HTTPS_PROXY=http://127.0.0.1:1",
		"NO_PROXY=",
	}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("offline helper failed: %v\n%s", err, output)
	}
}

func BenchmarkCountRequest(b *testing.B) {
	counter := newTestCounter(b)
	request := capturedRequest()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := counter.CountRequest(request); err != nil {
			b.Fatal(err)
		}
	}
}

func newTestCounter(tb testing.TB) *Counter {
	tb.Helper()
	counter, err := New()
	if err != nil {
		tb.Fatalf("New() error = %v", err)
	}
	return counter
}

// capturedRequest uses semantic fragments pinned by translate's full-history
// golden: system, text, tool calls/results, thinking, images, and tool schema.
func capturedRequest() codexwire.Request {
	return codexwire.Request{
		Instructions: "Rule one.\n\nRule two.",
		Input: []codexwire.InputItem{
			codexwire.Message{Role: "user", Content: []codexwire.ContentItem{codexwire.InputText{Text: "hello"}}},
			codexwire.FunctionCall{Name: "lookup", Arguments: `{"q":"go"}`, CallID: "call_1"},
			codexwire.FunctionCallOutput{CallID: "call_1", Output: codexwire.FunctionOutputText("Go")},
			codexwire.ReasoningItem{Summary: []codexwire.ReasoningSummary{codexwire.SummaryText{Text: "visible"}}},
			codexwire.Message{Role: "user", Content: []codexwire.ContentItem{codexwire.InputImage{ImageURL: "data:image/png;base64,AQID", Detail: codexwire.ImageDetailAuto}}},
			codexwire.Message{Role: "user", Content: []codexwire.ContentItem{codexwire.InputImage{ImageURL: "https://example.com/input.png", Detail: codexwire.ImageDetailAuto}}},
			codexwire.Message{Role: "user", Content: []codexwire.ContentItem{codexwire.InputText{Text: "你好, café 👋🏽 — مرحبا"}}},
		},
		Tools: []codexwire.Tool{codexwire.FunctionTool{
			Name:        "lookup",
			Description: "Look up a language.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`),
		}},
		PromptCacheKey: "thread-1",
	}
}
