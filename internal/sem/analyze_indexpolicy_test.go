package sem

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-graph/internal/gitutil"
)

// The semantic diff must report what happened to the GRAPH, not what happened to
// a path. These tests pin that equivalence directly: whatever the diff says
// entered or left the index is checked against a snapshot of the same repository,
// so neither side can drift without a failure here.

func initDiffPolicyRepo(t *testing.T, repo string) {
	t.Helper()
	git(t, repo, "init")
	git(t, repo, "config", "user.name", "Entire Graph Test")
	git(t, repo, "config", "user.email", "graph@example.com")
}

// snapshotSymbolNames is the graph's own answer for one file at the repository's
// current state, which is what the diff's additions and removals have to match.
func snapshotSymbolNames(t *testing.T, repo, file string) map[string]struct{} {
	t.Helper()
	snapshot, err := BuildProviderSnapshot(context.Background(), repo, "test")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]struct{}{}
	for _, symbol := range snapshot.Symbols {
		if filepath.ToSlash(symbol.FilePath) == file {
			names[symbol.Name] = struct{}{}
		}
	}
	return names
}

func changeNamesByType(file FileChange, changeType string) map[string]struct{} {
	names := map[string]struct{}{}
	for _, change := range file.Changes {
		if change.Type == changeType {
			names[change.Name] = struct{}{}
		}
	}
	return names
}

func sameNames(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for name := range left {
		if _, ok := right[name]; !ok {
			return false
		}
	}
	return true
}

func onlyFile(t *testing.T, result Result) FileChange {
	t.Helper()
	if len(result.Files) != 1 {
		t.Fatalf("files = %#v, want exactly one", result.Files)
	}
	return result.Files[0]
}

