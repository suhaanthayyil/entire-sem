package sem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The sweep's last pruning reason was "this path is already excluded", and it
// had the same defect as the two reasons before it (a vendored NAME, and the
// project's own ignore rules): an exclusion says "do not INDEX this tree", and
// the sweep indexes nothing. What it reads there is a `.git` pointer, and a
// pointer's target is somewhere ELSE — `vendorgit/dep/.git` ->
// `gitdir: ../../.dep-git` names a directory at the repository root that git
// lists in full and that, HEAD-less, no other rule can classify.
//
// Reproduced on d2b668d5 with the real `search` verb: `.dep-git/config` came
// back at rank 2 with the planted credential in its snippet.
//
// The excluding directory here is a git directory by structure, which is the
// cheapest way for a repository to buy itself an excluded tree: `vendorgit/`
// carries HEAD, objects/ and refs/, so the ancestor chain of its own listed
// files makes it a target before the sweep starts, and the sweep then refused
// to look inside it.
func TestGitDirExcluderSweepsInsideATreeItAlreadyExcluded(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "src/app.go", "package app\n")
	// A complete git directory: HEAD, objects/, refs/ — excluded on sight by the
	// structural half of rule 2.
	writeGitDirFixture(t, repo, "vendorgit")
	// The only pointer that names the HEAD-damaged target lives inside it.
	writeFile(t, repo, "vendorgit/dep/.git", "gitdir: ../../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")

	excluder := newGitDirExcluder(t.Context(), repo)
	excluder.unlistedRoots = []string{".dep-git/", "vendorgit/", "src/app.go"}
	excluder.gitAnsweredRoots = true
	excluder.observeListedPaths([]string{
		".dep-git/config", "src/app.go",
		"vendorgit/HEAD", "vendorgit/config", "vendorgit/hooks/post-commit.go",
	}, nil)

	if !excluder.excluded(".dep-git/config") {
		t.Error(`excluded(".dep-git/config") = false, want true: the pointer inside the excluded tree must still be read`)
	}
	if excluder.excluded("src/app.go") {
		t.Error(`excluded("src/app.go") = true, want false: ordinary source must stay listable`)
	}
	if !excluder.excluded("vendorgit/config") {
		t.Error(`excluded("vendorgit/config") = false, want true: the excluding directory itself stays excluded`)
	}
}

// The same shape end to end through the real `git ls-files` listings and the
// real `search` verb: the credential must not reach a snippet and the source
// beside the excluded tree must still be findable.
func TestSearchRepositoryNeverIndexesGitDirNamedFromInsideAnExcludedTree(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "src/app.go", "package app\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
	git(t, repo, "add", "src/app.go")
	git(t, repo, "commit", "-m", "src")
	writeGitDirFixture(t, repo, "vendorgit")
	writeFile(t, repo, "vendorgit/dep/.git", "gitdir: ../../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")

	assertNoGitDirLeak(t, repo, ".dep-git", "vendorgit")
	assertSearchFinds(t, repo, "src/app.go")
}

// And on the filesystem fallback, which prunes the same tree with SkipDir. The
// repository is deliberately NOT a git repository, so `git ls-files` fails and
// walkWorktreeFiles runs.
func TestSearchRepositoryNeverIndexesGitDirNamedFromInsideAnExcludedTreeOnTheFallback(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "src/app.go", "package app\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
	writeGitDirFixture(t, repo, "vendorgit")
	writeFile(t, repo, "vendorgit/dep/.git", "gitdir: ../../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")

	assertNoGitDirLeak(t, repo, ".dep-git", "vendorgit")
	assertSearchFinds(t, repo, "src/app.go")
}

// A pointer inside a `.git`-NAMED directory is the same shape with the other
// half of excluded(): the component rule, not a recorded target. A vendored
// tree unpacked from an archive can carry a literal `.git` directory that git
// itself never created, and the sweep must read the directories under it.
func TestGitDirExcluderSweepsInsideADotGitNamedTree(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "src/app.go", "package app\n")
	writeFile(t, repo, "vendor/pkg/.git/dep/.git", "gitdir: ../../../../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")

	excluder := newGitDirExcluder(t.Context(), repo)
	excluder.unlistedRoots = []string{".dep-git/", "vendor/", "src/app.go"}
	excluder.gitAnsweredRoots = true
	excluder.observeListedPaths([]string{".dep-git/config", "src/app.go"}, nil)

	if !excluder.excluded(".dep-git/config") {
		t.Error(`excluded(".dep-git/config") = false, want true: a pointer under a .git-named directory must still be read`)
	}
	if excluder.excluded("src/app.go") {
		t.Error(`excluded("src/app.go") = true, want false`)
	}
}

// The cost of reading a pruned tree's directories is one ReadDir per directory
// and no file read at all, so nothing inside an excluded tree can become
// indexable by way of the sweep. This pins that: every path the listing returns
// is still outside every excluded tree.
func TestSweepInsideExcludedTreeIndexesNothingFromIt(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "src/app.go", "package app\n")
	writeGitDirFixture(t, repo, "vendorgit")
	writeFile(t, repo, "vendorgit/dep/.git", "gitdir: ../../.dep-git\n")
	writeFile(t, repo, "vendorgit/dep/secret.go", "package dep\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")
	if err := os.MkdirAll(filepath.Join(repo, "vendorgit", "objects", "ab"), 0o755); err != nil {
		t.Fatal(err)
	}

	files, _, err := worktreeSourceFiles(t.Context(), repo, ignoreMatcher{}, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range files {
		if strings.HasPrefix(rel, "vendorgit/") || strings.HasPrefix(rel, ".dep-git/") {
			t.Errorf("listing returned %q from an excluded tree", rel)
		}
	}
	found := false
	for _, rel := range files {
		if rel == "src/app.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("listing lost src/app.go; files = %v", files)
	}
}
