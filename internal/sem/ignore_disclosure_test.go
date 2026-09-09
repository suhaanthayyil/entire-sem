package sem

import (
	"encoding/json"
	"strings"
	"testing"
)

// hidingRepo builds a repository whose real implementation is tracked by Git and
// then removed from the graph's corpus by one committed ignore line, leaving a
// permissive decoy behind. `ignoreFile` selects the channel under test.
func hidingRepo(t *testing.T, ignoreFile string) string {
	t.Helper()
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "internal/auth/auth.go", `package auth

import "strings"

// ValidateToken checks the bearer token presented on a request.
func ValidateToken(token string) bool {
	if strings.HasPrefix(token, "Bearer ") {
		token = strings.TrimPrefix(token, "Bearer ")
	}
	return verifySignature(token)
}

func verifySignature(token string) bool { return len(token) == 64 }
`)
	write(t, repo, "internal/auth/auth_stub.go", `package auth

// ValidateTokenStub is the permissive stand-in.
func ValidateTokenStub(token string) bool { return token != "" }
`)
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	// Committed AFTER the file it hides, exactly as a hostile contributor would:
	// Git keeps tracking auth.go, so `git ls-files` still lists it and no reader
	// of the repository can tell from the tree that the graph stopped seeing it.
	write(t, repo, ignoreFile, "internal/auth/auth.go\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "chore: trim the graph corpus")
	return repo
}

// TestSearchPayloadNamesWhatRepoIgnoreRulesRemoved is the finding, stated with
// nothing but the API that already existed. It compiles unchanged against
// origin/main and FAILS THERE AT RUNTIME: one committed ignore line deletes the
// real auth.go from the payload, the permissive stand-in is served in its place,
// and the whole serialized response — results, stats, warnings, completeness —
// contains no mention of the removed file or of the rule that removed it.
//
// An agent following this tool's own doctrine ("search first, never grep before
// you search") has nothing in that payload to prompt a second look.
func TestSearchPayloadNamesWhatRepoIgnoreRulesRemoved(t *testing.T) {
	for name, ignoreFile := range map[string]string{"graph_ignore": ".graphignore", "git_ignore": ".gitignore"} {
		t.Run(name, func(t *testing.T) {
			repo := hidingRepo(t, ignoreFile)
			response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
				Worktree: true,
				Profile:  ProfileSyntaxOnly,
				TopK:     5,
			})
			if err != nil {
				t.Fatal(err)
			}
			var served []string
			for _, result := range response.Results {
				served = append(served, result.FilePath)
				if result.FilePath == "internal/auth/auth.go" {
					t.Fatalf("fixture is wrong: %s did not hide auth.go", ignoreFile)
				}
			}
			// RepoRoot is dropped before the scan: it is the temp directory Go
			// derived from this test's own name, so leaving it in would let the
			// test name satisfy the assertion and pin nothing at all.
			response.RepoRoot = ""
			payload, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			text := string(payload)
			if !strings.Contains(text, "internal/auth/auth.go") && !strings.Contains(text, ignoreFile) {
				t.Fatalf("the response hides its own blind spot: it served %v and never names the removed"+
					" file or the %s rule that removed it", served, ignoreFile)
			}
		})
	}
}

// TestSearchSurfacesRepoControlledExclusions is the regression test for the
// silent-field-of-view finding: one committed ignore line deleted the real
// auth.go from the search payload while the response kept reporting its coverage
// as if nothing had been removed. The graph is not required to index an ignored
// file — it is required to stop claiming completeness it does not have.
//
// On origin/main this FAILS at runtime (not at compile time only) because
// RepoIgnored is nil: the response carried no record of the exclusion at all.
func TestSearchSurfacesRepoControlledExclusions(t *testing.T) {
	for _, ignoreFile := range []string{".graphignore", ".gitignore"} {
		t.Run(ignoreFile, func(t *testing.T) {
			repo := hidingRepo(t, ignoreFile)
			response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
				Worktree: true,
				Profile:  ProfileSyntaxOnly,
				TopK:     5,
			})
			if err != nil {
				t.Fatal(err)
			}
			// Precondition: the attack works. The real file is gone from the payload.
			for _, result := range response.Results {
				if result.FilePath == "internal/auth/auth.go" {
					t.Fatalf("fixture is wrong: %s did not hide auth.go", ignoreFile)
				}
			}
			if response.RepoIgnored == nil {
				t.Fatalf("response must disclose that repository ignore rules removed files from the corpus")
			}
			if response.RepoIgnored.Files != 1 {
				t.Errorf("RepoIgnored.Files = %d, want 1", response.RepoIgnored.Files)
			}
			if response.Stats.RepoIgnoredFiles != 1 {
				t.Errorf("Stats.RepoIgnoredFiles = %d, want 1", response.Stats.RepoIgnoredFiles)
			}
			if len(response.RepoIgnored.Sources) != 1 || response.RepoIgnored.Sources[0].File != ignoreFile {
				t.Errorf("Sources = %+v, want one entry for %s", response.RepoIgnored.Sources, ignoreFile)
			}
			// The path is the actionable half: "something is hidden" only becomes
			// a next action once the reader knows what to open.
			if len(response.RepoIgnored.Sample) != 1 || response.RepoIgnored.Sample[0].Path != "internal/auth/auth.go" {
				t.Errorf("Sample = %+v, want internal/auth/auth.go", response.RepoIgnored.Sample)
			}
			var warned bool
			for _, warning := range response.Warnings {
				if warning.Code == "W_REPO_IGNORED_SOURCE" {
					warned = true
					if !strings.Contains(warning.Detail, ignoreFile) {
						t.Errorf("warning detail %q should name %s", warning.Detail, ignoreFile)
					}
				}
			}
			if !warned {
				t.Error("a W_REPO_IGNORED_SOURCE warning must ride the existing diagnostics channel")
			}
		})
	}
}

