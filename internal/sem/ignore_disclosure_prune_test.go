package sem

import (
	"fmt"
	"testing"
)

// prunedTreePaths runs the walk fallback over root and returns the paths the
// disclosure named, plus the report itself.
func prunedTreePaths(t *testing.T, root string) (*RepoIgnoreReport, map[string]bool) {
	t.Helper()
	response, err := SearchRepository(t.Context(), root, "test", "bearer token validation", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     5,
	})
	if err != nil {
		t.Fatal(err)
	}
	named := make(map[string]bool)
	if response.RepoIgnored == nil {
		return nil, named
	}
	for _, exclusion := range response.RepoIgnored.Sample {
		named[exclusion.Path] = true
	}
	return response.RepoIgnored, named
}

// TestPrunedTreeDisclosureSkipsWhatGitsOwnRulesAlreadyHide is the nested-.gitignore
// blind spot in the directory-prune disclosure.
//
// The per-file sink asks `ignoredByGit` before it records anything, so ordinary
// build output never reaches the payload. The prune enumerator asks that question
// about the DIRECTORY only and then records every regular descendant unseen, so a
// `.gitignore` INSIDE the pruned tree — a rule Git applies itself — stops being
// consulted. Real `git` on the same fixture lists only `hidden/.gitignore`,
// `hidden/auth.go`, `hidden/sub/.gitignore` and `hidden/sub/keep.go`
// (`git check-ignore -v` blames `hidden/.gitignore:1:*.gen.go` for
// `hidden/bundle.gen.go`), so naming the generated files as source the repository
// hid is a claim Git contradicts.
//
// FAILS AT RUNTIME on the current head: both generated files are disclosed.
func TestPrunedTreeDisclosureSkipsWhatGitsOwnRulesAlreadyHide(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, "hidden/.gitignore", "*.gen.go\n")
	write(t, root, "hidden/auth.go", "package hidden\n\nfunc ValidateToken(token string) bool { return len(token) == 64 }\n")
	write(t, root, "hidden/bundle.gen.go", "package hidden\n\nfunc GeneratedBundle() string { return \"\" }\n")
	write(t, root, "hidden/sub/.gitignore", "*.tmp.go\n")
	write(t, root, "hidden/sub/keep.go", "package sub\n\nfunc Keep() {}\n")
	write(t, root, "hidden/sub/drop.tmp.go", "package sub\n\nfunc Drop() {}\n")
	write(t, root, "visible/auth_stub.go", "package visible\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")

	report, named := prunedTreePaths(t, root)
	if report == nil {
		t.Fatalf("the %s prune removed real source and the response disclosed nothing", graphIgnoreFileName)
	}
	for _, path := range []string{"hidden/auth.go", "hidden/sub/keep.go"} {
		if !named[path] {
			t.Errorf("%s left the corpus because of %s and was not disclosed: %+v", path, graphIgnoreFileName, report.Sample)
		}
	}
	for _, path := range []string{"hidden/bundle.gen.go", "hidden/sub/drop.tmp.go"} {
		if named[path] {
			t.Errorf("%s is hidden by a nested .gitignore Git applies itself, so %s did not remove it —"+
				" disclosing it prints build output as repository-hidden source: %+v", path, graphIgnoreFileName, report.Sample)
		}
	}
	if report.Files != len(report.Sample) {
		t.Errorf("Files = %d but Sample holds %d under the cap — the count and the list disagree", report.Files, len(report.Sample))
	}
}

// TestPrunedTreeDisclosureKeepsUncoveredDescendants is the kind-(b) guard on the
// fix above and must pass BEFORE and AFTER it: a nested .gitignore that covers
// nothing in the tree must not suppress the disclosure of anything.
func TestPrunedTreeDisclosureKeepsUncoveredDescendants(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	write(t, root, "hidden/.gitignore", "*.log\n")
	write(t, root, "hidden/auth.go", "package hidden\n\nfunc ValidateToken(token string) bool { return len(token) == 64 }\n")
	write(t, root, "hidden/sub/deep.go", "package sub\n\nfunc Deep() {}\n")
	write(t, root, "visible/auth_stub.go", "package visible\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")

	report, named := prunedTreePaths(t, root)
	if report == nil {
		t.Fatalf("a %s prune removed two source files and disclosed nothing", graphIgnoreFileName)
	}
	for _, path := range []string{"hidden/auth.go", "hidden/sub/deep.go"} {
		if !named[path] {
			t.Errorf("%s is covered by no rule Git applies and must stay disclosed: %+v", path, report.Sample)
		}
	}
}

// TestPrunedTreeDisclosureCountsEveryDescendant pins the exactness the field
// promises. RepoIgnoreReport.Files documents itself as "the exact number of listed
// paths excluded, even when Sample is capped", and the coverage line renders it as
// a count of files removed; a traversal that stops early reports a repository that
// hid 600 files as one that hid 512.
//
// FAILS AT RUNTIME on the current head: Files == 512.
func TestPrunedTreeDisclosureCountsEveryDescendant(t *testing.T) {
	t.Parallel()
	const descendants = 600
	root := t.TempDir()
	write(t, root, graphIgnoreFileName, "hidden/\n")
	for i := range descendants {
		write(t, root, fmt.Sprintf("hidden/f%03d.go", i), fmt.Sprintf("package hidden\n\nfunc F%03d() {}\n", i))
	}
	write(t, root, "visible/auth_stub.go", "package visible\n\nfunc ValidateTokenStub(token string) bool { return token != \"\" }\n")

	report, _ := prunedTreePaths(t, root)
	if report == nil {
		t.Fatalf("a %s prune removed %d files and disclosed nothing", graphIgnoreFileName, descendants)
	}
	if report.Files != descendants {
		t.Errorf("Files = %d, want %d — the field promises an exact count, so a bounded traversal understates"+
			" the exclusion by %d files", report.Files, descendants, descendants-report.Files)
	}
	if len(report.Sources) != 1 || report.Sources[0].Files != descendants {
		t.Errorf("Sources = %+v, want one %s entry counting %d", report.Sources, graphIgnoreFileName, descendants)
	}
	if len(report.Sample) != maxRepoExclusionSample || !report.SampleTruncated {
		t.Errorf("Sample = %d paths (truncated=%v), want the list capped at %d and flagged — only the LIST is bounded",
			len(report.Sample), report.SampleTruncated, maxRepoExclusionSample)
	}
}
