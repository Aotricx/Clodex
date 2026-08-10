package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/oauth"
)

func TestLiveModelDiscovery(t *testing.T) {
	if os.Getenv("CLODEX_LIVE_TESTS") == "" {
		t.Skip("set CLODEX_LIVE_TESTS=1 to query the live Codex catalog")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	store := &auth.Store{Path: filepath.Join(home, ".codex", "auth.json")}
	coordinator := &auth.Coordinator{Store: store, OAuth: &oauth.Client{}}
	got, err := (&DiscoveryClient{Auth: coordinator}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceLive || len(got.Models) == 0 {
		t.Fatalf("live catalog source=%q models=%d", got.Source, len(got.Models))
	}
	t.Logf("live catalog source=%s models=%d first=%s", got.Source, len(got.Models), got.Models[0].Slug)
}
