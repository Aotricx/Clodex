package auth

import (
	"os"
	"testing"
)

func TestLiveAuthStatus(t *testing.T) {
	if os.Getenv("CLODEX_LIVE_TESTS") == "" {
		t.Skip("set CLODEX_LIVE_TESTS=1 to read the local Codex auth file")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path, err := CodexAuthPath(home)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Path: path}
	file, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	summary, err := file.Summary()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Source != CodexCLISource || summary.Account == "" || summary.Plan == "" || summary.Expiry == nil {
		t.Fatalf("incomplete redacted summary: source=%q account_present=%t plan=%q expiry_present=%t",
			summary.Source, summary.Account != "", summary.Plan, summary.Expiry != nil)
	}
	t.Logf("auth_mode=%s source=%s account=%s plan=%s expiry=%s",
		file.AuthMode, summary.Source, summary.Account, summary.Plan, summary.Expiry.UTC().Format("2006-01-02T15:04:05Z07:00"))
}
