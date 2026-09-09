package sem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unreadableOrSkip takes the read permission off dir and confirms the process
// actually lost it, because a chmod is a request rather than a guarantee: root
// ignores the mode entirely, and so does a filesystem mounted without it. The
// capability is probed rather than inferred from GOOS — a Windows or a
// root-owned CI runner is skipped by the probe, and no platform is skipped that
// can in fact represent the layout.
//
// The mode is restored on cleanup so t.TempDir can remove the tree.
func unreadableOrSkip(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skipf("cannot drop read permission here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("this process reads a mode-000 directory anyway (root, or a filesystem that ignores the mode)")
	}
}

// unreadableFileOrSkip is the same probe for a FILE, which is the second way a
// `gitdir:` pointer goes unread.
func unreadableFileOrSkip(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Skipf("cannot drop read permission here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if file, err := os.Open(path); err == nil {
		_ = file.Close()
		t.Skip("this process reads a mode-000 file anyway (root, or a filesystem that ignores the mode)")
	}
}

func enumerableButUnsearchableOrSkip(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o444); err != nil {
		t.Skipf("cannot remove directory search permission here: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Skipf("filesystem cannot represent an enumerable, nonempty mode-0444 directory: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, entries[0].Name())); err == nil {
		t.Skip("this process can traverse a mode-0444 directory anyway (root or ignored mode bits)")
	}
}

// A sweep root the process may not READ hides the `.git` pointer inside it, and
// the git directory that pointer names is somewhere ELSE in the tree, where git
// lists it as ordinary untracked content. `build/` mode 000, holding
// `build/dep/.git` -> `gitdir: ../../.dep-git`, put `.dep-git/config` and its
// remote credential into the search results.
//
// Neither remedy the report named works: propagating the error makes a
// repository holding one root-owned build directory wholly unsearchable (and
// TestSearchRepositoryStillIndexesSourceBesideAnUnreadableDirectory is what
// holds that line), and excluding the unreadable subtree excludes the wrong
// path, since the target is elsewhere by construction. What closes it is
// promoteUnverifiedGitDirs: with the pointer out of reach, the structure the
// POINTER RULE ITSELF accepts — objects/ and refs/ through commondir, HEAD test
// dropped — is enough on its own.
func TestSearchRepositoryNeverIndexesGitDirNamedFromAnUnreadableSweepRootGitListed(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, ".gitignore", "build/\n")
	writeFile(t, repo, "tracked.go", "package tracked\n")
	git(t, repo, "add", ".gitignore", "tracked.go")
	git(t, repo, "commit", "-m", "tracked")
	writeFile(t, repo, "pkg/source.go", "package pkg\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
	writeFile(t, repo, "build/dep/.git", "gitdir: ../../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")
	unreadableOrSkip(t, filepath.Join(repo, "build"))

	assertNoGitDirLeak(t, repo, ".dep-git")
	assertSearchFinds(t, repo, "pkg/source.go")
}

// The same layout on the filesystem fallback, which prunes the ignored tree with
// observePrunedSubtree and so reads it through the same sweep.
func TestSearchRepositoryNeverIndexesGitDirNamedFromAnUnreadableSweepRootOnFallback(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, ".gitignore", "build/\n")
	writeFile(t, repo, "src/app.go", "package src\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
	writeFile(t, repo, "build/dep/.git", "gitdir: ../../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")
	unreadableOrSkip(t, filepath.Join(repo, "build"))

	assertNoGitDirLeak(t, repo, ".dep-git")
	assertSearchFinds(t, repo, "src/app.go")
}

