package claudee2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const e2eCheapModel = "gpt-5.6-luna:low"
const e2eXhighModel = "gpt-5.6-sol:xhigh"

func TestE2EUsesLiveCatalogModels(t *testing.T) {
	if e2eCheapModel != "gpt-5.6-luna:low" {
		t.Fatalf("e2eCheapModel=%q want gpt-5.6-luna:low (gpt-5.4-mini:low is absent from the live Codex catalog)", e2eCheapModel)
	}
	if e2eXhighModel != "gpt-5.6-sol:xhigh" {
		t.Fatalf("e2eXhighModel=%q want gpt-5.6-sol:xhigh", e2eXhighModel)
	}
}

type transcriptFacts struct {
	Result             string
	Success            bool
	NumTurns           int
	InputTokens        int
	OutputTokens       int
	PermissionDenials  int
	ThinkingBlocks     int
	ToolNames          []string
	MaxParallelTools   int
	CarrierContextSeen bool
}

func TestClaudeCodeE2E(t *testing.T) {
	if os.Getenv("CLODEX_E2E_TESTS") == "" {
		t.Skip("set CLODEX_E2E_TESTS=1 with binary, port, and image variables to run Claude Code e2e")
	}
	binary := os.Getenv("CLODEX_E2E_BINARY")
	if binary == "" {
		t.Fatal("CLODEX_E2E_BINARY is required")
	}
	port, err := strconv.Atoi(os.Getenv("CLODEX_E2E_PORT"))
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("CLODEX_E2E_PORT is invalid: %q", os.Getenv("CLODEX_E2E_PORT"))
	}
	imagePath := os.Getenv("CLODEX_E2E_IMAGE")
	if imagePath == "" {
		t.Fatal("CLODEX_E2E_IMAGE is required")
	}

	t.Run("luna image tool round trip", func(t *testing.T) {
		facts := runClodexClaude(t, binary, port, e2eCheapModel, "", []string{
			"--print", "Use the Read tool on " + imagePath + ". Then identify the dog breed. You must inspect the image; answer with breed and confidence.",
			"--output-format", "stream-json", "--verbose", "--include-partial-messages",
			"--tools", "Read", "--add-dir", filepath.Dir(imagePath), "--dangerously-skip-permissions", "--max-budget-usd", "2",
		})
		if !facts.Success || !strings.Contains(strings.ToLower(facts.Result), "great dane") || !contains(facts.ToolNames, "Read") || facts.NumTurns < 2 {
			t.Fatalf("luna facts = %#v", facts)
		}
		assertSaneUsage(t, facts)
		t.Logf("luna result=%q turns=%d tools=%v usage=%d/%d carrier_context=%v", facts.Result, facts.NumTurns, facts.ToolNames, facts.InputTokens, facts.OutputTokens, facts.CarrierContextSeen)
	})

	t.Run("xhigh multi-turn tools and thinking", func(t *testing.T) {
		dir := t.TempDir()
		first := filepath.Join(dir, "alpha.txt")
		second := filepath.Join(dir, "beta.txt")
		if err := os.WriteFile(first, []byte("ALPHA_MARKER\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(second, []byte("BETA_MARKER\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		prompt := fmt.Sprintf("Use Read on both %s and %s. After both results arrive, reply ALPHA_MARKER+BETA_MARKER.", first, second)
		// The task outcome is deterministic and is asserted on every attempt.
		// Whether the backend emits a reasoning summary for a given turn is not:
		// Codex may complete this turn with no summary at all. So thinking is
		// required to appear at least once across attempts rather than every
		// run, which keeps the assertion meaningful without being flaky.
		const attempts = 3
		thinkingSeen := false
		for attempt := 1; attempt <= attempts; attempt++ {
			facts := runClodexClaude(t, binary, port, e2eXhighModel, dir, []string{
				"--print", prompt, "--output-format", "stream-json", "--verbose", "--include-partial-messages",
				"--tools", "Read", "--dangerously-skip-permissions", "--bare",
				"--system-prompt", "Follow the task exactly. Use available tools. Think carefully.", "--max-budget-usd", "2",
			})
			if !facts.Success || !strings.Contains(facts.Result, "ALPHA_MARKER+BETA_MARKER") || len(facts.ToolNames) < 2 || facts.NumTurns < 2 {
				t.Fatalf("xhigh attempt %d facts = %#v", attempt, facts)
			}
			assertSaneUsage(t, facts)
			t.Logf("xhigh attempt=%d result=%q turns=%d tools=%v thinking=%d usage=%d/%d carrier_context=%v", attempt, facts.Result, facts.NumTurns, facts.ToolNames, facts.ThinkingBlocks, facts.InputTokens, facts.OutputTokens, facts.CarrierContextSeen)
			if facts.ThinkingBlocks > 0 {
				thinkingSeen = true
				break
			}
		}
		if !thinkingSeen {
			t.Fatalf("no thinking block in %d attempts: Codex never emitted a reasoning summary", attempts)
		}
	})
}

func runClodexClaude(t testing.TB, binary string, port int, model, workdir string, claudeArgs []string) transcriptFacts {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	args := []string{"claude", "--model", model, "--"}
	args = append(args, claudeArgs...)
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = replaceEnv(os.Environ(), map[string]string{
		"CLODEX_PORT":  strconv.Itoa(port),
		"CLODEX_MODEL": model,
	})
	if workdir != "" {
		command.Dir = workdir
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("Claude Code e2e timed out: %v", ctx.Err())
	}
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			t.Fatalf("Claude Code e2e exit %d: stdout=%s stderr=%s", exitError.ExitCode(), stdout.Bytes(), stderr.Bytes())
		}
		t.Fatalf("Claude Code e2e: %v: stdout=%s stderr=%s", err, stdout.Bytes(), stderr.Bytes())
	}
	output := stdout.Bytes()
	facts, err := parseTranscript(output)
	if err != nil {
		t.Fatalf("parse Claude transcript: %v", err)
	}
	if dir := os.Getenv("CLODEX_E2E_TRANSCRIPT_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, strings.ReplaceAll(model, ":", "-")+".jsonl")
		if err := os.WriteFile(path, output, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return facts
}

func parseTranscript(output []byte) (transcriptFacts, error) {
	var facts transcriptFacts
	activeTools := 0
	for lineNumber, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			return facts, fmt.Errorf("line %d: %w", lineNumber+1, err)
		}
		switch event["type"] {
		case "stream_event":
			stream, _ := event["event"].(map[string]any)
			switch stream["type"] {
			case "message_start":
				activeTools = 0
			case "content_block_start":
				block, _ := stream["content_block"].(map[string]any)
				switch block["type"] {
				case "thinking":
					facts.ThinkingBlocks++
				case "tool_use":
					activeTools++
					facts.MaxParallelTools = max(facts.MaxParallelTools, activeTools)
					if name, ok := block["name"].(string); ok {
						facts.ToolNames = append(facts.ToolNames, name)
					}
				}
			case "message_stop":
				activeTools = 0
			}
		case "result":
			facts.Success = event["subtype"] == "success" && event["is_error"] == false
			facts.Result, _ = event["result"].(string)
			facts.NumTurns = integer(event["num_turns"])
			facts.PermissionDenials = len(array(event["permission_denials"]))
			usage, _ := event["usage"].(map[string]any)
			facts.InputTokens = integer(usage["input_tokens"])
			facts.OutputTokens = integer(usage["output_tokens"])
			modelUsage, _ := event["modelUsage"].(map[string]any)
			for id, raw := range modelUsage {
				entry, _ := raw.(map[string]any)
				if (strings.HasPrefix(id, "anthropic-clodex-") || strings.HasSuffix(id, "[1m]")) && integer(entry["contextWindow"]) == 1_000_000 {
					facts.CarrierContextSeen = true
				}
			}
		}
	}
	if !facts.Success && facts.Result == "" {
		return facts, errors.New("successful result event missing")
	}
	return facts, nil
}

