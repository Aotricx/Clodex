package fixtures

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestLoadTestdata(t *testing.T) {
	manifest, err := Load("testdata")
	if err != nil {
		t.Fatalf("Load(testdata) error = %v", err)
	}
	if manifest.Version != 1 {
		t.Fatalf("Version = %d, want 1", manifest.Version)
	}
	if len(manifest.Fixtures) != 9 {
		t.Errorf("len(Fixtures) = %d, want 9", len(manifest.Fixtures))
	}

	wantEvents := map[string][]string{
		"real_text_http":                     {"response.output_item.added", "response.output_item.done", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta", "response.output_text.delta", "response.output_text.delta", "response.output_text.delta", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"},
		"real_tool_roundtrip":                {"response.output_item.added", "response.output_item.done", "response.output_item.added", "response.function_call_arguments.done", "response.output_item.done", "response.completed", "response.output_item.added", "response.output_text.done", "response.output_item.done", "response.completed"},
		"real_parallel_function_calls":       {"response.output_item.added", "response.reasoning_summary_text.done", "response.output_item.done", "response.output_item.added", "response.function_call_arguments.done", "response.output_item.done", "response.output_item.added", "response.function_call_arguments.done", "response.output_item.done", "response.completed"},
		"real_web_search":                    {"response.output_item.added", "response.output_item.done", "response.output_item.added", "response.web_search_call.searching", "response.output_item.done", "response.output_item.added", "response.reasoning_summary_text.done", "response.output_item.done", "response.output_item.added", "response.output_text.done", "response.output_item.done", "response.completed"},
		"real_dog_image":                     {"response.output_item.added", "response.reasoning_summary_text.done", "response.output_item.done", "response.output_item.added", "response.output_text.done", "response.output_item.done", "response.completed"},
		"regression_credited_rate_limit":     {"codex.rate_limits", "response.output_item.added", "response.output_text.delta", "response.output_item.done", "response.completed"},
		"regression_terminal_only_completed": {"response.completed"},
		"regression_terminal_only_done":      {"response.done"},
		"regression_incomplete":              {"response.incomplete"},
	}
	seen := make(map[string]bool, len(manifest.Fixtures))
	for _, fixture := range manifest.Fixtures {
		want, ok := wantEvents[fixture.Name]
		if !ok {
			t.Errorf("unexpected fixture %q", fixture.Name)
			continue
		}
		seen[fixture.Name] = true
		if strings.TrimSpace(fixture.Provenance) == "" {
			t.Errorf("fixture %q has empty provenance", fixture.Name)
		}
		if len(fixture.Events) != len(want) {
			t.Errorf("fixture %q has %d events, want %d", fixture.Name, len(fixture.Events), len(want))
			continue
		}
		for i := range want {
			if fixture.Events[i].Type != want[i] {
				t.Errorf("fixture %q event %d = %q, want %q", fixture.Name, i, fixture.Events[i].Type, want[i])
			}
		}
	}
	for name := range wantEvents {
		if !seen[name] {
			t.Errorf("missing fixture %q", name)
		}
	}
}

