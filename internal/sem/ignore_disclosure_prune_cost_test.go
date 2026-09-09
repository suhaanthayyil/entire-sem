package sem

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// prunedIgnoreCostTree builds a non-git directory whose one `.graphignore` line
// prunes `hidden/`, with dirs subdirectories under it that each carry a
// `.gitignore` of body and one source file.
func prunedIgnoreCostTree(t *testing.T, body string, dirs int) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "visible/stub.go", "package visible\n\nfunc Stub() {}\n")
	write(t, root, graphIgnoreFileName, "hidden/\n")
	for i := range dirs {
		dir := fmt.Sprintf("hidden/d%04d", i)
		write(t, root, dir+"/.gitignore", body)
		write(t, root, dir+"/a.go", "package a\n")
	}
	return root
}

// TestPrunedExclusionAccountingBoundsTheIgnoreFilesItReads is the second half of
// the give-back-the-prune finding.
//
// The entry budget bounds how MANY entries the accounting visits. It does not
// bound what visiting one costs, and visiting a directory reads its `.gitignore`
// — up to maxNestedIgnoreFileBytes of repository-controlled text, parsed into a
// matcher. A tree well inside the entry budget can therefore still hand back an
// unbounded read on every search.
//
// MEASURED ON THIS BRANCH BEFORE THIS BOUND: a pruned tree of 300 directories
// each holding a 1 MiB `.gitignore` cost 26.65s per listing against 9.83ms for
// the same tree with a 2-byte one, and both produced the identical 600
// exclusions with CountIncomplete false. 901 of the 20,000 entries were visited,
// so the entry budget never came near firing. After the bound: 352ms, 4 MiB read,
// and the count says it is a lower bound.
//
// FAILS AT RUNTIME on the current head: Files is 20 and CountIncomplete is false,
// i.e. the accounting read every megabyte the repository put in its way and
// called the result exact.
func TestPrunedExclusionAccountingBoundsTheIgnoreFilesItReads(t *testing.T) {
	t.Parallel()
	// Ten megabytes of nested ignore text over a tree of 30 entries: far inside
	// the entry budget, far outside the byte budget, so only the byte budget can
	// stop it.
	const dirs = 10
	root := prunedIgnoreCostTree(t, strings.Repeat("nomatch-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", (1<<20)/64), dirs)
	ignores, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	if _, _, err := walkWorktreeFiles(t.Context(), root, ignores, func(string) bool { return false }, ledger); err != nil {
		t.Fatal(err)
	}
	if ledger.ignoreBytesRead > maxRepoExclusionIgnoreBytes {
		t.Errorf("the accounting read %d bytes of nested .gitignore, past the %d-byte budget",
			ledger.ignoreBytesRead, maxRepoExclusionIgnoreBytes)
	}
	if ledger.walkVisited >= maxRepoExclusionWalkEntries {
		t.Fatalf("fixture is wrong: the entry budget fired (%d entries), so it is not the byte budget under test", ledger.walkVisited)
	}
	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned source tree disclosed nothing")
	}
	if !report.CountIncomplete {
		t.Errorf("the accounting stopped short of the tree and reported an exact count: %+v", *report)
	}
	if report.Files == 0 || report.Files >= dirs*2 {
		t.Errorf("RepoIgnored.Files = %d, want a partial count in (0, %d): what it could afford to attribute", report.Files, dirs*2)
	}
}

// TestOrdinaryNestedIgnoresInAPrunedTreeStayExact is the other direction. A real
// repository's nested `.gitignore` files are hundreds of bytes, so the bound must
// be invisible to them: every descendant still counted, still attributed, and the
// count still exact.
func TestOrdinaryNestedIgnoresInAPrunedTreeStayExact(t *testing.T) {
	t.Parallel()
	const dirs = 10
	root := prunedIgnoreCostTree(t, "*.log\n", dirs)
	ignores, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	if _, _, err := walkWorktreeFiles(t.Context(), root, ignores, func(string) bool { return false }, ledger); err != nil {
		t.Fatal(err)
	}
	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned source tree disclosed nothing")
	}
	if report.CountIncomplete || len(report.Unreadable) != 0 {
		t.Errorf("ordinary nested ignore files made the count a lower bound: CountIncomplete = %t, Unreadable = %v",
			report.CountIncomplete, report.Unreadable)
	}
	if report.Files != dirs*2 {
		t.Errorf("RepoIgnored.Files = %d, want %d (one source and one .gitignore per directory)", report.Files, dirs*2)
	}
}

