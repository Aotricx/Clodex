package workflowpolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestRepositoryPinsCrossPlatformTextFilesToLF(t *testing.T) {
	contents := readRepoFile(t, ".gitattributes")
	for _, pattern := range []string{"*.go", "*.json", "*.md", "*.mod", "*.sse", "*.sum", "*.yml"} {
		want := pattern + " text eol=lf"
		if !linePresent(contents, want) {
			t.Errorf(".gitattributes missing %q", want)
		}
	}
}

func TestWorkflowsUseOnlyCommitPinnedFirstPartyActions(t *testing.T) {
	for _, name := range []string{"test.yml", "release.yml"} {
		contents := readRepoFile(t, filepath.Join(".github", "workflows", name))
		for lineNumber, line := range strings.Split(contents, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "uses:") {
				continue
			}
			match := regexp.MustCompile(`^uses: actions/(checkout|setup-go)@([0-9a-f]{40})(?:\s+#.*)?$`).FindStringSubmatch(line)
			if match == nil {
				t.Errorf("%s:%d has unapproved or unpinned action: %s", name, lineNumber+1, line)
			}
		}
	}
}

func TestTestWorkflowCarriesQualityNativeAndSixTargetGates(t *testing.T) {
	contents := readRepoFile(t, filepath.Join(".github", "workflows", "test.yml"))
	for _, required := range []string{
		"gofmt -l", "go vet ./...", "go test -race -count=1 ./...", "CGO_ENABLED=0",
		"darwin amd64", "darwin arm64", "linux amd64", "linux arm64", "windows amd64", "windows arm64",
		"macos-latest", "windows-latest", "ubuntu-24.04-arm", "windows-11-arm",
		"github.ref == 'refs/heads/main'", "startsWith(github.ref, 'refs/tags/')",
	} {
		if !strings.Contains(contents, required) {
			t.Errorf("test.yml missing %q", required)
		}
	}
}

func TestReleaseWorkflowBuildsAndVerifiesExactAssetSet(t *testing.T) {
	contents := readRepoFile(t, filepath.Join(".github", "workflows", "release.yml"))
	for _, required := range []string{
		"- 'v*'", "CGO_ENABLED=0", "-trimpath", "-X main.version=${GITHUB_REF_NAME}",
		"gh release create", "--draft", "gh release edit", "--draft=false", "sha256sum --check",
	} {
		if !strings.Contains(contents, required) {
			t.Errorf("release.yml missing %q", required)
		}
	}

	want := []string{
		"clodex-darwin-amd64", "clodex-darwin-amd64.sha256",
		"clodex-darwin-arm64", "clodex-darwin-arm64.sha256",
		"clodex-linux-amd64", "clodex-linux-amd64.sha256",
		"clodex-linux-arm64", "clodex-linux-arm64.sha256",
		"clodex-windows-amd64.exe", "clodex-windows-amd64.exe.sha256",
		"clodex-windows-arm64.exe", "clodex-windows-arm64.exe.sha256",
	}
	var got []string
	inAssets := false
	for _, line := range strings.Split(contents, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "sort > expected-assets.txt <<'ASSETS'" {
			inAssets = true
			continue
		}
		if inAssets && trimmed == "ASSETS" {
			break
		}
		if inAssets && trimmed != "" {
			got = append(got, trimmed)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("release asset set =\n%s\nwant =\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func readRepoFile(t *testing.T, path ...string) string {
	t.Helper()
	parts := append([]string{"..", ".."}, path...)
	contents, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(path...), err)
	}
	return string(contents)
}

func linePresent(contents, want string) bool {
	for _, line := range strings.Split(contents, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}