func TestRealCaptureSemantics(t *testing.T) {
	manifest, err := Load("testdata")
	if err != nil {
		t.Fatalf("Load(testdata) error = %v", err)
	}
	fixturesByName := make(map[string]Fixture, len(manifest.Fixtures))
	for _, fixture := range manifest.Fixtures {
		fixturesByName[fixture.Name] = fixture
	}

	t.Run("tool roundtrip", func(t *testing.T) {
		fixture := requireFixture(t, fixturesByName, "real_tool_roundtrip")
		reasoning := decodeCaptureEvent(t, fixture.Events[1])
		if reasoning.Item.Type != "reasoning" || reasoning.Item.EncryptedContent != "[REDACTED]" {
			t.Fatalf("reasoning item = %#v, want redacted encrypted reasoning", reasoning.Item)
		}
		call := decodeCaptureEvent(t, fixture.Events[4])
		if call.Item.Name != "exec_command" || call.Item.CallID != "call_J9SBcdmlpAKgDYST6TYxKlZu" {
			t.Fatalf("function call = %#v, want captured exec_command call", call.Item)
		}
		wantArguments := `{"cmd":"pwd","login":true,"max_output_tokens":2000,"workdir":"[REDACTED_PATH]","yield_time_ms":1000}`
		if call.Item.Arguments != wantArguments {
			t.Fatalf("function arguments = %q, want %q", call.Item.Arguments, wantArguments)
		}
		message := decodeCaptureEvent(t, fixture.Events[8])
		if message.Item.Type != "message" || len(message.Item.Content) != 1 || message.Item.Content[0].Text != "[REDACTED_PATH]" {
			t.Fatalf("final message = %#v, want redacted path response", message.Item)
		}
		completed := decodeCaptureEvent(t, fixture.Events[9])
		if completed.Response.Usage.TotalTokens != 17312 {
			t.Fatalf("final usage total_tokens = %d, want 17312", completed.Response.Usage.TotalTokens)
		}
	})

	t.Run("parallel function calls", func(t *testing.T) {
		fixture := requireFixture(t, fixturesByName, "real_parallel_function_calls")
		first := decodeCaptureEvent(t, fixture.Events[5])
		second := decodeCaptureEvent(t, fixture.Events[8])
		if first.Item.Name != "lookup" || first.Item.CallID != "call_pPjUcB7YQAzjsrCqZMNFSb76" || first.Item.Arguments != `{"label":"FIRST"}` {
			t.Fatalf("first function call = %#v, want captured FIRST lookup", first.Item)
		}
		if second.Item.Name != "lookup" || second.Item.CallID != "call_9TUpiLhd0rskL8wXXbIA56Di" || second.Item.Arguments != `{"label":"SECOND"}` {
			t.Fatalf("second function call = %#v, want captured SECOND lookup", second.Item)
		}
		completed := decodeCaptureEvent(t, fixture.Events[9])
		if completed.Response.Usage.TotalTokens != 152 {
			t.Fatalf("usage total_tokens = %d, want 152", completed.Response.Usage.TotalTokens)
		}
	})

	t.Run("web search", func(t *testing.T) {
		fixture := requireFixture(t, fixturesByName, "real_web_search")
		search := decodeCaptureEvent(t, fixture.Events[4])
		if search.Item.Type != "web_search_call" || search.Item.Action.Type != "search" {
			t.Fatalf("web search item = %#v, want completed search action", search.Item)
		}
		if search.Item.Action.Query != "site:go.dev/dl Go release stable newest go.dev today" {
			t.Fatalf("web search query = %q", search.Item.Action.Query)
		}
		message := decodeCaptureEvent(t, fixture.Events[10])
		if len(message.Item.Content) != 1 || !strings.Contains(message.Item.Content[0].Text, "Go 1.26.5") {
			t.Fatalf("web search message = %#v, want captured release answer", message.Item)
		}
	})

	t.Run("dog image", func(t *testing.T) {
		fixture := requireFixture(t, fixturesByName, "real_dog_image")
		reasoning := decodeCaptureEvent(t, fixture.Events[2])
		if reasoning.Item.EncryptedContent != "[REDACTED]" {
			t.Fatalf("reasoning encrypted_content = %q, want [REDACTED]", reasoning.Item.EncryptedContent)
		}
		message := decodeCaptureEvent(t, fixture.Events[5])
		if len(message.Item.Content) != 1 || message.Item.Content[0].Text != "This is a Great Dane dog." {
			t.Fatalf("image response = %#v, want Great Dane identification", message.Item)
		}
		completed := decodeCaptureEvent(t, fixture.Events[6])
		if completed.Response.Usage.TotalTokens != 356 {
			t.Fatalf("usage total_tokens = %d, want 356", completed.Response.Usage.TotalTokens)
		}
	})
}