// TestPrunedTreeGitSwallowAdmitsTheBlindSpot is the uncertainty-reporting hole
// inside the pruned tree.
//
// The accounting drops a descendant Git's own rules already cover, because the
// outer walk would have dropped it too and crediting the repository's rule with
// removing it would cry wolf. "Git would have hidden it anyway" is true of an
// UNTRACKED path only — Git does not apply .gitignore to a tracked file — and
// this mode runs precisely because Git could not be asked which is which. The
// per-file sink records that limitation (noteRepoExclusion) and so does the
// prune itself; inside the pruned tree both swallows stayed silent, so a tracked
// source under `hidden/generated/` left the corpus with the report claiming to
// have seen everything.
//
// FAILS AT RUNTIME on the current head: GitListingUnavailable is false.
func TestPrunedTreeGitSwallowAdmitsTheBlindSpot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A .git entry is what makes this a checkout with tracked files, and it is
	// all the fallback can consult: the walk runs because git cannot list it.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, "visible/stub.go", "package visible\n\nfunc Stub() {}\n")
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, ".gitignore", "generated/\n")
	write(t, root, "hidden/auth.go", "package hidden\n\nfunc ValidateToken() {}\n")
	// Tracked by Git in a real checkout despite the .gitignore line, and removed
	// from the corpus by the .graphignore rule. Unattributable, not absent.
	write(t, root, "hidden/generated/tracked.go", "package generated\n\nfunc Tracked() {}\n")

	ignores, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	if _, _, err := walkWorktreeFiles(t.Context(), root, ignores, func(string) bool { return false }, ledger); err != nil {
		t.Fatal(err)
	}
	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned source tree disclosed nothing")
	}
	if !report.GitListingUnavailable {
		t.Errorf("the pruned tree swallowed a Git-covered subtree without saying Git could not be asked: %+v", *report)
	}
	rendered := string(RenderRepoIgnoreDisclosure(report))
	if !strings.Contains(rendered, "Git could not list this checkout") {
		t.Errorf("text payload does not state the limitation: %q", rendered)
	}
}

// TestPrunedTreeGitSwallowMarksThePositionIncomplete is
// TestPrunedTreeGitSwallowAdmitsTheBlindSpot's counterfactual-position
// counterpart: a Git-hidden DIRECTORY inside an already-pruned tree is skipped
// with its entire (unknown-size) subtree never reaching noteListingCandidate,
// unlike the single-file swallow one level up whose shortfall is exactly one
// path. Before the fix, listingPosition stayed a normal count instead of a
// lower bound, so a later exclusion elsewhere in the same listing could test
// as within the snapshot's file cap only because this subtree's positions
// were never charged against it.
func TestPrunedTreeGitSwallowMarksThePositionIncomplete(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, ".gitignore", "hidden/generated/\n")
	write(t, root, "hidden/keep.go", "package hidden\n\nfunc Keep() {}\n")
	// A tracked-in-a-real-checkout directory the outer .gitignore also hides:
	// the swallow this test targets is the DIRECTORY form (hidden/generated),
	// not the per-file form TestPrunedTreeGitSwallowAdmitsTheBlindSpot covers.
	write(t, root, "hidden/generated/tracked.go", "package generated\n\nfunc Tracked() {}\n")

	ignores, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	stack := newNestedIgnoreStack(root, ignores)
	stack.notePrunedRepoExclusion(ledger, "hidden", func(string) bool { return false })

	if !ledger.positionIncomplete {
		t.Fatal("a Git-hidden subtree of unknown size inside a pruned directory must mark the" +
			" counterfactual listing position incomplete, not leave it looking like an exact count")
	}
	// The consequence that matters: once the position cannot be trusted, no
	// FURTHER exclusion in this listing can be safely tested against the file
	// cap, so beyondListingCap must refuse everything from here on.
	ledger.listingLimit = 1_000_000
	if !ledger.beyondListingCap() {
		t.Fatal("positionIncomplete must make beyondListingCap refuse unconditionally, regardless of" +
			" how far under the cap listingPosition itself claims to be")
	}
}

