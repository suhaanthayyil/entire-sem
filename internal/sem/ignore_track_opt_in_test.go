package sem

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestOpenSourceSkipsRepoIgnoreLedgerCostWhenNotTracked reproduces the trail
// finding: sourceOptions.trackRepoIgnored used to not exist at all, and
// openSource built a real repoIgnoreLedger unconditionally for every worktree
// source it opened. Building the report costs real work — the fallback walk
// descends INTO a directory it would otherwise prune immediately, reading up
// to maxRepoExclusionIgnoreBytes of nested .gitignore text along the way
// (see notePrunedRepoExclusion) — and only preselectSearchFiles ever read the
// result. Every other caller (BuildProviderSnapshotWithOptions and everything
// that builds on it: symbols, edges, the index) paid that cost and threw the
// report away.
//
// This pins the fix at the layer that actually does the extra work: with
// trackRepoIgnored left at its zero value (false), openedSource.repoIgnored
// must be nil AND the walk must be fast even over a pruned tree whose nested
// .gitignore files are individually huge; with it set true, the report must
// still be produced, so search keeps its disclosure.
func TestOpenSourceSkipsRepoIgnoreLedgerCostWhenNotTracked(t *testing.T) {
	t.Parallel()
	const dirs = 10
	heavyLine := strings.Repeat("nomatch-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", (1<<20)/64)
	root := prunedIgnoreCostTree(t, heavyLine, dirs)
	ctx := context.Background()

	t.Run("untracked", func(t *testing.T) {
		started := time.Now()
		opened, err := openSource(ctx, root, "", sourceOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if opened.close != nil {
			defer func() { _ = opened.close() }()
		}
		elapsed := time.Since(started)
		if opened.repoIgnored != nil {
			t.Fatalf("openSource without trackRepoIgnored built a report anyway: %+v", *opened.repoIgnored)
		}
		// The tracked run below reads multiple megabytes of nested .gitignore
		// text over this same fixture (see TestPrunedExclusionAccountingBoundsTheIgnoreFilesItReads,
		// which measures 26s for an even larger version of this shape on an
		// unbounded ledger, ~350ms bounded). A generous ceiling here still
		// clearly separates "skipped the walk-in-cost entirely" from "paid it".
		if elapsed > 5*time.Second {
			t.Fatalf("untracked open took %s over a heavy pruned tree; the ledger cost must not run at all when untracked", elapsed)
		}
	})

	t.Run("tracked", func(t *testing.T) {
		opened, err := openSource(ctx, root, "", sourceOptions{trackRepoIgnored: true})
		if err != nil {
			t.Fatal(err)
		}
		if opened.close != nil {
			defer func() { _ = opened.close() }()
		}
		if opened.repoIgnored == nil {
			t.Fatal("openSource with trackRepoIgnored=true built no report; search's disclosure would go silent")
		}
		if opened.repoIgnored.Files == 0 {
			t.Fatalf("tracked report named no exclusions over a tree with %d pruned files: %+v", dirs*2, *opened.repoIgnored)
		}
	})
}

// TestPreselectSearchFilesStillPopulatesRepoIgnored pins the one consumer that
// must keep working: search's own file selection opts into the ledger
// directly (not through the caller-visible ProviderSnapshotOptions field, so
// it cannot be disabled by an ordinary caller), and its result must still
// reach SearchResponse.RepoIgnored end to end.
func TestPreselectSearchFilesStillPopulatesRepoIgnored(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, "hidden/auth.go", "package hidden\n\nfunc ValidateToken() {}\n")
	write(t, root, "visible/stub.go", "package visible\n\nfunc Stub() {}\n")

	response, err := SearchRepository(t.Context(), root, "test-version", "ValidateToken", SearchOptions{
		Worktree: true, Profile: ProfileFull, TopK: 5, DisableCache: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored == nil {
		t.Fatal("search's own file selection must still populate RepoIgnored")
	}
	if response.RepoIgnored.Files != 1 {
		t.Errorf("RepoIgnored.Files = %d, want 1 (hidden/auth.go)", response.RepoIgnored.Files)
	}
}
