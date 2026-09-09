package sem

import (
	"os"
	"path/filepath"
	"testing"
)

// A linked worktree's .git is a regular FILE ("gitdir: <path>"), not a directory.
// loadWorktreeIgnoreMatcher used to join <repo>/.git/info/exclude unconditionally, so
// os.Stat returned ENOTDIR (not ErrNotExist), loadOptional treated it as fatal, and
// every search inside a worktree aborted with zero results.
func TestLoadWorktreeIgnoreMatcher_WorktreeDotGitIsAFile(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	repo := filepath.Join(root, "wt")
	realGitDir := filepath.Join(root, "main", ".git", "worktrees", "wt")
	commonDir := filepath.Join(root, "main", ".git")

	for _, dir := range []string{repo, realGitDir, filepath.Join(commonDir, "info")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// .git as a regular file pointing at the linked worktree's gitdir.
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+realGitDir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// gitdir/commondir points back at the shared .git that owns info/.
	if err := os.WriteFile(filepath.Join(realGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "info", "exclude"), []byte("secret.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	matcher, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err != nil {
		t.Fatalf("loadWorktreeIgnoreMatcher in a worktree: %v", err)
	}
	// The shared info/exclude must be found through the gitdir+commondir indirection.
	if !matcher.Ignored("secret.txt", false) {
		t.Error("secret.txt: want ignored via the worktree's shared info/exclude, got not ignored")
	}
	if !matcher.Ignored("build", true) {
		t.Error("build/: want ignored via .gitignore, got not ignored")
	}
	if matcher.Ignored("main.go", false) {
		t.Error("main.go: want not ignored, got ignored")
	}
}

// A .git file whose gitdir has no commondir (or is unreadable) must degrade to "no
// exclude file" rather than erroring the caller out.
func TestLoadWorktreeIgnoreMatcher_UnresolvableGitDir(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: /nonexistent/gitdir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadWorktreeIgnoreMatcher(repo, nil, nil); err != nil {
		t.Fatalf("unresolvable gitdir must not be fatal: %v", err)
	}
}

func TestGitInfoExcludePath(t *testing.T) {
	t.Parallel()

	t.Run("ordinary clone", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(repo, ".git", "info", "exclude")
		resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(want))
		if err != nil {
			t.Fatal(err)
		}
		want = filepath.Join(resolvedParent, filepath.Base(want))
		got, err := gitInfoExcludePath(repo)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("directory gitdir with commondir", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		repo := filepath.Join(root, "repo")
		gitDir := filepath.Join(repo, ".git")
		commonDir := filepath.Join(root, "common")
		if err := os.MkdirAll(filepath.Join(commonDir, "info"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte(commonDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(commonDir, "info", "exclude")
		resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(want))
		if err != nil {
			t.Fatal(err)
		}
		want = filepath.Join(resolvedParent, filepath.Base(want))
		got, err := gitInfoExcludePath(repo)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("got %q want %q", got, want)
		}
	})

	t.Run("no git dir at all", func(t *testing.T) {
		t.Parallel()
		got, err := gitInfoExcludePath(t.TempDir())
		if err != nil {
			t.Fatalf("a directory that is not a repository must not be an error: %v", err)
		}
		if got != "" {
			t.Errorf("non-git directory: got %q want \"\"", got)
		}
	})

	// An ordinary directory is a supported repository here, so an untrusted
	// tree can plant an oversized regular file at ".git". Before the fix this
	// was read into memory in full before any size check; it must now be
	// refused via os.Stat without the content ever being allocated.
	t.Run("oversized .git file is refused without reading its content", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		oversized := make([]byte, maxGitFileBytes+1)
		for i := range oversized {
			oversized[i] = 'A'
		}
		if err := os.WriteFile(filepath.Join(repo, ".git"), oversized, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := gitInfoExcludePath(repo); err != nil || got != "" {
			t.Errorf("oversized .git file: got %q, want refusal (\"\")", got)
		}
	})

	// Content that does not start with "gitdir:" is not a Git-authored
	// pointer. Before the fix, strings.TrimPrefix left it unchanged and the
	// entire file content was used as a literal directory path.
	t.Run("dot-git file without the gitdir prefix is refused", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("/etc/passwd\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := gitInfoExcludePath(repo); err != nil || got != "" {
			t.Errorf("gitdir-prefixless .git file: got %q, want refusal (\"\")", got)
		}
	})

	// commondir is not Git-size-limited itself, but this package still bounds
	// it defensively; an oversized one must degrade to "no exclude file"
	// rather than being read in full.
	t.Run("oversized commondir is refused without reading its content", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		gitDir := filepath.Join(repo, "elsewhere", "gitdir")
		if err := os.MkdirAll(gitDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".git"), []byte("gitdir: "+gitDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		oversized := make([]byte, maxGitPointerBytes+1)
		for i := range oversized {
			oversized[i] = 'B'
		}
		if err := os.WriteFile(filepath.Join(gitDir, "commondir"), oversized, 0o644); err != nil {
			t.Fatal(err)
		}
		// An oversized commondir refuses the git directory outright: no
		// info/exclude is applied at all. That is gitCommonDir's documented rule
		// and git's own — file_exists() decides presence, so an ABSENT commondir
		// resolves inside gitDir while a PRESENT but unreadable one refuses the
		// directory. Refusing is also the safe direction for this branch: no
		// exclude list is loaded, so no path can be removed from the corpus by
		// one, which is the failure this disclosure exists to catch.
		if got, err := gitInfoExcludePath(repo); err != nil || got != "" {
			t.Errorf("oversized commondir: got %q, want refusal (\"\")", got)
		}
	})
}
