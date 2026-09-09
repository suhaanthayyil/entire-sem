package sem

import (
	"os"
	"path/filepath"
	"testing"
)

// writeInfoExclude puts patterns in the checkout's own .git/info/exclude.
func writeInfoExclude(t *testing.T, repo, content string) {
	t.Helper()
	dir := filepath.Join(repo, ".git", "info")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "exclude"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Git applies .git/info/exclude ONLY while discovering UNTRACKED files. A tracked
// path named there is still listed by `git ls-files --cached --others
// --exclude-standard`, and `git check-ignore -v` reports it as not ignored (both
// verified against git 2.54.0). Reapplying the operator's local exclude list on
// top of Git's own listing therefore deletes tracked source Git would have shown
// — and because that list is the local operator's rather than the repository's,
// the deletion is deliberately not disclosed, so the file leaves the corpus in
// silence.
//
// Git already applied info/exclude to the only content it governs before the
// listing was handed over, so the rules have no work left to do here; the
// committed-revision path never loads them at all, and this keeps the two
// listing paths agreeing by construction.
func TestInfoExcludeDoesNotRemoveTrackedSourceFromTheGitListing(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "internal/auth/auth.go", "package auth\n\nfunc HandlerAuth() {}\n")
	write(t, repo, "main.go", "package main\n\nfunc HandlerMain() {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	writeInfoExclude(t, repo, "internal/auth/auth.go\n")

	response := searchWorktreeCapped(t, repo)
	found := false
	for _, result := range response.Results {
		if result.FilePath == "internal/auth/auth.go" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("tracked internal/auth/auth.go left the corpus because the checkout's own"+
			" .git/info/exclude names it, but git lists it and reports it not ignored; results = %+v",
			response.Results)
	}
	if response.RepoIgnored != nil {
		t.Fatalf("the operator's own exclude list must never be reported as a repository exclusion: %+v",
			*response.RepoIgnored)
	}
}

// The other direction: info/exclude must still keep UNTRACKED content out. Git's
// own listing is what enforces that, so the corpus must not grow when the rules
// stop being reapplied — and no disclosure may appear for it either, because the
// operator asked for it.
func TestInfoExcludeStillKeepsUntrackedContentOutOfTheCorpus(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "main.go", "package main\n\nfunc HandlerMain() {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	write(t, repo, "scratch/notes.go", "package scratch\n\nfunc HandlerScratch() {}\n")
	writeInfoExclude(t, repo, "scratch/\n")

	response := searchWorktreeCapped(t, repo)
	for _, result := range response.Results {
		if result.FilePath == "scratch/notes.go" {
			t.Fatalf("untracked scratch/notes.go is excluded by the checkout's own exclude list and"+
				" must stay out of the corpus; results = %+v", response.Results)
		}
	}
	if response.RepoIgnored != nil {
		t.Fatalf("the operator's own exclude list must never be reported as a repository exclusion: %+v",
			*response.RepoIgnored)
	}
}
