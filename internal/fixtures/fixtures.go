// Package fixtures loads and validates protocol fixtures used by Clodex tests.
package fixtures

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const supportedVersion = 1

var supportedKinds = map[string]struct{}{
	"capture":    {},
	"regression": {},
}

var secretPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{name: "API key", pattern: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`)},
	{name: "JWT", pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]+`)},
	{name: "bearer credential", pattern: regexp.MustCompile(`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=-]{8,}`)},
	{name: "email", pattern: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
}

var sensitiveKeys = map[string]struct{}{
	"account":          {},
	"accountid":        {},
	"accesstoken":      {},
	"apikey":           {},
	"auth":             {},
	"authorization":    {},
	"credential":       {},
	"credentials":      {},
	"email":            {},
	"encryptedcontent": {},
	"password":         {},
	"refreshtoken":     {},
}

// Manifest describes a fixture set and contains each fixture's parsed events.
type Manifest struct {
	Version  int       `json:"version"`
	Fixtures []Fixture `json:"fixtures"`
}

// Fixture describes one source-backed SSE fixture.
type Fixture struct {
	Name       string  `json:"name"`
	Kind       string  `json:"kind"`
	File       string  `json:"file"`
	Provenance string  `json:"provenance"`
	Events     []Event `json:"-"`
}

// Event is one event/data pair from an SSE fixture.
type Event struct {
	Type string
	Data json.RawMessage
}

// Load reads manifest.json and validates every referenced SSE fixture beneath root.
func Load(root string) (Manifest, error) {
	var manifest Manifest

	rootPath, err := filepath.Abs(root)
	if err != nil {
		return manifest, fmt.Errorf("resolve fixture root: %w", err)
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return manifest, fmt.Errorf("resolve fixture root: %w", err)
	}

	manifestData, err := os.ReadFile(filepath.Join(rootPath, "manifest.json"))
	if err != nil {
		return manifest, fmt.Errorf("read fixture manifest: %w", err)
	}
	if err := rejectStringSecrets(manifestData); err != nil {
		return manifest, fmt.Errorf("fixture manifest contains sensitive data: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, fmt.Errorf("decode fixture manifest: %w", err)
	}
	if decoder.More() {
		return manifest, fmt.Errorf("decode fixture manifest: trailing JSON value")
	}
	if manifest.Version != supportedVersion {
		return manifest, fmt.Errorf("unsupported fixture version %d; want %d", manifest.Version, supportedVersion)
	}

	for i := range manifest.Fixtures {
		fixture := &manifest.Fixtures[i]
		if strings.TrimSpace(fixture.Provenance) == "" {
			return manifest, fmt.Errorf("fixture %q has empty provenance", fixture.Name)
		}
		if _, ok := supportedKinds[fixture.Kind]; !ok {
			return manifest, fmt.Errorf("fixture %q has unrecognized kind %q", fixture.Name, fixture.Kind)
		}
		fixturePath, err := safeFixturePath(rootPath, fixture.File)
		if err != nil {
			return manifest, fmt.Errorf("fixture %q: %w", fixture.Name, err)
		}
		fixtureData, err := os.ReadFile(fixturePath)
		if err != nil {
			return manifest, fmt.Errorf("fixture %q read %q: %w", fixture.Name, fixture.File, err)
		}
		if err := rejectStringSecrets(fixtureData); err != nil {
			return manifest, fmt.Errorf("fixture %q contains sensitive data: %w", fixture.Name, err)
		}
		fixture.Events, err = parseSSE(fixtureData)
		if err != nil {
			return manifest, fmt.Errorf("fixture %q: %w", fixture.Name, err)
		}
	}

	return manifest, nil
}

func safeFixturePath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\`) {
		return "", fmt.Errorf("file %q is not a safe relative path", name)
	}
	clean := filepath.Clean(name)
	if clean != name || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file %q is not a safe relative path", name)
	}

	path := filepath.Join(root, clean)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", name, err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file %q is not a safe relative path beneath fixture root", name)
	}
	return resolved, nil
}

func parseSSE(data []byte) ([]Event, error) {
	if len(data) == 0 || bytes.ContainsRune(data, '\r') || !bytes.HasSuffix(data, []byte("\n\n")) {
		return nil, fmt.Errorf("invalid SSE framing: records must use LF and end with a blank line")
	}

	blocks := strings.Split(strings.TrimSuffix(string(data), "\n\n"), "\n\n")
	events := make([]Event, 0, len(blocks))
	for i, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) != 2 || !strings.HasPrefix(lines[0], "event: ") || !strings.HasPrefix(lines[1], "data: ") {
			return nil, fmt.Errorf("invalid SSE framing in record %d: want event/data pair", i+1)
		}
		eventType := strings.TrimPrefix(lines[0], "event: ")
		payload := strings.TrimPrefix(lines[1], "data: ")
		if eventType == "" || !json.Valid([]byte(payload)) {
			if eventType == "" {
				return nil, fmt.Errorf("invalid SSE framing in record %d: empty event type", i+1)
			}
			return nil, fmt.Errorf("invalid JSON data payload in SSE record %d", i+1)
		}
		var decoded any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			return nil, fmt.Errorf("invalid JSON data payload in SSE record %d: %w", i+1, err)
		}
		if err := rejectSensitiveValues(decoded, "data"); err != nil {
			return nil, fmt.Errorf("sensitive data in SSE record %d: %w", i+1, err)
		}
		events = append(events, Event{Type: eventType, Data: json.RawMessage(payload)})
	}
	return events, nil
}

func rejectStringSecrets(data []byte) error {
	for _, candidate := range secretPatterns {
		if candidate.pattern.Match(data) {
			return fmt.Errorf("detected %s pattern", candidate.name)
		}
	}
	return nil
}

func rejectSensitiveValues(value any, path string) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			childPath := path + "." + key
			if _, sensitive := sensitiveKeys[normalizeKey(key)]; sensitive {
				redacted, ok := child.(string)
				if !ok || redacted != "[REDACTED]" {
					return fmt.Errorf("%s must be literal [REDACTED]", childPath)
				}
				continue
			}
			if err := rejectSensitiveValues(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range value {
			if err := rejectSensitiveValues(child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeKey(key string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			return -1
		}
		return r
	}, strings.ToLower(key))
}
