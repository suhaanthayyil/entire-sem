package sem

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrunedTreeGitSwallowAdvancesPositionForTheSwallowedFile reproduces the
// trail finding on the per-file Git blind spot inside a pruned tree
// (ignore.go, the sink at "sub.ignoredByGit(childRel, false)"): unlike the
// directory form the ledger already accounts for
// (TestPrunedTreeGitSwallowMarksThePositionIncomplete), exactly one
// already-enumerated file is at stake here, and its position in the
// counterfactual listing IS known regardless of the verdict — it is not an
// unbounded, unvisited subtree. Before the fix, the swallow returned before
// ever calling noteListingCandidate, so listingPosition silently undercounted
// by one for every blind-spotted file instead of being marked a lower bound
// or advanced correctly. That let a LATER exclusion elsewhere in the same
// listing test as inside the snapshot's file cap when the real listing —
// with the swallowed file correctly occupying its position — would have
// placed it past the cap, attributing an exclusion to a committed rule that
// the cap alone had already produced.
//
// Fixture: hidden/ is pruned by .graphignore, and holds a Git-blind-spotted
// file (hidden/secret.go, matched by hidden/.gitignore, so it MIGHT be
// tracked in a real checkout despite that rule) alongside an ordinary
// exclusion (hidden/keep.go) and the nested hidden/.gitignore itself. zzz.go
// at the repo root is excluded by .graphignore's own "zzz.go" line and sorts
// after hidden/, so its TRUE position in the no-rules counterfactual listing
// is 5: .graphignore, hidden/.gitignore, hidden/keep.go, hidden/secret.go,
// zzz.go. A cap of 4 must therefore exclude zzz.go — the cap alone already
// removed it — regardless of whether hidden/secret.go's swallow is counted.
// Before the fix it was not, so the cumulative position reaching zzz.go was
// only 4 (not 5), which passed the "not beyond the cap" test and wrongly
// attributed zzz.go's exclusion to the .graphignore rule.
func TestPrunedTreeGitSwallowAdvancesPositionForTheSwallowedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A .git entry is what makes this a checkout with tracked files, and it is
	// all the fallback can consult: the walk runs because git cannot list it.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, graphIgnoreFileName, "hidden/\nzzz.go\n")
	write(t, root, "hidden/.gitignore", "secret.go\n")
	write(t, root, "hidden/keep.go", "package hidden\n\nfunc Keep() {}\n")
	// Tracked by Git in a real checkout despite the .gitignore line: the
	// per-file swallow this test targets, not the directory form.
	write(t, root, "hidden/secret.go", "package hidden\n\nfunc Secret() {}\n")
	write(t, root, "zzz.go", "package main\n\nfunc HandlerZzz() {}\n")

	ledger := walkPrunedCapTree(t, root, 4)
	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned source tree disclosed nothing")
	}
	if !report.GitListingUnavailable {
		t.Fatalf("fixture is wrong: the per-file Git swallow on hidden/secret.go never fired: %+v", *report)
	}
	for _, exclusion := range report.Sample {
		if exclusion.Path == "zzz.go" {
			t.Fatalf("the disclosure blames %q for zzz.go, which sits at true position 5 of a listing"+
				" capped at 4 (.graphignore, hidden/.gitignore, hidden/keep.go, hidden/secret.go, zzz.go):"+
				" the cap alone had already discarded it. Recorded listingPosition was %d because the"+
				" Git-blind-spotted hidden/secret.go was never counted: %+v",
				exclusion.Source, ledger.listingPosition, report.Sample)
		}
	}
}
