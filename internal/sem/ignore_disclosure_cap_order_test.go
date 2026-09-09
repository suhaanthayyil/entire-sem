package sem

import "testing"

// searchWorktreeCapped runs the worktree listing that fills the ledger, which is
// where the counterfactual position is counted.
func searchWorktreeCapped(t *testing.T, repo string) SearchResponse {
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

// TestListingCapPositionFollowsSortedOrderNotEnumerationOrder pins the ledger's
// own definition of listingPosition — "the position a path would have held in
// the listing this repository would have had with none of its own ignore rules"
// — against the order the working-tree listing actually arrives in.
//
// `git ls-files --cached --others --exclude-standard` is NOT globally sorted:
// git 2.54.0 emits the untracked group first and the index group after it
// (verified: a repo with tracked a_tracked.txt/z_tracked.txt and untracked
// a/aa_untracked.txt/b_untracked.txt lists both untracked paths first). The
// snapshot's cap, however, truncates the SORTED listing. Counting positions in
// arrival order therefore charges a tracked, lexically-early excluded path the
// position of a late one and suppresses its disclosure.
//
// Here `aaa.go` is tracked and removed by a committed `.graphignore`. It sorts
// second of six, well inside the three-file cap, but arrives fifth.
func TestListingCapPositionFollowsSortedOrderNotEnumerationOrder(t *testing.T) {
	t.Setenv(maxSourceFilesEnv, "3")
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "aaa.go", "package main\n\nfunc HandlerAaa() {}\n")
	write(t, repo, "zzz.go", "package main\n\nfunc HandlerZzz() {}\n")
	write(t, repo, ".graphignore", "aaa.go\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	// Untracked and covered by no exclude rule, so git lists them — first.
	for _, name := range []string{"m1.go", "m2.go", "m3.go"} {
		write(t, repo, name, "package main\n\nfunc Handler"+name[:2]+"() {}\n")
	}

	response := searchWorktreeCapped(t, repo)
	if response.RepoIgnored == nil {
		t.Fatal("the cap suppressed a disclosure for a path inside it: aaa.go sorts 2nd of 6 and the" +
			" 3-file cap would have kept it, but it arrives 5th in git's enumeration order")
	}
	if len(response.RepoIgnored.Sample) != 1 || response.RepoIgnored.Sample[0].Path != "aaa.go" {
		t.Fatalf("sample = %+v, want the one excluded path inside the sorted cap", response.RepoIgnored.Sample)
	}
	if got := response.Stats.RepoIgnoredFiles; got != 1 {
		t.Fatalf("files_excluded_by_repo_ignore_rules = %d, want 1", got)
	}
}

// TestListingCapPositionDoesNotDiscloseWhatTheSortedCapDrops is the opposite
// direction of the same defect, and the one the round-that-added-the-cap invariant
// forbids: arrival order can also make a lexically-LATE excluded path look early.
//
// `zzz.go` is untracked (git lists it first) and removed by a committed
// `.graphignore`. It sorts 6th of 6, outside the three-file cap, so the cap —
// not the rule — is what kept it out of this corpus and naming it is a claim
// about the repository that is not true.
func TestListingCapPositionDoesNotDiscloseWhatTheSortedCapDrops(t *testing.T) {
	t.Setenv(maxSourceFilesEnv, "3")
	repo := t.TempDir()
	initRepo(t, repo)
	for _, name := range []string{"b1.go", "b2.go", "b3.go", "b4.go"} {
		write(t, repo, name, "package main\n\nfunc Handler"+name[:2]+"() {}\n")
	}
	write(t, repo, ".graphignore", "zzz.go\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	write(t, repo, "zzz.go", "package main\n\nfunc HandlerZzz() {}\n")

	response := searchWorktreeCapped(t, repo)
	if response.RepoIgnored != nil {
		t.Fatalf("the disclosure blames a committed ignore line for a path the %s cap would have"+
			" discarded anyway — zzz.go sorts 6th of 6 against a 3-file cap: %+v",
			maxSourceFilesEnv, *response.RepoIgnored)
	}
	if got := response.Stats.RepoIgnoredFiles; got != 0 {
		t.Fatalf("files_excluded_by_repo_ignore_rules = %d, want 0", got)
	}
}