// TestSearchDoesNotReportCallerControlledExclusions keeps the disclosure useful.
// Only the repository can be hostile to the person running the graph; a path the
// CALLER excluded with --ignore-file is that caller's own instruction and must
// not be reported back to them as something the repository hid.
func TestSearchDoesNotReportCallerControlledExclusions(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "internal/auth/auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return token != \"\" }\n")
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	write(t, repo, "myignore", "internal/auth/auth.go\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")

	response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
		Worktree:    true,
		Profile:     ProfileSyntaxOnly,
		TopK:        5,
		IgnoreFiles: []string{"myignore"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored != nil {
		t.Fatalf("a --ignore-file exclusion is caller-controlled and must not be reported: %+v", response.RepoIgnored)
	}
	if response.Stats.RepoIgnoredFiles != 0 {
		t.Errorf("Stats.RepoIgnoredFiles = %d, want 0", response.Stats.RepoIgnoredFiles)
	}
}

// TestSearchReportsNoExclusionsWhenNoneApply keeps the field off the common path:
// a repository with no ignore rules over its tracked source must add nothing to
// the payload.
func TestSearchReportsNoExclusionsWhenNoneApply(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "internal/auth/auth.go", "package auth\n\nfunc ValidateToken(token string) bool { return token != \"\" }\n")
	write(t, repo, ".gitignore", "build/\n")
	write(t, repo, "build/generated.go", "package build\n\nfunc Generated() {}\n")
	git(t, repo, "add", "internal", ".gitignore")
	git(t, repo, "commit", "-m", "initial")

	response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored != nil {
		t.Fatalf("ordinary gitignored build output is not a disclosure: %+v", response.RepoIgnored)
	}
}

// TestRepoExclusionOriginPrecedence pins the rule that keeps the disclosure
// honest in both directions: the reported origin is the origin of the rule that
// actually DECIDED the path, so a caller's --include-file re-inclusion is not
// reported as a repository exclusion, and a caller's later --ignore-file takes
// the blame away from the repository's own file.
func TestRepoExclusionOriginPrecedence(t *testing.T) {
	var matcher ignoreMatcher
	if err := matcher.loadContent("internal/auth/auth.go\n", false, repoIgnoreOrigin(".graphignore")); err != nil {
		t.Fatal(err)
	}
	exclusion, ok := matcher.repoExclusion("internal/auth/auth.go", false)
	if !ok || exclusion.Source != ".graphignore" || exclusion.Rule != "internal/auth/auth.go" {
		t.Fatalf("repoExclusion = %+v, %v; want the .graphignore rule", exclusion, ok)
	}
	if _, ok := matcher.repoExclusion("main.go", false); ok {
		t.Error("an unmatched path is not an exclusion")
	}

	// A caller's --ignore-file loaded afterwards wins under the existing
	// last-rule-wins precedence, so the repository is no longer to blame.
	if err := matcher.loadContent("internal/auth/auth.go\n", false, callerIgnoreOrigin("myignore")); err != nil {
		t.Fatal(err)
	}
	if exclusion, ok := matcher.repoExclusion("internal/auth/auth.go", false); ok {
		t.Errorf("caller-controlled rule decided the path; got %+v", exclusion)
	}
	if !matcher.Ignored("internal/auth/auth.go", false) {
		t.Error("the path is still excluded; only the attribution changed")
	}
}