// The second way the pointer goes unread, and it needs no unreadable directory
// at all: the `.git` FILE itself is mode 000. os.Stat still reports a regular
// file, so the rule got as far as reading it and then treated "I may not read
// this" as "this is not a gitfile" — the same silent no-answer, one layer down.
func TestSearchRepositoryNeverIndexesGitDirNamedFromAnUnreadablePointerFile(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "tracked.go", "package tracked\n")
	git(t, repo, "add", "tracked.go")
	git(t, repo, "commit", "-m", "tracked")
	writeFile(t, repo, "pkg/source.go", "package pkg\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
	writeFile(t, repo, "dep/.git", "gitdir: ../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")
	unreadableFileOrSkip(t, filepath.Join(repo, "dep", ".git"))

	assertNoGitDirLeak(t, repo, ".dep-git")
	assertSearchFinds(t, repo, "pkg/source.go")
	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, warnings, err := worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, warning := range warnings {
		if warning.Code == "W_GITDIR_POINTER_UNREADABLE" && strings.Contains(warning.Detail, "dep/.git") {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %+v, want W_GITDIR_POINTER_UNREADABLE naming dep/.git", warnings)
	}
}

// The availability half, and the reason the report's first remedy was refused.
// A root-owned build directory or a permission-restricted cache is ordinary in a
// real checkout, and one of them must not cost the caller the whole repository.
//
// The unignored fallback case is a fix, not a guard: filepath.WalkDir reports a
// directory it could not read by calling the walk function a second time with
// the error, the walk returned it, and `search` over a directory with ONE
// mode-000 subdirectory answered `permission denied` and indexed nothing at all.
func TestSearchRepositoryStillIndexesSourceBesideAnUnreadableDirectory(t *testing.T) {
	cases := []struct {
		name      string
		gitRepo   bool
		gitignore string
	}{
		{name: "git listing, ignored", gitRepo: true, gitignore: "out/\n"},
		{name: "git listing, untracked", gitRepo: true},
		{name: "fallback, ignored", gitignore: "out/\n"},
		{name: "fallback, untracked"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			repo := t.TempDir()
			writeFile(t, repo, "src/app.go", "package src\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
			writeFile(t, repo, "out/gen.go", "package out\n\nfunc Built() {}\n")
			if testCase.gitignore != "" {
				writeFile(t, repo, ".gitignore", testCase.gitignore)
			}
			if testCase.gitRepo {
				initRepo(t, repo)
				git(t, repo, "add", "-A")
				git(t, repo, "commit", "-m", "src")
			}
			unreadableOrSkip(t, filepath.Join(repo, "out"))

			assertSearchFinds(t, repo, "src/app.go")
		})
	}
}

func TestSearchRepositoryStillIndexesSourceBesideEnumerableUnsearchableDirectory(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "src/app.go", "package src\n\nfunc LoadOriginCredential() string { return \"\" }\n")
	writeFile(t, repo, "out/.gitignore", "*.go\n")
	writeFile(t, repo, "out/gen.go", "package out\n")
	enumerableButUnsearchableOrSkip(t, filepath.Join(repo, "out"))

	assertSearchFinds(t, repo, "src/app.go")
}

// The promotion is conditional, and this is the condition. A directory holding
// an `objects/` and a `refs/` directory is ordinary program text —
// `testdata/parser` in this very repository is exactly that — which is why the
// STANDALONE structural rule keeps git's HEAD test. With nothing unreadable
// there is no unread pointer to compensate for, so that directory stays indexed,
// and the price of the fix is paid only where evidence was actually hidden.
func TestSearchRepositoryStillIndexesAStructureOnlyDirectoryWhenNothingWasUnreadable(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "testdata/parser/objects/fixture.txt", "objects\n")
	writeFile(t, repo, "testdata/parser/refs/fixture.txt", "refs\n")
	writeFile(t, repo, "testdata/parser/loader.go", "package parser\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")

	assertSearchFinds(t, repo, "testdata/parser/loader.go")
}

// And the same tree with one unreadable directory beside it, which is the
// measured cost stated exactly: the structure-only directory is given up,
// nothing else is. `src/app.go` — every other file in the repository — is still
// indexed, so this narrows one subtree rather than the search.
func TestSearchRepositoryGivesUpOnlyTheStructureOnlyDirectoryWhileEvidenceIsHidden(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "src/app.go", "package src\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
	writeFile(t, repo, "testdata/parser/objects/fixture.txt", "objects\n")
	writeFile(t, repo, "testdata/parser/refs/fixture.txt", "refs\n")
	writeFile(t, repo, "testdata/parser/loader.go", "package parser\n\n// LoadOriginCredential returns the origin remote credential.\nfunc LoadOriginCredential() string { return \"\" }\n")
	writeFile(t, repo, "out/gen.go", "package out\n\nfunc Built() {}\n")
	unreadableOrSkip(t, filepath.Join(repo, "out"))

	response, err := SearchRepository(t.Context(), repo, "test", "origin remote credential loader", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     10,
	})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	found := false
	for _, result := range response.Results {
		paths = append(paths, result.FilePath)
		if result.FilePath == "src/app.go" {
			found = true
		}
		if strings.HasPrefix(result.FilePath, "testdata/parser/") {
			t.Errorf("search returned %q; a structure-only directory is given up while a pointer could be hidden", result.FilePath)
		}
	}
	if !found {
		t.Errorf("search did not return src/app.go; results = %v", paths)
	}
}

