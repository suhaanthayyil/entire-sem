//go:build windows

package sem

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestGitWorktreeListingFailureThroughJunctionedSubdirectoryWarns(t *testing.T) {
	physicalRepo := t.TempDir()
	initRepo(t, physicalRepo)
	writeFile(t, physicalRepo, "nested/source.go", "package source\nfunc PresentOnFallback() {}\n")
	git(t, physicalRepo, "add", ".")
	git(t, physicalRepo, "commit", "-m", "source")

	repo := filepath.Join(t.TempDir(), "alias")
	windowsJunction(t, filepath.Join(physicalRepo, "nested"), repo)
	calls := 0
	paths, warnings, err := worktreeSourceFilesWithLister(
		t.Context(), repo, ignoreMatcher{}, false, nil,
		func(context.Context, string) ([]string, error) {
			calls++
			return nil, errors.New("forced worktree listing failure")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("worktree lister calls = %d, want 1", calls)
	}
	if !hasProviderWarning(warnings, "W_GIT_WORKTREE_FALLBACK") {
		t.Fatalf("warnings = %+v, want W_GIT_WORKTREE_FALLBACK", warnings)
	}
	if !containsWorktreePath(paths, "source.go") {
		t.Fatalf("fallback paths = %q, want source.go", paths)
	}
}

func TestGitWorktreePreflightJunctionFailsClosedBeforeListerOrFallback(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "clean.go", "package clean\nfunc Present() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "clean worktree")

	external := t.TempDir()
	writeFile(t, external, "outside.go", "package outside\n")
	windowsJunction(t, external, filepath.Join(repo, "mounted"))

	calls := 0
	paths, warnings, err := worktreeSourceFilesWithLister(
		t.Context(), repo, ignoreMatcher{}, false, nil,
		func(context.Context, string) ([]string, error) {
			calls++
			return nil, nil
		},
	)
	if !errors.Is(err, errGitWorktreeFallbackUnsafe) {
		t.Fatalf("junction error = %v, want %v", err, errGitWorktreeFallbackUnsafe)
	}
	if calls != 0 {
		t.Fatalf("worktree lister calls = %d, want 0", calls)
	}
	if len(paths) != 0 || len(warnings) != 0 {
		t.Fatalf("paths = %q, warnings = %+v; unsafe traversal must fail closed", paths, warnings)
	}
}

func TestGitWorktreeFallbackSelectedEarlyCannotBypassJunctionGuard(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, ".git", `gitdir: \\203.0.113.1\share\repo`+"\n")
	writeFile(t, repo, "clean.go", "package clean\n")

	external := t.TempDir()
	writeFile(t, external, "outside.go", "package outside\n")
	windowsJunction(t, external, filepath.Join(repo, "mounted"))

	calls := 0
	paths, warnings, err := worktreeSourceFilesWithLister(
		t.Context(), repo, ignoreMatcher{}, false, nil,
		func(context.Context, string) ([]string, error) {
			calls++
			return nil, nil
		},
	)
	if !errors.Is(err, errGitWorktreeFallbackUnsafe) {
		t.Fatalf("early fallback junction error = %v, want %v", err, errGitWorktreeFallbackUnsafe)
	}
	if calls != 0 {
		t.Fatalf("worktree lister calls = %d, want 0", calls)
	}
	if len(paths) != 0 || len(warnings) != 0 {
		t.Fatalf("paths = %q, warnings = %+v; early fallback must fail closed", paths, warnings)
	}
}

func TestGitWorktreePreflightUsesNoFollowFallbackForDirectorySymlink(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "clean.go", "package clean\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "clean worktree")

	external := t.TempDir()
	writeFile(t, external, "outside.go", "package outside\n")
	windowsSymlinkOrSkip(t, external, filepath.Join(repo, "linked"))

	calls := 0
	paths, warnings, err := worktreeSourceFilesWithLister(
		t.Context(), repo, ignoreMatcher{}, false, nil,
		func(context.Context, string) ([]string, error) {
			calls++
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("worktree lister calls = %d, want 0", calls)
	}
	if !hasProviderWarning(warnings, "W_GIT_WORKTREE_FALLBACK") {
		t.Fatalf("warnings = %+v, want W_GIT_WORKTREE_FALLBACK", warnings)
	}
	if !containsWorktreePath(paths, "clean.go") || containsWorktreePath(paths, "linked/outside.go") {
		t.Fatalf("fallback paths = %q, want clean.go without linked/outside.go", paths)
	}
}

func TestGitWorktreePreflightCompatibilityModeSkipsJunctionInNoFollowFallback(t *testing.T) {
	t.Setenv("GODEBUG", "winsymlink=0")
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "clean.go", "package clean\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "clean worktree")

	external := t.TempDir()
	writeFile(t, external, "outside.go", "package outside\n")
	windowsJunction(t, external, filepath.Join(repo, "mounted"))

	calls := 0
	paths, warnings, err := worktreeSourceFilesWithLister(
		t.Context(), repo, ignoreMatcher{}, false, nil,
		func(context.Context, string) ([]string, error) {
			calls++
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("worktree lister calls = %d, want 0", calls)
	}
	if !hasProviderWarning(warnings, "W_GIT_WORKTREE_FALLBACK") {
		t.Fatalf("warnings = %+v, want W_GIT_WORKTREE_FALLBACK", warnings)
	}
	if !containsWorktreePath(paths, "clean.go") || containsWorktreePath(paths, "mounted/outside.go") {
		t.Fatalf("compatibility fallback paths = %q, want clean.go without mounted/outside.go", paths)
	}
}
