package sem

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// walkHidingTree builds a NON-GIT directory — the listing mode `walkWorktreeFiles`
// serves, because `git ls-files` cannot enumerate it — whose real implementation
// sits in a subdirectory that one `.graphignore` line prunes.
func walkHidingTree(t *testing.T, rule string) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "hidden/auth.go", `package hidden

// ValidateToken checks the bearer token presented on a request.
func ValidateToken(token string) bool { return len(token) == 64 }
`)
	write(t, root, "visible/auth_stub.go", `package visible

// ValidateTokenStub is the permissive stand-in.
func ValidateTokenStub(token string) bool { return token != "" }
`)
	write(t, root, graphIgnoreFileName, rule)
	return root
}

// TestSearchDisclosesWalkFallbackDirectoryPrune is the directory-prune blind spot.
//
// A rule naming a FILE is disclosed by the walk fallback, but a rule naming a
// DIRECTORY makes WalkDir return SkipDir before any child is ever tested, so an
// entire source tree left the corpus with repo_ignored == nil. It FAILS AT
// RUNTIME on the current head: the subtree disappears and the payload discloses
// nothing.
func TestSearchDisclosesWalkFallbackDirectoryPrune(t *testing.T) {
	t.Parallel()
	root := walkHidingTree(t, "hidden/\n")
	response, err := SearchRepository(t.Context(), root, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Precondition: the prune works, so there is something to disclose.
	for _, result := range response.Results {
		if result.FilePath == "hidden/auth.go" {
			t.Fatalf("fixture is wrong: %s did not prune hidden/", graphIgnoreFileName)
		}
	}
	if response.RepoIgnored == nil {
		t.Fatalf("a %s directory rule pruned a whole source tree and the response disclosed nothing", graphIgnoreFileName)
	}
	if response.RepoIgnored.Files != 1 {
		t.Errorf("RepoIgnored.Files = %d, want 1 (the pruned tree holds one source file)", response.RepoIgnored.Files)
	}
	if response.Stats.RepoIgnoredFiles != response.RepoIgnored.Files {
		t.Errorf("Stats.RepoIgnoredFiles = %d, want %d", response.Stats.RepoIgnoredFiles, response.RepoIgnored.Files)
	}
	if len(response.RepoIgnored.Sample) != 1 || response.RepoIgnored.Sample[0].Path != "hidden/auth.go" {
		t.Errorf("Sample = %+v, want hidden/auth.go — the actionable half is the path", response.RepoIgnored.Sample)
	}
}

// TestWalkFallbackKeepsGitAppliedDirectoryPrunesQuiet is the kind-(b) guard on the
// fix above: the ordinary build-output directory every .gitignore excludes must
// not start printing paths. Only a prune Git would not have made itself is worth
// a reader's attention.
func TestWalkFallbackKeepsGitAppliedDirectoryPrunesQuiet(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "build/generated.go", "package build\n\nfunc Generated() {}\n")
	write(t, root, "app/main.go", "package app\n\nfunc Main() {}\n")
	write(t, root, ".gitignore", "build/\n")
	response, err := SearchRepository(t.Context(), root, "test", "generated", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored != nil {
		t.Fatalf("a .gitignore directory prune Git applies itself must stay quiet, got %+v", response.RepoIgnored)
	}
}

// TestDisclosureSkipsPathsTheSnapshotWouldNotRead is the phantom-exclusion
// finding. `git ls-files` lists index entries, including a file whose deletion is
// not staged, and the snapshot never reads one of those. Attributing it to the
// repository's ignore rules claims a file was hidden that was not there to hide.
//
// FAILS AT RUNTIME on the current head: the ledger is written before the
// eligibility check, so the deleted path is reported as removed by .graphignore.
func TestDisclosureSkipsPathsTheSnapshotWouldNotRead(t *testing.T) {
	t.Parallel()
	repo := hidingRepo(t, graphIgnoreFileName)
	// The ignored file leaves the working tree; Git still lists the index entry.
	if err := os.Remove(filepath.Join(repo, "internal", "auth", "auth.go")); err != nil {
		t.Fatal(err)
	}
	response, err := SearchRepository(t.Context(), repo, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored != nil {
		t.Fatalf("no file was removed from the corpus by an ignore rule — auth.go was already gone from the"+
			" working tree — yet the response disclosed %+v", response.RepoIgnored)
	}
	if response.Stats.RepoIgnoredFiles != 0 {
		t.Errorf("Stats.RepoIgnoredFiles = %d, want 0", response.Stats.RepoIgnoredFiles)
	}
}

// TestRepoIgnoreDisclosureComesFirst pins the placement, not a preference: every
// renderer that caps the diagnostics it prints takes them from the head of the
// list, so a disclosure appended behind three unrelated warnings loses the path
// it exists to name.
func TestRepoIgnoreDisclosureComesFirst(t *testing.T) {
	t.Parallel()
	existing := []ProviderWarning{{Code: "W_ONE"}, {Code: "W_TWO"}, {Code: "W_THREE"}}
	got := withRepoIgnoreDisclosure(existing, &RepoIgnoreReport{
		Files:   1,
		Sources: []RepoIgnoreSource{{File: graphIgnoreFileName, Files: 1}},
		Sample:  []RepoExclusion{{Path: "internal/auth/auth.go", Source: graphIgnoreFileName, Rule: "internal/auth/auth.go"}},
	})
	if len(got) != 4 {
		t.Fatalf("warnings = %d, want 4", len(got))
	}
	if got[0].Code != repoIgnoreDisclosureCode {
		t.Fatalf("warnings[0] = %q, want the disclosure first so a capped renderer still prints it", got[0].Code)
	}
	if got[0].FilePath != "internal/auth/auth.go" {
		t.Errorf("FilePath = %q, want the excluded path", got[0].FilePath)
	}
	for i, code := range []string{"W_ONE", "W_TWO", "W_THREE"} {
		if got[i+1].Code != code {
			t.Errorf("warnings[%d] = %q, want %q — the existing order must survive", i+1, got[i+1].Code, code)
		}
	}
	if existing[0].Code != "W_ONE" {
		t.Errorf("the caller's slice was mutated: %+v", existing)
	}
}

// TestWalkPruneDisclosureOmitsWhatTheScanDropsAnyway is the over-disclosure half
// of the prune accounting. The outer walk never puts a vendored tree or a
// lockfile in the corpus, so a `.graphignore` rule over their parent did not
// remove them from it — naming them as removed by the repository's rules blames a
// committed line for content no line hid, and buries the one path that matters
// (here `hidden/auth.go`, which the ten-path sample could not even reach).
//
// The other direction is asserted in the same test: ordinary source nested deeper
// inside the pruned tree, behind a nested .gitignore with no opinion about it,
// must still be counted and named. Fixing over-disclosure by disclosing less of
// the real thing would be the worse regression.
//
// FAILS AT RUNTIME on the current head: Files = 5 and the sample leads with
// node_modules.
func TestWalkPruneDisclosureOmitsWhatTheScanDropsAnyway(t *testing.T) {
	t.Parallel()
	root := walkHidingTree(t, "hidden/\n")
	write(t, root, "hidden/pkg/deep/service.go", `package deep

// ValidateTokenDeep is first-party source nested inside the pruned tree.
func ValidateTokenDeep(token string) bool { return token != "" }
`)
	write(t, root, "hidden/node_modules/dep/index.js", "module.exports = function () { return 1; };\n")
	write(t, root, "hidden/vendor/lib/lib.go", "package lib\n\nfunc Lib() {}\n")
	write(t, root, "hidden/yarn.lock", "# lockfile\n")
	response, err := SearchRepository(t.Context(), root, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored == nil {
		t.Fatalf("a %s directory rule pruned a source tree and the response disclosed nothing", graphIgnoreFileName)
	}
	disclosed := make(map[string]bool, len(response.RepoIgnored.Sample))
	for _, exclusion := range response.RepoIgnored.Sample {
		disclosed[exclusion.Path] = true
	}
	for _, path := range []string{"hidden/auth.go", "hidden/pkg/deep/service.go"} {
		if !disclosed[path] {
			t.Errorf("%s left the corpus with the prune and was not named; sample = %+v", path, response.RepoIgnored.Sample)
		}
	}
	for _, path := range []string{
		"hidden/node_modules/dep/index.js",
		"hidden/vendor/lib/lib.go",
		"hidden/yarn.lock",
	} {
		if disclosed[path] {
			t.Errorf("%s is dropped by the scan itself, so no ignore rule removed it from the corpus", path)
		}
	}
	if response.RepoIgnored.Files != 2 {
		t.Errorf("RepoIgnored.Files = %d, want 2 (the two source files the rule actually removed)", response.RepoIgnored.Files)
	}
	if response.Stats.RepoIgnoredFiles != response.RepoIgnored.Files {
		t.Errorf("Stats.RepoIgnoredFiles = %d, want %d", response.Stats.RepoIgnoredFiles, response.RepoIgnored.Files)
	}
}

// TestWalkPruneDisclosureDoesNotEnterGitExcludedSubtrees proves the accounting
// walk stops where the outer walk stops instead of crawling a Git-excluded tree
// to filter it file by file.
//
// The unreadable directory is the probe: it can only be reached by descending
// into `hidden/generated/`, which `.gitignore` excludes and the outer walk prunes
// wholesale. Reaching it also corrupted the disclosure — the count came back
// `count_incomplete` over a subtree that was never part of the count, i.e. the
// report understated itself about content it had no business counting.
//
// FAILS AT RUNTIME on the current head: CountIncomplete is true and Unreadable
// names hidden/generated/deep.
func TestWalkPruneDisclosureDoesNotEnterGitExcludedSubtrees(t *testing.T) {
	t.Parallel()
	if !unreadableDirectoriesHold(t) {
		t.Skip("this user/filesystem can read a 0o000 directory, so the probe cannot be built")
	}
	root := walkHidingTree(t, "hidden/\n")
	write(t, root, ".gitignore", "generated/\n")
	write(t, root, "hidden/generated/deep/built.go", "package deep\n\nfunc Built() {}\n")
	unreadable := filepath.Join(root, "hidden", "generated", "deep")
	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o755) })
	response, err := SearchRepository(t.Context(), root, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored == nil {
		t.Fatalf("a %s directory rule pruned a source tree and the response disclosed nothing", graphIgnoreFileName)
	}
	if response.RepoIgnored.CountIncomplete || len(response.RepoIgnored.Unreadable) != 0 {
		t.Errorf("the accounting walk descended into a Git-excluded tree: CountIncomplete = %t, Unreadable = %v",
			response.RepoIgnored.CountIncomplete, response.RepoIgnored.Unreadable)
	}
	if response.RepoIgnored.Files != 1 {
		t.Errorf("RepoIgnored.Files = %d, want 1 (hidden/auth.go; the generated tree is Git's own exclusion)",
			response.RepoIgnored.Files)
	}
}

// TestPrunedExclusionAccountingIsBounded is the give-back-the-prune finding.
//
// SkipDir is what makes an ignored directory cost nothing: nothing under it is
// ever stat'd. Enumerating that tree to disclose what it removed hands the cost
// back, and the tree's size is set by the repository whose rules the report
// exists to expose — so a committed rule over a very large tree buys a
// filesystem crawl on EVERY search, and the ignore file a project added to make
// searching cheap stops doing that.
//
// FAILS AT RUNTIME on the current head: the accounting walks all
// maxRepoExclusionWalkEntries+ entries and reports an exact count over them, with
// nothing bounding the work.
func TestPrunedExclusionAccountingIsBounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "visible/stub.go", "package visible\n\nfunc Stub() {}\n")
	write(t, root, graphIgnoreFileName, "hidden/\n")
	// One entry past the budget, so the bound is the only thing that can stop it.
	hidden := filepath.Join(root, "hidden")
	for shard := range 4 {
		dir := filepath.Join(hidden, "s"+strconv.Itoa(shard))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := range maxRepoExclusionWalkEntries/4 + 1 {
			if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)+".go"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	response, err := SearchRepository(t.Context(), root, "test", "stub", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored == nil {
		t.Fatalf("a %s directory rule pruned a tree and the response disclosed nothing", graphIgnoreFileName)
	}
	if response.RepoIgnored.Files > maxRepoExclusionWalkEntries {
		t.Errorf("RepoIgnored.Files = %d, want at most %d — the accounting walk is unbounded, so a"+
			" repository sets how much filesystem every search crawls",
			response.RepoIgnored.Files, maxRepoExclusionWalkEntries)
	}
	// Bounded is only honest if the payload stops calling the short count exact.
	if !response.RepoIgnored.CountIncomplete {
		t.Errorf("RepoIgnored.CountIncomplete = false with a count the walk could not finish; a bounded" +
			" walk that still claims an exact number understates in silence")
	}
	if len(response.RepoIgnored.Unreadable) != 0 {
		t.Errorf("Unreadable = %v, want empty — nothing here was unreadable, it was merely large",
			response.RepoIgnored.Unreadable)
	}
	var codes []string
	for _, failure := range response.PartialFailures {
		codes = append(codes, failure.Code)
	}
	if !slices.Contains(codes, repoIgnoreTruncatedCode) {
		t.Errorf("partial failure codes = %v, want %s", codes, repoIgnoreTruncatedCode)
	}
	if slices.Contains(codes, repoIgnoreIncompleteCode) {
		t.Errorf("partial failure codes = %v, must not claim %s — nothing was unreadable",
			codes, repoIgnoreIncompleteCode)
	}
	// The text payload must not render an empty list of unreadable paths.
	rendered := string(RenderRepoIgnoreDisclosure(response.RepoIgnored))
	if strings.Contains(rendered, "could not be read") {
		t.Errorf("text disclosure blames an unreadable path for a tree that was merely large:\n%s", rendered)
	}
	if !strings.Contains(rendered, "LOWER BOUND") {
		t.Errorf("text disclosure prints a short count as if it were exact:\n%s", rendered)
	}
	if strings.Contains(rendered, "the count is exact") {
		t.Errorf("text disclosure calls a short count exact and a lower bound in the same payload:\n%s", rendered)
	}
}

// TestOrdinaryPrunedExclusionStaysExact is the kind-(b) guard on the bound: a
// tree of the size a real repository excludes must keep an EXACT count, name its
// paths, and raise no partial failure. It passes before and after the bound.
func TestOrdinaryPrunedExclusionStaysExact(t *testing.T) {
	t.Parallel()
	root := walkHidingTree(t, "hidden/\n")
	response, err := SearchRepository(t.Context(), root, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.RepoIgnored == nil {
		t.Fatalf("a %s directory rule pruned a source tree and the response disclosed nothing", graphIgnoreFileName)
	}
	if response.RepoIgnored.Files != 1 || response.RepoIgnored.CountIncomplete {
		t.Errorf("Files = %d, CountIncomplete = %t; want 1 and false — a one-file tree is nowhere near the"+
			" walk bound and must still be counted exactly",
			response.RepoIgnored.Files, response.RepoIgnored.CountIncomplete)
	}
	for _, failure := range response.PartialFailures {
		if failure.Code == repoIgnoreTruncatedCode || failure.Code == repoIgnoreIncompleteCode {
			t.Errorf("an ordinary pruned tree raised %s; the bound must not turn every disclosure into a"+
				" shortfall report", failure.Code)
		}
	}
	if strings.Contains(string(RenderRepoIgnoreDisclosure(response.RepoIgnored)), "LOWER BOUND") {
		t.Errorf("an exactly counted disclosure rendered itself as a lower bound")
	}
}

// TestPrunedExclusionAccountingBoundsWhatItReads is the give-back-the-prune
// finding one layer in: the entry budget bounded the entries the accounting
// VISITED, and filepath.WalkDir reads and sorts a directory IN FULL before the
// first of them reaches the callback. The reported count was bounded; the work
// behind it was not, and the repository still set how much filesystem every
// search crawls.
//
// Reproduced at runtime on the head that carried the entry budget, one flat
// pruned directory, same search, same reported count:
//
//	fanout=20000  elapsed=112ms  Files=19998 CountIncomplete=true
//	fanout=200000 elapsed=468ms  Files=19998 CountIncomplete=true
//
// Ten times the tree for 4.2x the wall clock of every search, with a payload
// that could not tell the two apart. That defect is visible only as cost, so the
// assertion here is on the entries read rather than on elapsed time, which is
// why this test counts them instead of timing them.
func TestPrunedExclusionAccountingBoundsWhatItReads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "visible/stub.go", "package visible\n\nfunc Stub() {}\n")
	write(t, root, graphIgnoreFileName, "hidden/\n")
	hidden := filepath.Join(root, "hidden")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	// Twice the budget in ONE directory: nothing past the budget can ever be
	// visited, so every entry past it that is read is cost the report cannot use.
	for i := range maxRepoExclusionWalkEntries * 2 {
		if err := os.WriteFile(filepath.Join(hidden, "f"+strconv.Itoa(i)+".go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	matcher, err := loadWorktreeIgnoreMatcher(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &repoIgnoreLedger{}
	if _, _, err := walkWorktreeFiles(t.Context(), root, matcher, nil, ledger); err != nil {
		t.Fatal(err)
	}
	// +1: the read distinguishes "the whole directory" from "as much of it as the
	// budget allows" by asking for one entry more than it can pay for.
	if got, want := ledger.walkDirentsRead(), maxRepoExclusionWalkEntries+1; got > want {
		t.Errorf("the accounting read %d directory entries against a budget of %d; the bound holds for"+
			" the count and not for the crawl behind it, so the repository still sets how much"+
			" filesystem every search reads", got, want)
	}
	report := ledger.report()
	if report == nil {
		t.Fatalf("a %s directory rule pruned a tree and the ledger disclosed nothing", graphIgnoreFileName)
	}
	// Bounding the read must not turn the disclosure into a silent shortfall.
	if !report.CountIncomplete {
		t.Errorf("CountIncomplete = false over a tree the read could not finish; a short count that"+
			" calls itself exact is the silence this disclosure exists to end (Files = %d)", report.Files)
	}
	if len(report.Unreadable) != 0 {
		t.Errorf("Unreadable = %v, want empty — nothing here was unreadable, it was merely large",
			report.Unreadable)
	}
	if report.Files == 0 || report.Files > maxRepoExclusionWalkEntries {
		t.Errorf("Files = %d, want between 1 and %d — bounding the READ must still name what it did"+
			" visit, not collapse the disclosure to nothing", report.Files, maxRepoExclusionWalkEntries)
	}
}
