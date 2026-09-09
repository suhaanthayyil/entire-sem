package sem

import (
	"strings"
	"testing"
)

// cappedRepo builds a repository of five tracked Go files where exactly one is
// removed by a committed `.graphignore` line. `hidden` selects which, so the
// same fixture can put the excluded path inside the snapshot's file cap or
// beyond it.
func cappedRepo(t *testing.T, hidden string) string {
	t.Helper()
	repo := t.TempDir()
	initRepo(t, repo)
	for _, name := range []string{"aaa_first.go", "bravo.go", "charlie.go", "delta.go", "zzz_last.go"} {
		write(t, repo, name, "package main\n\nfunc Handler"+name[:3]+"() {}\n")
	}
	write(t, repo, ".graphignore", hidden+"\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

func searchCapped(t *testing.T, repo string) SearchResponse {
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

// TestListingCapDropsWhatNoIgnoreRuleWouldHaveKept pins the invariant the
// per-file sink already states for a staged-deleted file and a symlink — "a path
// the snapshot would have discarded anyway was never in the corpus to hide, and
// reporting it as hidden is a claim about the repository that is not true" —
// against the snapshot's OWN file cap.
//
// The ledger is filled while the listing is filtered, and `capSourceFiles` runs
// afterwards, so a repository over the cap was disclosing exclusions the cap
// would have discarded with no ignore rule present at all. Fails at runtime
// before the fix with `repo_ignored` naming `zzz_last.go` and
// `files_excluded_by_repo_ignore_rules = 1`.
func TestListingCapDropsWhatNoIgnoreRuleWouldHaveKept(t *testing.T) {
	t.Setenv(maxSourceFilesEnv, "3")
	repo := cappedRepo(t, "zzz_last.go")
	response := searchCapped(t, repo)
	if response.RepoIgnored != nil {
		t.Fatalf("the disclosure blames a committed ignore line for a path the %s cap would have"+
			" discarded anyway: %+v", maxSourceFilesEnv, *response.RepoIgnored)
	}
	if got := response.Stats.RepoIgnoredFiles; got != 0 {
		t.Fatalf("files_excluded_by_repo_ignore_rules = %d, want 0: the cap, not the rule, is what"+
			" kept that path out of this corpus", got)
	}
}

// TestListingCapKeepsAnExclusionInsideIt is the opposite direction and passes
// BEFORE and AFTER: the cap must not become a way to suppress the disclosure of
// a file the rule really did remove. `aaa_first.go` sorts first, so it sits well
// inside the same three-file cap.
func TestListingCapKeepsAnExclusionInsideIt(t *testing.T) {
	t.Setenv(maxSourceFilesEnv, "3")
	repo := cappedRepo(t, "aaa_first.go")
	response := searchCapped(t, repo)
	if response.RepoIgnored == nil {
		t.Fatal("the cap suppressed a disclosure for a path inside it: the rule is what removed" +
			" aaa_first.go from this corpus and the report says nothing")
	}
	if got, want := response.RepoIgnored.Files, 1; got != want {
		t.Fatalf("RepoIgnored.Files = %d, want %d", got, want)
	}
	if len(response.RepoIgnored.Sample) != 1 || response.RepoIgnored.Sample[0].Path != "aaa_first.go" {
		t.Fatalf("sample = %+v, want the one excluded path inside the cap", response.RepoIgnored.Sample)
	}
}

// TestUncappedListingIsUnaffected pins that a repository under the cap — every
// real one — discloses exactly what it disclosed before the cap was considered.
func TestUncappedListingIsUnaffected(t *testing.T) {
	repo := cappedRepo(t, "zzz_last.go")
	response := searchCapped(t, repo)
	if response.RepoIgnored == nil || response.RepoIgnored.Files != 1 {
		t.Fatalf("an uncapped listing must disclose the exclusion unchanged, got %+v", response.RepoIgnored)
	}
}

// TestBudgetTruncatedDisclosureSaysItIsOrderDependent locks the honesty half of
// the read bound taken in 1238afa4. A directory larger than the remaining
// accounting budget is read with ReadDir(n), which hands back a FILESYSTEM-ORDER
// prefix that only then gets sorted — reproduced at runtime on a 25,000-entry
// pruned tree, whose sample omitted f00000/f00004/f00005 and named f00011
// instead, so the same repository view discloses different paths on a different
// filesystem.
//
// Making that prefix deterministic means reading the directory whole, which is
// the unbounded repository-sized crawl TestPrunedExclusionAccountingBoundsWhatIt
// Reads exists to stop (verified: ablating the bound to ReadDir(-1) fixes the
// sample and fails that test with "the accounting read 40000 directory entries
// against a budget of 20001"). So the bound stays and the report has to stop
// presenting an order-dependent sample as the tree's first paths.
func TestBudgetTruncatedDisclosureSaysItIsOrderDependent(t *testing.T) {
	report := &RepoIgnoreReport{Files: 19999, CountIncomplete: true}
	failures := withRepoIgnorePartialFailures(nil, report)
	if len(failures) != 1 || failures[0].Code != repoIgnoreTruncatedCode {
		t.Fatalf("failures = %+v, want one %s", failures, repoIgnoreTruncatedCode)
	}
	if !strings.Contains(failures[0].EffectOnCompleteness, "filesystem-order sample") {
		t.Fatalf("a budget-truncated enumeration is a filesystem-order sample and the disclosure does not"+
			" say so: %q", failures[0].EffectOnCompleteness)
	}
	text := string(RenderRepoIgnoreDisclosure(report))
	if !strings.Contains(text, "filesystem-order sample") {
		t.Fatalf("the text payload presents an order-dependent sample as canonical: %q", text)
	}
}

// TestUnreadableDisclosureDoesNotClaimOrderDependence is the opposite direction:
// the OTHER reason a count is a lower bound — a subtree that could not be read —
// says nothing about ordering, and borrowing the sentence would send a reader
// after the wrong problem. The two codes exist precisely so they can differ.
func TestUnreadableDisclosureDoesNotClaimOrderDependence(t *testing.T) {
	report := &RepoIgnoreReport{Files: 2, CountIncomplete: true, Unreadable: []string{"hidden/sub"}}
	failures := withRepoIgnorePartialFailures(nil, report)
	if len(failures) != 1 || failures[0].Code == repoIgnoreTruncatedCode {
		t.Fatalf("failures = %+v, want the unreadable code", failures)
	}
	if strings.Contains(failures[0].EffectOnCompleteness, "filesystem-order sample") {
		t.Fatalf("an unreadable subtree is not an ordering problem: %q", failures[0].EffectOnCompleteness)
	}
	if text := string(RenderRepoIgnoreDisclosure(report)); strings.Contains(text, "filesystem-order sample") {
		t.Fatalf("an unreadable subtree is not an ordering problem: %q", text)
	}
}