// TestPrunedExclusionAccountingOrdersCandidatesLikeTheOuterCapDoes reproduces
// the trail finding on readDirBounded's sort key: a pruned directory holding a
// sibling file `a.go` and directory `a/` (containing `a/big.go`) visits `a/`
// FIRST when its children are sorted by bare Name() (`"a" < "a.go"`
// byte-for-byte), while the flat listing capSourceFiles truncates sorts
// `a.go` first (`'.'` is 0x2E, below `'/'` at 0x2F, so "a.go" < "a/big.go").
// At a cap boundary that falls between the two, the two passes must agree on
// which one path crosses it — otherwise the accounting attributes the
// exclusion of a/big.go (a path the cap alone would already exclude) to the
// ignore rule, while silently dropping a.go (a path genuinely inside the cap)
// as though the cap had excluded it.
func TestPrunedExclusionAccountingOrdersCandidatesLikeTheOuterCapDoes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, "hidden/a.go", "package a\n")
	write(t, root, "hidden/a/big.go", "package a\n")

	ignores, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{listingLimit: 1}
	stack := newNestedIgnoreStack(root, ignores)
	stack.notePrunedRepoExclusion(ledger, "hidden", func(string) bool { return false })

	report := ledger.report()
	if report == nil {
		t.Fatal("a pruned tree at the cap boundary disclosed nothing")
	}
	named := make(map[string]bool)
	for _, exclusion := range report.Sample {
		named[exclusion.Path] = true
	}
	if !named["hidden/a.go"] {
		t.Errorf("hidden/a.go is the one candidate inside the file cap in capSourceFiles' own flat order"+
			" ('.' sorts below '/'), so it must be attributed to the %s rule; report = %+v", graphIgnoreFileName, report)
	}
	if named["hidden/a/big.go"] {
		t.Errorf("hidden/a/big.go falls past the file cap in capSourceFiles' own flat order and must not"+
			" be blamed on the %s rule instead; report = %+v", graphIgnoreFileName, report)
	}
}

// TestPrunedLedgerBookkeepingStaysInsideTheListingCap is the lock for the
// "`seen` grows without bound" reading of this ledger.
//
// It does not: `note` is gated on beyondListingCap, so once the counterfactual
// listing passes the snapshot's own file cap nothing further is recorded at all.
// The map is therefore bounded by the cap the snapshot already applies to the
// corpus, not by the repository's path count.
//
// TEETH: delete the `if l.beyondListingCap() { return }` gate in `note` and this
// test fails with len(seen) = 1000000 against the 200000 cap.
func TestPrunedLedgerBookkeepingStaysInsideTheListingCap(t *testing.T) {
	t.Parallel()
	limit := resolveMaxSourceFiles(0)
	ledger := &repoIgnoreLedger{listingLimit: limit}
	const paths = 1_000_000
	for i := range paths {
		ledger.noteListingCandidate()
		ledger.note(RepoExclusion{Path: fmt.Sprintf("hidden/pkg%06d/file.go", i), Source: graphIgnoreFileName, Rule: "hidden/"})
	}
	if len(ledger.seen) > limit {
		t.Errorf("the ledger retained %d paths for a listing capped at %d", len(ledger.seen), limit)
	}
	if ledger.files > limit {
		t.Errorf("Files = %d, past the listing cap of %d", ledger.files, limit)
	}
}