// The unit-level statement of the same rule, so a change to the promotion is
// caught without a whole search: the flag decides, and the structure test it
// applies is the POINTER rule's own, not the standalone rule's.
func TestGitDirExcluderPromotesStructureOnlyDirectoriesOnlyWhileEvidenceIsHidden(t *testing.T) {
	t.Parallel()
	for _, hidden := range []bool{false, true} {
		t.Run(map[bool]string{false: "nothing hidden", true: "evidence hidden"}[hidden], func(t *testing.T) {
			t.Parallel()
			repo := t.TempDir()
			writeHeadlessGitDirFixture(t, repo, ".dep-git")
			writeFile(t, repo, "src/app.go", "package src\n")

			excluder := newGitDirExcluder(t.Context(), repo)
			excluder.observeListedPaths([]string{".dep-git/config", "src/app.go"}, nil)
			if hidden {
				// observeListedPaths already ran the promotion and returned
				// early with nothing hidden, leaving promotedUnverified unset,
				// so this is the first and only pass.
				excluder.hiddenEvidence++
				excluder.promoteUnverifiedGitDirs()
			}
			if got := excluder.excluded(".dep-git/config"); got != hidden {
				t.Errorf("excluded(.dep-git/config) = %v, want %v", got, hidden)
			}
			if excluder.excluded("src/app.go") {
				t.Error("excluded(src/app.go) = true, want false: only the git-directory shape is given up")
			}
		})
	}
}

// TestWalkWorktreeFilesWarnsAboutAnUnreadableDirectory reproduces the trail
// finding on the WalkDir error branch: hiddenEvidence already makes an
// unreadable subtree fail closed for promoteUnverifiedGitDirs, but nothing
// told the CALLER that ordinary source under that path is now silently
// missing from the listing rather than reported as an error, the way main
// would have reported it (at the cost of aborting the whole walk, which is
// the outage the fail-open behavior below this fix exists to prevent).
func TestWalkWorktreeFilesWarnsAboutAnUnreadableDirectory(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	writeFile(t, repo, "src/app.go", "package src\n")
	writeFile(t, repo, "out/gen.go", "package out\n\nfunc Built() {}\n")
	unreadableOrSkip(t, filepath.Join(repo, "out"))

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	paths, warnings, walkErr := walkWorktreeFiles(t.Context(), repo, ignores, nil, nil)
	if walkErr != nil {
		t.Fatalf("an unreadable subdirectory must not abort the whole walk: %v", walkErr)
	}
	found := false
	for _, p := range paths {
		if p == "src/app.go" {
			found = true
		}
	}
	if !found {
		t.Errorf("src/app.go missing from paths = %v: an unreadable sibling must not drop unrelated source", paths)
	}
	var warned bool
	for _, w := range warnings {
		if w.Code == "W_WALK_UNREADABLE_DIRECTORY" && strings.Contains(w.Detail, "out") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("warnings = %+v, want a W_WALK_UNREADABLE_DIRECTORY warning naming \"out\": an unreadable"+
			" directory now silently drops the source under it from the listing, and that gap must be"+
			" disclosed since it is no longer reported as an error", warnings)
	}
}

