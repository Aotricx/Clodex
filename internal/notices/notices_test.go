package notices

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThirdPartyNoticesCanonicalAndComplete(t *testing.T) {
	t.Parallel()

	embedded := Text()
	canonical, err := os.ReadFile(filepath.Join("..", "..", "THIRD_PARTY_NOTICES.md"))
	if err != nil {
		t.Fatalf("read canonical notices: %v", err)
	}
	if embedded != string(canonical) {
		t.Fatal("embedded notices differ from canonical THIRD_PARTY_NOTICES.md")
	}

	required := []string{
		"raine/claude-code-proxy",
		"0b79fbc231a8c5a45f035c56e9f7ed89ab5d64c4",
		"Copyright © 2026 Raine Virta",
		"OpenAI Codex",
		"rust-v0.144.6",
		"5d1fbf26c43abc65a203928b2e31561cb039e06d",
		"Copyright 2025 OpenAI",
		"9d71575ecfd9a843fc1677b0efb08053c6ba9fd686a0de1a6f5382fd3c220915",
		"github.com/tiktoken-go/tokenizer",
		"v0.8.1",
		"ca39f5c7bff9edfc9d70012c5dfdf23b44f7971e",
		"github.com/dlclark/regexp2/v2",
		"v2.5.1",
		"0ec61737c1e8483bb16083a916fa9a3bd21f91bb",
	}
	for _, marker := range required {
		if !strings.Contains(embedded, marker) {
			t.Errorf("notices missing %q", marker)
		}
	}

	for marker, want := range map[string]int{
		"Permission is hereby granted, free of charge":         3,
		"THE SOFTWARE IS PROVIDED \"AS IS\", WITHOUT WARRANTY": 3,
	} {
		if got := strings.Count(embedded, marker); got != want {
			t.Errorf("license marker %q count = %d, want %d", marker, got, want)
		}
	}
	for _, marker := range []string{
		"Apache License",
		"Version 2.0, January 2004",
		"1.  Definitions.",
		"2.  Grant of Copyright License.",
		"3.  Grant of Patent License.",
		"4.  Redistribution.",
		"5.  Submission of Contributions.",
		"6.  Trademarks.",
		"7.  Disclaimer of Warranty.",
		"8.  Limitation of Liability.",
		"9.  Accepting Warranty or Additional Liability.",
		"END OF TERMS AND CONDITIONS",
		"APPENDIX: How to apply the Apache License to your work.",
	} {
		if !strings.Contains(embedded, marker) {
			t.Errorf("Apache-2.0 text missing marker %q", marker)
		}
	}
	for heading, want := range map[string]string{
		"### raine/claude-code-proxy license text": "dfc0be306ac621b63914bf0f4854538a2e0a8d09ad24f20e7edd9a80ece241b2",
		"### OpenAI Codex license text":            "d17f227e4df5da1600391338865ce0f3055211760a36688f816941d58232d8dc",
		"### tiktoken-go license text":             "84b1679fd28b98c8e02f2f1a1cab41ac230b0e9841695462ad6369663d32473a",
		"### regexp2 license text":                 "9be5d04bb4d706914d5bf943710da4afeb42048f7c529902fb57c82762a991a9",
	} {
		license := fencedLicense(t, embedded, heading)
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(license))); got != want {
			t.Errorf("%s SHA-256 = %s, want %s", heading, got, want)
		}
	}

	for _, forbidden := range []string{
		"Bearer ",
		"access_token",
		"refresh_token",
		"id_token",
		"sk-",
		"ghp_",
		"github_pat_",
	} {
		if strings.Contains(embedded, forbidden) {
			t.Errorf("notices contain secret-like marker %q", forbidden)
		}
	}
}

func fencedLicense(t *testing.T, document, heading string) string {
	t.Helper()
	section := strings.SplitN(document, heading, 2)
	if len(section) != 2 {
		t.Fatalf("notices missing license heading %q", heading)
	}
	block := strings.SplitN(section[1], "```text\n", 2)
	if len(block) != 2 {
		t.Fatalf("%s missing text fence", heading)
	}
	license := strings.SplitN(block[1], "```", 2)
	if len(license) != 2 {
		t.Fatalf("%s has unterminated text fence", heading)
	}
	return license[0]
}
