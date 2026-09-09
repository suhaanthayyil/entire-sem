package sem

import "testing"

// TestVendoredDirectoryReinclusionSurvivesAStrippedLocalExclude reproduces the
// trail finding: worktreeSourceFiles passed the SAME ignores value into both
// the final tracked-file ignore verdict (which must strip .git/info/exclude,
// since Git already applied it only to untracked discovery) and
// worktreeVendorIgnoreRules (which answers a different question — is a path
// Git already decided to list one this heuristic should still treat as
// vendored). A local `!vendor/pkg/` in .git/info/exclude is exactly the kind
// of rule Git used to decide to list vendor/pkg/keep.go as untracked at all;
// stripping it before ReincludesDescendant saw it made the vendored-directory
// heuristic drop that same path right back out, silently and with no
// disclosure (info/exclude exclusions are deliberately never disclosed).
func TestVendoredDirectoryReinclusionSurvivesAStrippedLocalExclude(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	write(t, repo, "main.go", "package main\n\nfunc main() {}\n")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	// Untracked: exercises the worktree listing path, which is the one that
	// strips local excludes for its final verdict.
	write(t, repo, "vendor/pkg/keep.go", "package pkg\n")
	write(t, repo, "vendor/other/skip.go", "package other\n")
	writeInfoExclude(t, repo, "vendor/*\n!vendor/pkg/\n")

	snapshot, err := BuildProviderSnapshotWithOptions(t.Context(), repo, "test-version", ProviderSnapshotOptions{Worktree: true})
	if err != nil {
		t.Fatal(err)
	}
	filesByPath := map[string]struct{}{}
	for _, file := range snapshot.Files {
		filesByPath[file.Path] = struct{}{}
	}
	if _, ok := filesByPath["vendor/pkg/keep.go"]; !ok {
		t.Error("vendor/pkg/keep.go missing: a local .git/info/exclude negation that made Git list this" +
			" path as untracked must also reach the vendored-directory heuristic's re-inclusion check")
	}
	if _, ok := filesByPath["vendor/other/skip.go"]; ok {
		t.Error("vendor/other/skip.go present: Git's own listing already excludes it (no re-inclusion" +
			" rule names it), so it must not appear regardless of the vendoring heuristic")
	}
}