func TestUnreadableEvidenceSamplesAreOrderIndependent(t *testing.T) {
	var excluder gitDirExcluder
	for _, value := range []string{"j", "i", "h", "g", "f", "e", "d", "c", "b", "a", "h"} {
		excluder.noteUnreadableWalkDir(value)
		excluder.noteSweepUnreadableDir(value)
		excluder.noteUnreadablePointer(value)
	}
	want := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	if got := excluder.unreadableWalkDirs; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unreadable walk sample = %v, want %v", got, want)
	}
	if got := excluder.sweepUnreadableDirs; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sweep unreadable sample = %v, want %v", got, want)
	}
	pointerWant := []string{"a/.git", "b/.git", "c/.git", "d/.git", "e/.git", "f/.git", "g/.git", "h/.git"}
	if got := excluder.unreadablePointers; strings.Join(got, ",") != strings.Join(pointerWant, ",") {
		t.Fatalf("unreadable pointer sample = %v, want %v", got, pointerWant)
	}
}

// TestWorktreeSourceFilesWarnsAboutAnUnreadableSweepDirectory reproduces the
// trail finding on descendObserving's ReadDir error branch: an unreadable
// directory the `.git`-pointer sweep could not read only incremented
// hiddenEvidence, which makes promoteUnverifiedGitDirs fail closed by
// excluding every structurally-git-shaped directory it can no longer rule
// out — but sweepStop stays sweepRanToCompletion for this case (the sweep
// skips the one directory and keeps going), so sweepWarnings' switch on
// sweepStop never disclosed it. The caller had no machine-readable signal
// that unrelated structure-only source trees may have been excluded as a
// side effect of one unreadable directory. The pre-launch worktree guard can
// now detect the same directory earlier and select the filesystem fallback;
// that path must preserve equivalent explicit unreadable + fallback warnings.
func TestWorktreeSourceFilesWarnsAboutAnUnreadableSweepDirectory(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, ".gitignore", "build/\n")
	writeFile(t, repo, "tracked.go", "package tracked\n")
	git(t, repo, "add", ".gitignore", "tracked.go")
	git(t, repo, "commit", "-m", "tracked")
	writeFile(t, repo, "build/dep/.git", "gitdir: ../../.dep-git\n")
	writeHeadlessGitDirFixture(t, repo, ".dep-git")
	unreadableOrSkip(t, filepath.Join(repo, "build"))

	ignores, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, warnings, err := worktreeSourceFiles(t.Context(), repo, ignores, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sweepWarned, walkWarned, fallbackWarned bool
	for _, w := range warnings {
		if w.Code == "W_GITDIR_SWEEP_UNREADABLE_DIRECTORY" && strings.Contains(w.Detail, "build") {
			sweepWarned = true
		}
		if w.Code == "W_WALK_UNREADABLE_DIRECTORY" && strings.Contains(w.Detail, "build") {
			walkWarned = true
		}
		if w.Code == "W_GIT_WORKTREE_FALLBACK" && strings.Contains(w.Detail, "build") {
			fallbackWarned = true
		}
	}
	if !sweepWarned && !(walkWarned && fallbackWarned) {
		t.Errorf("warnings = %+v, want either the sweep warning or explicit unreadable-directory and Git-fallback warnings naming \"build\"", warnings)
	}
}

// TestGitDirExcluderSweepUnreadableWarningIsSilentWhenNothingWasUnreadable is
// the widening direction: an ordinary sweep that reads everything it is given
// must not emit the new warning.
func TestGitDirExcluderSweepUnreadableWarningIsSilentWhenNothingWasUnreadable(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	excluder := newGitDirExcluder(t.Context(), repo)
	if got := excluder.sweepUnreadableDirWarning(); got != nil {
		t.Errorf("sweepUnreadableDirWarning() = %+v on a sweep with nothing unreadable, want nil", got)
	}
}

// assertSearchFinds is the availability assertion these tests share: the
// repository is still searchable and the ordinary source file is still in the
// results.
func assertSearchFinds(t *testing.T, repo, want string) {
	t.Helper()
	response, err := SearchRepository(t.Context(), repo, "test", "origin remote credential loader", SearchOptions{
		Worktree: true,
		Profile:  ProfileSyntaxOnly,
		TopK:     10,
	})
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	var paths []string
	for _, result := range response.Results {
		paths = append(paths, result.FilePath)
		if result.FilePath == want {
			return
		}
	}
	t.Errorf("search did not return %q; results = %v", want, paths)
}