type captureEventData struct {
	Text      string `json:"text"`
	Arguments string `json:"arguments"`
	Item      struct {
		Type             string `json:"type"`
		Name             string `json:"name"`
		CallID           string `json:"call_id"`
		Arguments        string `json:"arguments"`
		EncryptedContent string `json:"encrypted_content"`
		Action           struct {
			Type  string `json:"type"`
			Query string `json:"query"`
		} `json:"action"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
	Response struct {
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

func requireFixture(t *testing.T, fixtures map[string]Fixture, name string) Fixture {
	t.Helper()
	fixture, ok := fixtures[name]
	if !ok {
		t.Fatalf("missing fixture %q", name)
	}
	return fixture
}

func decodeCaptureEvent(t *testing.T, event Event) captureEventData {
	t.Helper()
	var data captureEventData
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatalf("decode %q event: %v", event.Type, err)
	}
	return data
}

func TestLoadRejectsInvalidManifestOrFixture(t *testing.T) {
	tests := []struct {
		name       string
		version    int
		kind       string
		file       string
		provenance string
		fixture    string
		wantError  string
		skipFile   bool
	}{
		{
			name:       "bad version",
			version:    2,
			kind:       "regression",
			file:       "fixture.sse",
			provenance: "test source",
			fixture:    validSSE,
			wantError:  "version",
		},
		{
			name:       "empty provenance",
			version:    1,
			kind:       "regression",
			file:       "fixture.sse",
			provenance: "  ",
			fixture:    validSSE,
			wantError:  "provenance",
		},
		{
			name:       "traversal path",
			version:    1,
			kind:       "regression",
			file:       "../fixture.sse",
			provenance: "test source",
			fixture:    validSSE,
			wantError:  "safe relative path",
			skipFile:   true,
		},
		{
			name:       "malformed SSE framing",
			version:    1,
			kind:       "regression",
			file:       "fixture.sse",
			provenance: "test source",
			fixture:    "event: response.completed\ndata: {}\nevent: response.done\ndata: {}\n\n",
			wantError:  "framing",
		},
		{
			name:       "invalid JSON",
			version:    1,
			kind:       "regression",
			file:       "fixture.sse",
			provenance: "test source",
			fixture:    "event: response.completed\ndata: {invalid}\n\n",
			wantError:  "JSON",
		},
		{
			name:       "missing file",
			version:    1,
			kind:       "regression",
			file:       "missing.sse",
			provenance: "test source",
			fixture:    validSSE,
			wantError:  "read",
			skipFile:   true,
		},
		{
			name:       "unknown kind",
			version:    1,
			kind:       "invented",
			file:       "fixture.sse",
			provenance: "test source",
			fixture:    validSSE,
			wantError:  "kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeFixtureRoot(t, tt.version, tt.kind, tt.file, tt.provenance, tt.fixture, tt.skipFile)
			_, err := Load(root)
			if err == nil {
				t.Fatal("Load() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("Load() error = %q, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestLoadRejectsSecrets(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "API key", payload: `{"key":"sk-proj-abcdefghijklmnopqrstuvwxyz123456"}`},
		{name: "JWT", payload: `{"token":"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signature"}`},
		{name: "refresh token", payload: `{"refresh_token":"live-refresh-token"}`},
		{name: "account", payload: `{"account_id":"account-123"}`},
		{name: "email", payload: `{"email":"person@example.com"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := "event: response.completed\ndata: " + tt.payload + "\n\n"
			root := writeFixtureRoot(t, 1, "regression", "fixture.sse", "test source", fixture, false)
			_, err := Load(root)
			if err == nil {
				t.Fatal("Load() error = nil, want secret rejection")
			}
			if !strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("Load() error = %q, want substring %q", err, "sensitive")
			}
		})
	}
}

const validSSE = "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n"

func writeFixtureRoot(t *testing.T, version int, kind, file, provenance, fixture string, skipFile bool) string {
	t.Helper()
	root := t.TempDir()
	manifest := `{"version":` + strconv.Itoa(version) + `,"fixtures":[{"name":"case","kind":"` + kind + `","file":"` + file + `","provenance":"` + provenance + `"}]}`
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if !skipFile {
		if err := os.WriteFile(filepath.Join(root, file), []byte(fixture), 0o600); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}
	return root
}
