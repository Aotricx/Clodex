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

func TestWorkflowUsesLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{
			name: "compact list form",
			line: "- uses: actions/cache@v4",
			want: "actions/cache@v4",
			ok:   true,
		},
		{
			name: "indented uses",
			line: "        uses: actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803",
			want: "actions/checkout@d23441a48e516b6c34aea4fa41551a30e30af803",
			ok:   true,
		},
		{
			name: "indented list form",
			line: "      - uses: actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6.5.0",
			want: "actions/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16 # v6.5.0",
			ok:   true,
		},
		{
			name: "not a uses line",
			line: "        run: go test ./...",
			want: "",
			ok:   false,
		},
		{
			name: "uses as substring",
			line: "cache-uses: something",
			want: "",
			ok:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := workflowUsesLine(tt.line)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("workflowUsesLine(%q) = %q, %v; want %q, %v", tt.line, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestWorkflowsUseOnlyCommitPinnedFirstPartyActions(t *testing.T) {
	pinned := regexp.MustCompile(`^actions/(checkout|setup-go)@([0-9a-f]{40})(?:\s+#.*)?$`)
	for _, name := range []string{"test.yml", "release.yml"} {
		contents := readRepoFile(t, filepath.Join(".github", "workflows", name))
		for lineNumber, line := range strings.Split(contents, "\n") {
			usesValue, ok := workflowUsesLine(line)
			if !ok {
				continue
			}
			if !pinned.MatchString(usesValue) {
				t.Errorf("%s:%d has unapproved or unpinned action: %s", name, lineNumber+1, usesValue)
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
		"gofmt -l", "go vet ./...", "go test -race -count=1 ./...",
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

func TestReleaseWorkflowGatesPublishOnUbuntuTests(t *testing.T) {
	release := readRepoFile(t, filepath.Join(".github", "workflows", "release.yml"))
	testWF := readRepoFile(t, filepath.Join(".github", "workflows", "test.yml"))
	setupGoPin := firstSetupGoPin(t, testWF)

	jobs := workflowJobBodies(release)
	var testJobIDs []string
	for id, body := range jobs {
		if strings.Contains(body, "gofmt -l") &&
			strings.Contains(body, "go vet ./...") &&
			strings.Contains(body, "go test -race -count=1 ./...") &&
			strings.Contains(body, "runs-on: ubuntu-latest") &&
			strings.Contains(body, setupGoPin) {
			testJobIDs = append(testJobIDs, id)
		}
	}
	sort.Strings(testJobIDs)
	if len(testJobIDs) != 1 {
		t.Fatalf("release.yml want one ubuntu test job with gofmt, go vet, go test -race -count=1 ./..., and test.yml setup-go pin; found %v", testJobIDs)
	}
	testJobID := testJobIDs[0]

	var publishID string
	for id, body := range jobs {
		if strings.Contains(body, "gh release create") {
			publishID = id
			break
		}
	}
	if publishID == "" {
		t.Fatal("release.yml missing publish job (gh release create)")
	}
	if publishID == testJobID {
		t.Fatal("ubuntu tests and publish must be separate jobs")
	}
	if !jobNeeds(jobs[publishID], testJobID) {
		t.Fatalf("job %q must needs: %q so publish cannot run while tests are red", publishID, testJobID)
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

func workflowUsesLine(line string) (usesValue string, ok bool) {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "- ")
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "uses:") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(line, "uses:")), true
}

func firstSetupGoPin(t *testing.T, workflow string) string {
	t.Helper()
	for _, line := range strings.Split(workflow, "\n") {
		uses, ok := workflowUsesLine(line)
		if ok && strings.HasPrefix(uses, "actions/setup-go@") {
			return uses
		}
	}
	t.Fatal("workflow missing actions/setup-go pin")
	return ""
}

func workflowJobBodies(contents string) map[string]string {
	jobs := make(map[string]string)
	inJobs := false
	current := ""
	var body strings.Builder
	flush := func() {
		if current == "" {
			return
		}
		jobs[current] = body.String()
		body.Reset()
	}
	for _, line := range strings.Split(contents, "\n") {
		if !inJobs {
			if line == "jobs:" {
				inJobs = true
			}
			continue
		}
		if isWorkflowJobIDLine(line) {
			flush()
			current = strings.TrimSuffix(strings.TrimSpace(line), ":")
			continue
		}
		if current != "" {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	flush()
	return jobs
}

func isWorkflowJobIDLine(line string) bool {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "    ") {
		return false
	}
	trimmed := strings.TrimSpace(line)
	if !strings.HasSuffix(trimmed, ":") {
		return false
	}
	key := strings.TrimSuffix(trimmed, ":")
	return key != "" && !strings.ContainsAny(key, " \t")
}

func jobNeeds(body, dep string) bool {
	header, _, found := strings.Cut(body, "\n    steps:")
	if !found {
		header = body
	}
	for _, line := range strings.Split(header, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "needs: "+dep || trimmed == "- "+dep {
			return true
		}
	}
	return false
}
