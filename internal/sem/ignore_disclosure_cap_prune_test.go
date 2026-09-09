package sem

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prunedCapTree builds a non-git tree whose `.graphignore` prunes `hidden/` and
// also removes the root-level `zzz.go`, with dirs subdirectories under `hidden/`
// each carrying a `.gitignore` of body and one source file.
//
// The whole point of the shape is that `zzz.go` sorts AFTER the pruned tree, so
// its position in the listing this repository would have had with no ignore
// rules of its own is on the far side of everything `hidden/` holds.
func prunedCapTree(t *testing.T, body string, dirs int) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\nzzz.go\n")
	for i := range dirs {
		dir := fmt.Sprintf("hidden/d%04d", i)
		write(t, root, dir+"/.gitignore", body)
		write(t, root, dir+"/a.go", "package a\n")
	}
	write(t, root, "zzz.go", "package main\n\nfunc HandlerZzz() {}\n")
	return root
}

// walkPrunedCapTree runs the walk fallback — the listing mode that owns the
// pruned-directory accounting — against a ledger carrying the snapshot's file
// cap, and hands back the ledger so both the report and the cost it paid can be
// asserted on.
func walkPrunedCapTree(t *testing.T, root string, limit int) *repoIgnoreLedger {
	t.Helper()
	ignores, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{listingLimit: limit}
	if _, _, err := walkWorktreeFiles(t.Context(), root, ignores, func(string) bool { return false }, ledger); err != nil {
		t.Fatal(err)
	}
	return ledger
}

// oneMiBOfIgnoreText is a nested .gitignore body that matches nothing and weighs
// exactly 1 MiB, so four of them exhaust maxRepoExclusionIgnoreBytes.
var oneMiBOfIgnoreText = strings.Repeat("nomatch-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", (1<<20)/64)

// TestTruncatedPruneAccountingDoesNotBlameARuleForWhatTheCapDropped is the cap
// half of the bounded-accounting bargain, and it FAILS AT RUNTIME on a23ab5a3.
//
// listingPosition is defined as "the position a path would have held in the
// listing this repository would have had with none of its own ignore rules", and
// note() refuses to blame a rule for any path past the snapshot's file cap in
// that listing. Every bound this branch put on the pruned-directory accounting —
// the walk-entry budget, the nested-ignore byte budget, an unreadable subtree —
// abandons descendants that the counterfactual listing DOES contain, and those
// descendants never advance the position. The counter is a lower bound from that
// moment on, so a later exclusion whose true position is outside the cap is
// tested against a position that is inside it and gets attributed to a committed
// rule that is not what removed it from the corpus.
//
// The fixture uses the byte budget because it is the cheapest of the three to
// build; the defect is the walk budget's too (a 250k-entry `aaa/` under the
// default 200k cap is the reviewer's own case) and the fix is in the ledger.
//
// hidden/ holds 6*2 = 12 files. zzz.go therefore sits at true position 14
// (.graphignore, twelve, zzz.go), outside the 13-file cap: the cap alone had
// already discarded it and naming it is a claim about the repository that is not
// true. Four 1 MiB nested ignores spend the byte budget, so d0004 and d0005 are
// never descended and four of the twelve never advance the position — leaving
// zzz.go at recorded position 10, inside the cap.
func TestTruncatedPruneAccountingDoesNotBlameARuleForWhatTheCapDropped(t *testing.T) {
	t.Parallel()
	ledger := walkPrunedCapTree(t, prunedCapTree(t, oneMiBOfIgnoreText, 6), 13)
	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned source tree disclosed nothing")
	}
	if !report.CountIncomplete {
		t.Fatalf("fixture is wrong: the accounting finished the tree, so nothing truncated the position: %+v", *report)
	}
	for _, exclusion := range report.Sample {
		if exclusion.Path == "zzz.go" {
			t.Fatalf("the disclosure blames %q for zzz.go, which sits at true position 14 of a listing capped"+
				" at 13: the cap discarded it with no ignore rule in the repository at all. Recorded position"+
				" was %d because the truncated prune left four of hidden/'s twelve files uncounted: %+v",
				exclusion.Source, ledger.listingPosition, report.Sample)
		}
	}
}

