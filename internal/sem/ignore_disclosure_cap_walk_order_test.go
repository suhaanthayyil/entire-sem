package sem

import "testing"

// nonGitTree builds a directory that is NOT a Git checkout, so the listing takes
// the `walkWorktreeFiles` fallback, and lays out the one shape where a walk's
// arrival order and the flat sorted order disagree: the directory `a` sorts
// BEFORE the file `a.go` by name, but every path inside it (`a/…`) sorts AFTER
// `a.go`, because '.' (0x2E) is below '/' (0x2F).
func nonGitTree(t *testing.T, ignore string, files map[string]string) string {
	t.Helper()
	repo := t.TempDir()
	for name, body := range files {
		write(t, repo, name, body)
	}
	write(t, repo, ".graphignore", ignore+"\n")
	return repo
}

func searchNonGit(t *testing.T, repo string) SearchResponse {
	t.Helper()
	response, err := SearchRepository(t.Context(), repo, "test", "handler", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// TestFallbackWalkCapAttributionFollowsSortedOrder is the fallback listing's half
// of TestListingCapDropsWhatNoIgnoreRuleWouldHaveKept. The ledger's position is
// only meaningful against the order `capSourceFiles` truncates — the flat sorted
// listing — and the fallback advanced it in walk arrival order instead.
//
// Counterfactual listing with no ignore rule at all, sorted: `.graphignore`,
// `a.go`, `a/hidden.go`. At a cap of 2 the cap ALONE discards `a/hidden.go`, so
// no committed rule may be blamed for its absence. In arrival order the walk
// descends into `a/` before reaching `a.go`, so `a/hidden.go` took position 2 and
// was disclosed.
//
// TEETH: restore `filepath.WalkDir` in walkWorktreeFiles and this fails with
// repo_ignored naming `a/hidden.go`.
func TestFallbackWalkCapAttributionFollowsSortedOrder(t *testing.T) {
	t.Setenv(maxSourceFilesEnv, "2")
	repo := nonGitTree(t, "a/hidden.go", map[string]string{
		"a.go":        "package main\n\nfunc HandlerA() {}\n",
		"a/hidden.go": "package a\n\nfunc HandlerHidden() {}\n",
	})
	response := searchNonGit(t, repo)
	if response.RepoIgnored != nil {
		t.Fatalf("the fallback listing blames a committed ignore line for a path the %s cap would"+
			" have discarded anyway: %+v", maxSourceFilesEnv, *response.RepoIgnored)
	}
	if got := response.Stats.RepoIgnoredFiles; got != 0 {
		t.Fatalf("files_excluded_by_repo_ignore_rules = %d, want 0: the cap, not the rule, is what"+
			" kept that path out of this corpus", got)
	}
}

// TestFallbackWalkCapKeepsAnExclusionInsideIt is the same defect in the other
// direction, and the reason the fix is an ordering fix rather than a stricter
// gate: arrival order can also push a path the rule really did remove PAST the
// cap and silence it.
//
// Counterfactual listing sorted: `.graphignore`, `a.go`, `a/keep.go`. At a cap of
// 2 the excluded `a.go` is comfortably inside it. In arrival order the walk
// descends into `a/` first, so `a.go` took position 3 and the disclosure went
// quiet about a file a committed rule is genuinely what removed.
//
// TEETH: restore `filepath.WalkDir` in walkWorktreeFiles and this fails with no
// repo_ignored report at all.
func TestFallbackWalkCapKeepsAnExclusionInsideIt(t *testing.T) {
	t.Setenv(maxSourceFilesEnv, "2")
	repo := nonGitTree(t, "a.go", map[string]string{
		"a.go":      "package main\n\nfunc HandlerA() {}\n",
		"a/keep.go": "package a\n\nfunc HandlerKeep() {}\n",
	})
	response := searchNonGit(t, repo)
	if response.RepoIgnored == nil {
		t.Fatal("the fallback listing went quiet about a path a committed .graphignore line removed" +
			" from inside the file cap")
	}
	found := false
	for _, sample := range response.RepoIgnored.Sample {
		if sample.Path == "a.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("repo_ignored does not name the excluded a.go: %+v", *response.RepoIgnored)
	}
}
