package sem

import (
	"os"
	"path/filepath"
	"testing"
)

// A negation in a NESTED ignore file carries the path context of the directory
// that file lives in, so a basename-only negation there is not the pathless
// signal a basename-only negation at the repository root is. Skipping it made
// the vendored-directory heuristic answer "nothing here is first-party" for a
// tree whose own .gitignore reopens part of it, and Git-tracked source was
// dropped from the corpus without a warning.
func TestNestedIgnoreNegationReincludesUnderItsOwnDirectory(t *testing.T) {
	t.Parallel()

	rules := newNestedIgnoreRules(ignoreMatcher{})
	// vendor/.gitignore excludes everything and reopens one package, spelled the
	// ordinary unanchored way.
	if err := rules.addFile("vendor/.gitignore", "*\n!mypkg/\n"); err != nil {
		t.Fatal(err)
	}
	// A deeper file whose own negation names a bare filename.
	if err := rules.addFile("third_party/pkg/.gitignore", "*\n!lib.py\n"); err != nil {
		t.Fatal(err)
	}

	if !rules.ReincludesDescendant("vendor") {
		t.Error("vendor: a negation in vendor/.gitignore must count as a re-included descendant")
	}
	if !rules.ReincludesDescendant("third_party") {
		t.Error("third_party: a negation in third_party/pkg/.gitignore must count as a re-included descendant")
	}
	// Path-scoped: the negation speaks for its own directory, not for the whole
	// repository. A tree that holds no such ignore file keeps no re-inclusion.
	if rules.ReincludesDescendant("build") {
		t.Error("build: a negation elsewhere in the tree must not re-include an unrelated directory")
	}

	// At the repository root there is no directory to supply the missing path,
	// so a basename-only negation still carries no signal.
	var root ignoreMatcher
	if err := root.loadContent("*\n!.keep\n", false, repoIgnoreOrigin(".gitignore")); err != nil {
		t.Fatal(err)
	}
	if root.ReincludesDescendant("vendor") {
		t.Error("vendor: a root basename-only negation names no path and must not re-include a tree")
	}
}

// `.git/info/exclude` is the repository's own private exclude list, and the
// graph reads it with the same authority as .gitignore. A linked worktree
// reaches it through the `.git` POINTER FILE, and every failure to read that
// pointer was reported as "there is no git directory here" — so a pointer this
// process may not read silently dropped the whole exclude list and admitted the
// files it names. Absence must degrade; a read failure must not.
func TestLoadWorktreeIgnoreMatcherRefusesAnUnreadableGitPointer(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "wt")
	gitDir := filepath.Join(root, "main", ".git", "worktrees", "wt")
	commonDir := filepath.Join(root, "main", ".git")
	for _, dir := range []string{repo, gitDir, filepath.Join(commonDir, "info")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "info", "exclude"), []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pointer := filepath.Join(repo, ".git")
	if err := os.WriteFile(pointer, []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Readable: the exclude list is found and applied.
	matcher, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatalf("readable pointer: %v", err)
	}
	if !matcher.Ignored("secret.txt", false) {
		t.Fatal("readable pointer: the shared info/exclude was not applied")
	}

	unreadableFileOrSkip(t, pointer)

	if _, err := loadWorktreeIgnoreMatcher(repo, nil, nil); err == nil {
		t.Fatal("an unreadable .git pointer was reported as \"no git directory\": " +
			"the repository's info/exclude was skipped and its excluded files admitted, silently")
	}
}

// The other side of that line. A repository ROOT this process cannot read is not
// a dropped exclude policy — the entry naming one was never visible, and the
// listing preflight refuses the whole operation on its own terms
// (TestExecuteOnlyRepositoryRootIsRefusedByTheListingPreflight). Reporting it
// here instead would make every such repository fail with the wrong reason.
func TestLoadWorktreeIgnoreMatcherStaysSilentForAnUnlistableRepositoryRoot(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Execute-only: names inside can still be looked up, so the earlier
	// .gitignore probe passes and the `.git` resolution is what fails. That is
	// the layout the openSource preflight test uses.
	unlistableButSearchableOrSkip(t, repo)

	if _, err := loadWorktreeIgnoreMatcher(repo, nil, nil); err != nil {
		t.Fatalf("an unlistable repository root must not be reported as a dropped exclude policy: %v", err)
	}
}

// unlistableButSearchableOrSkip makes dir execute-only and confirms the process
// really lost enumeration while keeping lookup, because a chmod is a request:
// root ignores the mode and so does a filesystem mounted without it.
func unlistableButSearchableOrSkip(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o111); err != nil {
		t.Skipf("cannot make this directory execute-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if opened, err := os.Open(dir); err == nil {
		_ = opened.Close()
		t.Skip("this process lists a mode-0111 directory anyway (root, or a filesystem that ignores the mode)")
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Skipf("this filesystem does not grant lookup without read: %v", err)
	}
}