func assertSaneUsage(t testing.TB, facts transcriptFacts) {
	t.Helper()
	if facts.InputTokens <= 0 || facts.OutputTokens <= 0 || facts.PermissionDenials != 0 || !facts.CarrierContextSeen {
		t.Fatalf("usage/context facts = %#v", facts)
	}
}

func replaceEnv(environment []string, replacements map[string]string) []string {
	result := make([]string, 0, len(environment)+len(replacements))
	for _, entry := range environment {
		key, _, ok := strings.Cut(entry, "=")
		if _, replaced := replacements[key]; ok && replaced {
			continue
		}
		result = append(result, entry)
	}
	for key, value := range replacements {
		result = append(result, key+"="+value)
	}
	return result
}

func integer(value any) int {
	number, _ := value.(float64)
	return int(number)
}

func array(value any) []any {
	result, _ := value.([]any)
	return result
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestClaudeCodeAstraE2E(t *testing.T) {
	if os.Getenv("CLODEX_E2E_TESTS") == "" {
		t.Skip("set CLODEX_E2E_TESTS=1 with CLODEX_E2E_BINARY and CLODEX_E2E_PORT to test Astra")
	}
	binary := os.Getenv("CLODEX_E2E_BINARY")
	if binary == "" {
		t.Fatal("CLODEX_E2E_BINARY is required")
	}
	port, err := strconv.Atoi(os.Getenv("CLODEX_E2E_PORT"))
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("CLODEX_E2E_PORT must be a valid port")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "marker.txt")
	const marker = "CLODEX_ASTRA_TOOL_ROUND_TRIP_OK"
	if err := os.WriteFile(path, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	facts := runClodexClaude(t, binary, port, "gpt-6-astra:medium", dir, []string{
		"--print", "Use Read to read " + path + ". Reply with exactly its contents.",
		"--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--tools", "Read", "--allowedTools", "Read", "--strict-mcp-config", "--setting-sources", "",
		"--no-session-persistence", "--system-prompt", "Follow the task exactly. Use the Read tool.",
	})
	if !facts.Success || strings.TrimSpace(facts.Result) != marker || !contains(facts.ToolNames, "Read") || facts.NumTurns < 2 || facts.PermissionDenials != 0 {
		t.Fatalf("Astra facts = %#v", facts)
	}
	assertSaneUsage(t, facts)
	t.Logf("Astra result=%q turns=%d tools=%v usage=%d/%d", facts.Result, facts.NumTurns, facts.ToolNames, facts.InputTokens, facts.OutputTokens)
}