// TestAnalyzeGitRangeIndexedToIgnoredRenameRemovesOldSymbols covers a rename OUT
// of the graph. The base snapshot holds the old path's symbols and the head
// snapshot holds nothing, so the delta a consumer needs is a removal of every one
// of them. Deciding from the destination path alone discarded the whole entry and
// left those symbols in the consumer's index forever.
func TestAnalyzeGitRangeIndexedToIgnoredRenameRemovesOldSymbols(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, ".graphignore", "vendored/\n")
	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 1 }\n\nfunc Also() int { return 9 }\n\nfunc Third() int { return 3 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	base := rev(t, repo, "HEAD")

	wantRemoved := snapshotSymbolNames(t, repo, "keep/k.go")
	if len(wantRemoved) == 0 {
		t.Fatal("fixture is wrong: the snapshot indexes no symbol in keep/k.go")
	}

	if err := os.Remove(filepath.Join(repo, "keep", "k.go")); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "vendored/k.go", "package vendored\n\nfunc Keep() int { return 111 }\n\nfunc Also() int { return 9 }\n\nfunc Third() int { return 3 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "move the tree out of the graph")
	head := rev(t, repo, "HEAD")

	if got := snapshotSymbolNames(t, repo, "vendored/k.go"); len(got) != 0 {
		t.Fatalf("fixture is wrong: the snapshot indexed an ignored file: %#v", got)
	}

	result, err := AnalyzeGitRange(context.Background(), repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := onlyFile(t, result)
	if file.Path != "keep/k.go" || file.Status != "D" {
		t.Fatalf("file = %#v, want a deletion of keep/k.go", file)
	}
	if file.OldPath != "" {
		t.Fatalf("file = %#v, want no rename provenance: nothing moved inside the graph", file)
	}
	if removed := changeNamesByType(file, "removed"); !sameNames(removed, wantRemoved) {
		t.Fatalf("removed = %#v, want every symbol the base snapshot held: %#v", removed, wantRemoved)
	}
	if len(file.Changes) != len(wantRemoved) {
		t.Fatalf("changes = %#v, want only removals", file.Changes)
	}
}

// TestAnalyzeGitRangeIgnoredToIndexedRenameAddsEveryHeadSymbol covers the mirror
// crossing, INTO the graph. No snapshot contains the base path, so comparing
// against it reported a body change for a file the graph never held and never
// named the head file's other symbols at all.
func TestAnalyzeGitRangeIgnoredToIndexedRenameAddsEveryHeadSymbol(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, ".graphignore", "vendored/\n")
	write(t, repo, "anchor.go", "package root\n\nfunc Anchor() int { return 0 }\n")
	write(t, repo, "vendored/g.go", "package vendored\n\nfunc Gen() int { return 1 }\n\nfunc GenTwo() int { return 2 }\n\nfunc GenThree() int { return 3 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	base := rev(t, repo, "HEAD")

	if got := snapshotSymbolNames(t, repo, "vendored/g.go"); len(got) != 0 {
		t.Fatalf("fixture is wrong: the snapshot indexed an ignored file: %#v", got)
	}

	if err := os.Remove(filepath.Join(repo, "vendored", "g.go")); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "keep/g.go", "package keep\n\nfunc Gen() int { return 111 }\n\nfunc GenTwo() int { return 2 }\n\nfunc GenThree() int { return 3 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "move the tree into the graph")
	head := rev(t, repo, "HEAD")

	wantAdded := snapshotSymbolNames(t, repo, "keep/g.go")
	if len(wantAdded) < 2 {
		t.Fatalf("fixture is wrong: want several symbols in keep/g.go, got %#v", wantAdded)
	}

	result, err := AnalyzeGitRange(context.Background(), repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	file := onlyFile(t, result)
	if file.Path != "keep/g.go" || file.Status != "A" {
		t.Fatalf("file = %#v, want an addition of keep/g.go", file)
	}
	if file.OldPath != "" {
		t.Fatalf("file = %#v, want no provenance from a path no snapshot holds", file)
	}
	if added := changeNamesByType(file, "added"); !sameNames(added, wantAdded) {
		t.Fatalf("added = %#v, want every symbol the head snapshot holds: %#v", added, wantAdded)
	}
	if len(file.Changes) != len(wantAdded) {
		t.Fatalf("changes = %#v, want only additions", file.Changes)
	}
}

// TestAnalyzeGitRangeSubtreeRangeAppliesNoIgnorePolicy is the over-rejection
// guard for the root-relative gate. A subtree range names its files relative to
// that subtree, so a root-anchored rule matched the wrong names and silently
// dropped a file the graph does index.
func TestAnalyzeGitRangeSubtreeRangeAppliesNoIgnorePolicy(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	// Anchored at the repository root: it names top.go, never sub/top.go.
	write(t, repo, ".graphignore", "/top.go\n")
	write(t, repo, "top.go", "package top\n\nfunc Top() int { return 1 }\n")
	write(t, repo, "sub/top.go", "package sub\n\nfunc Top() int { return 1 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")

	write(t, repo, "top.go", "package top\n\nfunc Top() int { return 2 }\n")
	write(t, repo, "sub/top.go", "package sub\n\nfunc Top() int { return 2 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "edit both")

	if got := snapshotSymbolNames(t, repo, "sub/top.go"); len(got) == 0 {
		t.Fatal("fixture is wrong: the snapshot must index sub/top.go")
	}

	subtree, err := AnalyzeGitRange(context.Background(), repo, "HEAD~1:sub", "HEAD:sub", nil)
	if err != nil {
		t.Fatal(err)
	}
	file := onlyFile(t, subtree)
	if file.Path != "top.go" {
		t.Fatalf("subtree range file = %#v, want the subtree-relative top.go", file)
	}

	// The same rule still applies for the ordinary root-relative range, where the
	// names it was written against are the names Git emits.
	whole, err := AnalyzeGitRange(context.Background(), repo, "HEAD~1", "HEAD", nil)
	if err != nil {
		t.Fatal(err)
	}
	file = onlyFile(t, whole)
	if file.Path != "sub/top.go" {
		t.Fatalf("root range file = %#v, want only sub/top.go", file)
	}
}

// TestAnalyzeGitRangeTreePeeledRangeStillAppliesIgnorePolicy pins the other side
// of the gate: <commit-ish>^{tree} is a root tree, so its names ARE root
// relative and the policy must still apply.
func TestAnalyzeGitRangeTreePeeledRangeStillAppliesIgnorePolicy(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, ".graphignore", "vendored/\n")
	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 1 }\n")
	write(t, repo, "vendored/gen.go", "package vendored\n\nfunc Gen() int { return 1 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")

	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 2 }\n")
	write(t, repo, "vendored/gen.go", "package vendored\n\nfunc Gen() int { return 2 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "edit both")

	result, err := AnalyzeGitRange(context.Background(), repo, "HEAD~1^{tree}", "HEAD^{tree}", nil)
	if err != nil {
		t.Fatal(err)
	}
	file := onlyFile(t, result)
	if file.Path != "keep/k.go" {
		t.Fatalf("file = %#v, want only keep/k.go", file)
	}
}

// TestAnalyzeGitRangeHonorsVendoredTrees covers the other filter the committed
// snapshot applies. A tracked vendor/ tree is the canonical case: no snapshot
// contains it, so no diff may report entity changes in it.
func TestAnalyzeGitRangeHonorsVendoredTrees(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 1 }\n")
	write(t, repo, "vendor/v.go", "package vendorpkg\n\nfunc V() int { return 1 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	base := rev(t, repo, "HEAD")

	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 2 }\n")
	write(t, repo, "vendor/v.go", "package vendorpkg\n\nfunc V() int { return 2 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "edit both")
	head := rev(t, repo, "HEAD")

	if got := snapshotSymbolNames(t, repo, "vendor/v.go"); len(got) != 0 {
		t.Fatalf("fixture is wrong: the snapshot indexed a vendored file: %#v", got)
	}

	result, err := AnalyzeGitRange(context.Background(), repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if file := onlyFile(t, result); file.Path != "keep/k.go" {
		t.Fatalf("file = %#v, want only keep/k.go", file)
	}
}

// TestAnalyzeGitRangeReportsReincludedVendorTree is the over-rejection guard for
// the vendored-tree filter. A project that re-includes part of a vendored tree
// means to keep it, the snapshot indexes it, and so the diff must report it.
func TestAnalyzeGitRangeReportsReincludedVendorTree(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, ".gitignore", "vendor/*\n!vendor/rootpkg/\n!vendor/rootpkg/**\n")
	write(t, repo, "vendor/rootpkg/root.go", "package rootpkg\n\nfunc Root() int { return 1 }\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "initial")
	base := rev(t, repo, "HEAD")

	write(t, repo, "vendor/rootpkg/root.go", "package rootpkg\n\nfunc Root() int { return 2 }\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "edit the re-included tree")
	head := rev(t, repo, "HEAD")

	if got := snapshotSymbolNames(t, repo, "vendor/rootpkg/root.go"); len(got) == 0 {
		t.Fatal("fixture is wrong: the snapshot must index a re-included vendor tree")
	}

	result, err := AnalyzeGitRange(context.Background(), repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	if file := onlyFile(t, result); file.Path != "vendor/rootpkg/root.go" {
		t.Fatalf("file = %#v, want the re-included vendor path reported", file)
	}
}

// TestAdmitChangedFilesRewritesIndexCrossings covers the rewrite table directly,
// including the copy case Git is not currently asked to detect: a copy's source
// still exists, so an unindexed destination must never be reported as a deletion
// of it.
func TestAdmitChangedFilesRewritesIndexCrossings(t *testing.T) {
	indexed := diffIndexPolicy{ignores: ignoreMatcher{}, vendorRules: ignoreMatcher{}, enabled: true}
	var ignoredAll ignoreMatcher
	if err := ignoredAll.loadContent("*\n", false, repoIgnoreOrigin(".gitignore")); err != nil {
		t.Fatal(err)
	}
	unindexed := diffIndexPolicy{ignores: ignoredAll, vendorRules: ignoreMatcher{}, enabled: true}

	tests := []struct {
		name string
		base diffIndexPolicy
		head diffIndexPolicy
		in   gitutil.ChangedFile
		want []gitutil.ChangedFile
	}{
		{
			name: "indexed both sides is untouched",
			base: indexed, head: indexed,
			in:   gitutil.ChangedFile{Status: "R", OldPath: "a.go", Path: "b.go"},
			want: []gitutil.ChangedFile{{Status: "R", OldPath: "a.go", Path: "b.go"}},
		},
		{
			name: "rename out of the index becomes a deletion of the old path",
			base: indexed, head: unindexed,
			in:   gitutil.ChangedFile{Status: "R", OldPath: "a.go", Path: "b.go"},
			want: []gitutil.ChangedFile{{Status: "D", Path: "a.go"}},
		},
		{
			name: "rename into the index becomes an addition of the new path",
			base: unindexed, head: indexed,
			in:   gitutil.ChangedFile{Status: "R", OldPath: "a.go", Path: "b.go"},
			want: []gitutil.ChangedFile{{Status: "A", Path: "b.go"}},
		},
		{
			name: "unindexed on both sides is dropped",
			base: unindexed, head: unindexed,
			in:   gitutil.ChangedFile{Status: "M", Path: "a.go"},
			want: nil,
		},
		{
			name: "copy to an unindexed destination never deletes its source",
			base: indexed, head: unindexed,
			in:   gitutil.ChangedFile{Status: "C", OldPath: "a.go", Path: "b.go"},
			want: nil,
		},
		{
			name: "copy into the index adds only the destination",
			base: unindexed, head: indexed,
			in:   gitutil.ChangedFile{Status: "C", OldPath: "a.go", Path: "b.go"},
			want: []gitutil.ChangedFile{{Status: "A", Path: "b.go"}},
		},
		{
			// The source is untouched by a copy, so comparing the destination
			// against it would report a change to a file that did not change.
			name: "copy between indexed paths is still only an addition",
			base: indexed, head: indexed,
			in:   gitutil.ChangedFile{Status: "C", OldPath: "a.go", Path: "b.go"},
			want: []gitutil.ChangedFile{{Status: "A", Path: "b.go"}},
		},
		{
			// A rewrite restates the status, not what the entry is. The mode
			// decides whether the surviving side is file content at all, so it
			// has to survive the rebuild: a symlink that arrives here with an
			// empty mode is read as an ordinary blob and parsed as source.
			name: "rename out of the index carries the base mode",
			base: indexed, head: unindexed,
			in:   gitutil.ChangedFile{Status: "R", OldPath: "a.go", Path: "b.go", OldMode: gitutil.SymlinkMode, NewMode: "100644"},
			want: []gitutil.ChangedFile{{Status: "D", Path: "a.go", OldMode: gitutil.SymlinkMode}},
		},
		{
			name: "rename into the index carries the head mode",
			base: unindexed, head: indexed,
			in:   gitutil.ChangedFile{Status: "R", OldPath: "a.go", Path: "b.go", OldMode: "100644", NewMode: gitutil.SymlinkMode},
			want: []gitutil.ChangedFile{{Status: "A", Path: "b.go", NewMode: gitutil.SymlinkMode}},
		},
		{
			// A copy is only ever an addition of its destination, so the mode
			// that has to travel is the destination's.
			name: "copy into the index carries the destination mode",
			base: unindexed, head: indexed,
			in:   gitutil.ChangedFile{Status: "C", OldPath: "a.go", Path: "b.go", OldMode: "100644", NewMode: gitutil.SymlinkMode},
			want: []gitutil.ChangedFile{{Status: "A", Path: "b.go", NewMode: gitutil.SymlinkMode}},
		},
		{
			name: "a disabled policy admits everything",
			base: diffIndexPolicy{}, head: diffIndexPolicy{},
			in:   gitutil.ChangedFile{Status: "M", Path: "a.go"},
			want: []gitutil.ChangedFile{{Status: "M", Path: "a.go"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := admitChangedFiles([]gitutil.ChangedFile{test.in}, test.base, test.head)
			if len(got) != len(test.want) {
				t.Fatalf("admitted %#v, want %#v", got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("admitted %#v, want %#v", got, test.want)
				}
			}
		})
	}
}

// TestAnalyzeGitRangeWarnsWhenAnIndexPolicyFileChanges pins the one
// incompleteness a change-based diff cannot filter its way out of. A committed
// exclusion rule decides membership for files it never names, so dropping a
// re-inclusion removes a whole subtree from the head snapshot while Git reports
// only the rule file. The delta cannot contain those removals, so it must at
// least say so rather than read as complete.
func TestAnalyzeGitRangeWarnsWhenAnIndexPolicyFileChanges(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, ".gitignore", "vendor/*\n!vendor/rootpkg/\n!vendor/rootpkg/**\n")
	write(t, repo, "vendor/rootpkg/root.go", "package rootpkg\n\nfunc Root() int { return 1 }\n")
	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 1 }\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "initial")
	base := rev(t, repo, "HEAD")

	// The re-inclusion goes away. vendor/rootpkg/root.go does not change, but it
	// leaves the graph.
	write(t, repo, ".gitignore", "vendor/*\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "drop the re-inclusion")
	head := rev(t, repo, "HEAD")

	result, err := AnalyzeGitRange(context.Background(), repo, base, head, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, warning := range result.Warnings {
		if warning.Code == "W_INDEX_POLICY_CHANGED" && warning.FilePath == ".gitignore" {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %#v, want W_INDEX_POLICY_CHANGED for .gitignore", result.Warnings)
	}

	// A .graphignore outside the repository root decides nothing: the matcher only
	// ever reads <repo>/.graphignore, so warning about it would be a false report.
	write(t, repo, "pkg/.graphignore", "generated/\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "add a nested graphignore")
	nested, err := AnalyzeGitRange(context.Background(), repo, head, rev(t, repo, "HEAD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range nested.Warnings {
		if warning.Code == "W_INDEX_POLICY_CHANGED" {
			t.Fatalf("a non-root .graphignore warned about an index policy change: %#v", nested.Warnings)
		}
	}
	head = rev(t, repo, "HEAD")

	// An ordinary range must stay quiet; the warning is a real signal, not noise.
	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 2 }\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "ordinary edit")
	quiet, err := AnalyzeGitRange(context.Background(), repo, head, rev(t, repo, "HEAD"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range quiet.Warnings {
		if warning.Code == "W_INDEX_POLICY_CHANGED" {
			t.Fatalf("ordinary range warned about an index policy change: %#v", quiet.Warnings)
		}
	}
}

// TestAnalyzeGitRangeWarnsAboutPolicyChangesOutsideThePathspec pins that a
// pathspec narrows what Git reports, not what an exclusion rule decides. A root
// rule edited outside the requested scope still changes membership inside it,
// and the scoped changed-file list cannot show that.
func TestAnalyzeGitRangeWarnsAboutPolicyChangesOutsideThePathspec(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, ".gitignore", "vendor/*\n!vendor/rootpkg/\n!vendor/rootpkg/**\n")
	write(t, repo, "vendor/rootpkg/root.go", "package rootpkg\n\nfunc Root() int { return 1 }\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "initial")
	base := rev(t, repo, "HEAD")

	write(t, repo, ".gitignore", "vendor/*\n")
	write(t, repo, "vendor/rootpkg/root.go", "package rootpkg\n\nfunc Root() int { return 2 }\n")
	git(t, repo, "add", "-A", "-f")
	git(t, repo, "commit", "-m", "drop the re-inclusion and edit inside it")
	head := rev(t, repo, "HEAD")

	// Scoped to the subtree, so Git never reports the root rule file at all.
	result, err := AnalyzeGitRange(context.Background(), repo, base, head, []string{"vendor"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, warning := range result.Warnings {
		if warning.Code == "W_INDEX_POLICY_CHANGED" {
			found = true
		}
	}
	if !found {
		t.Fatalf("scoped range went silent about a policy change: %#v", result.Warnings)
	}
}

// TestAnalyzeGitRangeAcceptsEveryRootTreeSpelling pins the peel. Git spells "the
// tree of this commit" several ways, and a spelling this failed to recognize
// disabled every exclusion rule for the range.
func TestAnalyzeGitRangeAcceptsEveryRootTreeSpelling(t *testing.T) {
	repo := t.TempDir()
	initDiffPolicyRepo(t, repo)
	write(t, repo, ".graphignore", "vendored/\n")
	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 1 }\n")
	write(t, repo, "vendored/gen.go", "package vendored\n\nfunc Gen() int { return 1 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")

	write(t, repo, "keep/k.go", "package keep\n\nfunc Keep() int { return 2 }\n")
	write(t, repo, "vendored/gen.go", "package vendored\n\nfunc Gen() int { return 2 }\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "edit both")

	for _, spelling := range []string{"^{tree}", "^{tree}^{}", "^{tree}^{object}"} {
		t.Run(spelling, func(t *testing.T) {
			result, err := AnalyzeGitRange(context.Background(), repo, "HEAD~1"+spelling, "HEAD"+spelling, nil)
			if err != nil {
				t.Fatal(err)
			}
			if file := onlyFile(t, result); file.Path != "keep/k.go" {
				t.Fatalf("file = %#v, want only keep/k.go", file)
			}
		})
	}
}