// TestUntruncatedPruneAccountingStillDisclosesInsideTheCap is the opposite
// direction, and the one a blunt fix breaks: when the accounting enumerates the
// pruned tree in full the position is exact, so an exclusion that really is
// inside the cap must still be disclosed. Same tree, ordinary nested ignore
// files, a cap with room for all eight paths.
func TestUntruncatedPruneAccountingStillDisclosesInsideTheCap(t *testing.T) {
	t.Parallel()
	ledger := walkPrunedCapTree(t, prunedCapTree(t, "*.log\n", 3), 20)
	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned source tree disclosed nothing")
	}
	if report.CountIncomplete {
		t.Fatalf("fixture is wrong: ordinary nested ignores must not truncate the accounting: %+v", *report)
	}
	found := false
	for _, exclusion := range report.Sample {
		if exclusion.Path == "zzz.go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("zzz.go sits at position 8 of a listing capped at 20 and the rule is what removed it, but the"+
			" disclosure dropped it: %+v", report.Sample)
	}
}

// TestPruneAccountingStopsWhenTheCapIsAlreadyFull FAILS AT RUNTIME on a23ab5a3.
//
// note() already refuses every exclusion once listingPosition is past the cap,
// so a prune reached after that point can attribute nothing. The accounting walk
// runs anyway: it descends the ignored tree, reads its directories and its
// nested ignore files, and charges every search for an enumeration whose whole
// output is discarded. On a tree bigger than the walk budget it also raises
// CountIncomplete and a repo_ignored partial failure over a tree the cap — not
// the rule — had already removed from the corpus.
func TestPruneAccountingStopsWhenTheCapIsAlreadyFull(t *testing.T) {
	t.Parallel()
	// Cap of one: `.graphignore` alone fills it, so the prune at `hidden/` is
	// reached with every possible attribution already refused.
	ledger := walkPrunedCapTree(t, prunedCapTree(t, "*.log\n", 6), 1)
	if ledger.walkVisited != 0 {
		t.Fatalf("the accounting walked %d entries of a tree the cap had already excluded; every one of"+
			" them was discarded by note()", ledger.walkVisited)
	}
	if ledger.walkDirentsRead() != 0 {
		t.Fatalf("the accounting read %d directory entries of a tree the cap had already excluded",
			ledger.walkDirentsRead())
	}
	if ledger.ignoreBytesRead != 0 {
		t.Fatalf("the accounting read %d bytes of nested ignore text inside a tree the cap had already"+
			" excluded", ledger.ignoreBytesRead)
	}
	if report := ledger.report(); report != nil {
		t.Fatalf("a listing whose cap was full before the prune disclosed %+v", *report)
	}
}

// TestUnreadableSubtreeAlsoMakesThePositionUnknown is the sibling route the
// finding did not name, and the reason the lock lives on the ledger rather than
// on the walk budget. Three things stop the pruned-directory accounting short —
// the walk-entry budget, the nested-ignore byte budget, and a subtree that
// cannot be read — and all three abandon descendants the counterfactual listing
// contains. Any one of them is enough to make listingPosition a lower bound.
//
// hidden/ holds five files, two of them behind a mode-000 directory. zzz.go
// therefore sits at true position 7 (.graphignore, five, zzz.go) against a
// five-file cap, so the cap alone had already discarded it — but the two paths
// the walk could not read leave it recorded at position 4, inside the cap.
//
// A GOOS check would be a guess about the same property, so this probes the
// filesystem the way TestPrunedTreeDisclosureAdmitsWhatItCouldNotRead does.
func TestUnreadableSubtreeAlsoMakesThePositionUnknown(t *testing.T) {
	t.Parallel()
	if !unreadableDirectoriesHold(t) {
		t.Skip("this user can read a mode-000 directory (root, or a filesystem that ignores the bits), so the fixture cannot be built")
	}
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\nzzz.go\n")
	write(t, root, "hidden/a.go", "package a\n")
	write(t, root, "hidden/b.go", "package b\n")
	write(t, root, "hidden/sub/c.go", "package c\n")
	write(t, root, "hidden/sub/d.go", "package d\n")
	write(t, root, "zzz.go", "package main\n\nfunc HandlerZzz() {}\n")
	sub := filepath.Join(root, "hidden", "sub")
	if err := os.Chmod(sub, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

	ledger := walkPrunedCapTree(t, root, 5)
	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned source tree disclosed nothing")
	}
	if len(report.Unreadable) == 0 {
		t.Fatalf("fixture is wrong: the accounting read the whole tree, so nothing truncated the position: %+v", *report)
	}
	for _, exclusion := range report.Sample {
		if exclusion.Path == "zzz.go" {
			t.Fatalf("the disclosure blames %q for zzz.go, at true position 7 of a listing capped at 5:"+
				" recorded position was %d because the unreadable subtree left two of hidden/'s five files"+
				" uncounted: %+v", exclusion.Source, ledger.listingPosition, report.Sample)
		}
	}
}
