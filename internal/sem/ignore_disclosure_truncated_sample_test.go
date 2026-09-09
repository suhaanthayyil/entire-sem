package sem

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestPrunedExclusionSampleNamesNothingChosenByFilesystemOrder is the
// determinism half of the read bound.
//
// readDirBounded reads remaining+1 entries and THEN sorts them, so for a
// directory larger than the remaining budget the kept prefix is whatever
// getdents happened to offer first. Those entries are then noted into the
// ledger, and the first maxRepoExclusionSample of them become the paths
// `repo_ignored` DISCLOSES. Two runs over the same repository view — a
// different filesystem, the same directory recreated, a different readdir
// implementation — therefore name different files as examples of what the
// repository's rules removed, while the provider's whole contract is that the
// same view renders the same answer.
//
// The count may be a stated lower bound; the paths may not be a guess. Nothing
// this walk read past a truncation is deterministic, so nothing after it is
// NAMED — the count keeps accruing and SampleTruncated says names were withheld.
func TestPrunedExclusionSampleNamesNothingChosenByFilesystemOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "visible/stub.go", "package visible\n\nfunc Stub() {}\n")
	write(t, root, graphIgnoreFileName, "hidden/\n")
	hidden := filepath.Join(root, "hidden")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	// More entries than the budget below leaves, which is what makes the read
	// take a filesystem-ordered prefix. Spending the budget in the ledger rather
	// than writing 20,000 files keeps this test cheap; the code path is the one
	// the runaway tree takes.
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(hidden, "f"+strconv.Itoa(i)+".go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	matcher, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{walkVisited: maxRepoExclusionWalkEntries - 3}
	if _, _, err := walkWorktreeFiles(t.Context(), root, matcher, nil, ledger); err != nil {
		t.Fatal(err)
	}
	report := ledger.report()
	if report == nil {
		t.Fatalf("a %s directory rule pruned a tree and the ledger disclosed nothing", graphIgnoreFileName)
	}
	if !report.CountIncomplete {
		t.Fatalf("CountIncomplete = false over a directory the read could not finish (Files = %d)", report.Files)
	}
	if len(report.Sample) != 0 {
		t.Errorf("the disclosure named %d path(s) — %v — out of a directory whose read stopped at a"+
			" filesystem-ordered prefix; which files those are is a property of the filesystem, not of"+
			" the repository, so the same view discloses different examples on two machines",
			len(report.Sample), report.Sample)
	}
	if report.Files == 0 {
		t.Errorf("Files = 0; withholding NAMES chosen by filesystem order must not also withhold the" +
			" count, which stays a stated lower bound")
	}
	if !report.SampleTruncated {
		t.Errorf("SampleTruncated = false while %d excluded files went unnamed; a reader cannot tell an"+
			" empty sample from a repository that excluded nothing", report.Files)
	}
}
