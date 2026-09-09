package sem

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/entireio/entire-graph/internal/gitutil"
)

func TestGitWorktreePreflightAllowsRootGitMetadataAndInvokesLister(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "clean.go", "package clean\nfunc Present() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "clean worktree")

	calls := 0
	paths, warnings, err := worktreeSourceFilesWithLister(
		t.Context(), repo, ignoreMatcher{}, false, nil,
		func(ctx context.Context, repo string) ([]string, error) {
			calls++
			return gitutil.ListWorktreeFiles(ctx, repo)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("worktree lister calls = %d, want 1", calls)
	}
	if hasProviderWarning(warnings, "W_GIT_WORKTREE_FALLBACK") {
		t.Fatalf("clean worktree unexpectedly fell back: %+v", warnings)
	}
	if !containsWorktreePath(paths, "clean.go") {
		t.Fatalf("clean worktree paths = %q, want clean.go", paths)
	}
}

func TestGitWorktreeListingFailuresThroughSymlinkedAncestorWarn(t *testing.T) {
	physicalRepo := t.TempDir()
	initRepo(t, physicalRepo)
	writeFile(t, physicalRepo, "nested/source.go", "package source\nfunc PresentOnFallback() {}\n")
	git(t, physicalRepo, "add", ".")
	git(t, physicalRepo, "commit", "-m", "source")

	repo := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(physicalRepo, "nested"), repo); err != nil {
		t.Skipf("create repository-subdirectory symlink: %v", err)
	}

	t.Run("index listing", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		calls := 0
		paths, warnings, err := worktreeSourceFilesWithLister(
			t.Context(), repo, ignoreMatcher{}, false, nil,
			func(context.Context, string) ([]string, error) {
				calls++
				return nil, errors.New("worktree lister must not run after index failure")
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if calls != 0 {
			t.Fatalf("worktree lister calls = %d, want 0 after index failure", calls)
		}
		if !hasProviderWarning(warnings, "W_GIT_WORKTREE_FALLBACK") {
			t.Fatalf("warnings = %+v, want W_GIT_WORKTREE_FALLBACK", warnings)
		}
		if !containsWorktreePath(paths, "source.go") {
			t.Fatalf("fallback paths = %q, want source.go", paths)
		}
	})

	t.Run("worktree listing", func(t *testing.T) {
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
	})
}

func TestPlainDirectoryListingFailureThroughSymlinkedAncestorIsUnmarked(t *testing.T) {
	physicalRoot := t.TempDir()
	writeFile(t, physicalRoot, "nested/source.go", "package source\nfunc PlainFallback() {}\n")
	repo := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(filepath.Join(physicalRoot, "nested"), repo); err != nil {
		t.Skipf("create plain-directory symlink: %v", err)
	}
	t.Setenv("PATH", t.TempDir())

	paths, warnings, err := worktreeSourceFilesWithLister(
		t.Context(), repo, ignoreMatcher{}, false, nil,
		func(context.Context, string) ([]string, error) {
			t.Fatal("worktree lister ran after index failure")
			return nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if hasProviderWarning(warnings, "W_GIT_WORKTREE_FALLBACK") {
		t.Fatalf("plain-directory warnings = %+v, do not want W_GIT_WORKTREE_FALLBACK", warnings)
	}
	if !containsWorktreePath(paths, "source.go") {
		t.Fatalf("fallback paths = %q, want source.go", paths)
	}
}

func TestGitEntryProbeSharesDirectoryObservationBudget(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "first/a", "a")
	writeFile(t, root, "second/b", "b")
	resolver, resolution := NewGitCeilingPathResolver(root)
	if resolution != GitCeilingPathResolved {
		t.Fatalf("create resolver: resolution %v", resolution)
	}
	defer resolver.Close()
	canonicalRoot, resolution := resolver.Canonicalize(root)
	if resolution != GitCeilingPathResolved {
		t.Fatalf("canonicalize resolver root: resolution %v", resolution)
	}

	entriesSeen, bytesSeen := maxListedDirectoryObservations-1, 0
	found, resolution := resolver.hasGitEntryWithBudget(filepath.Join(canonicalRoot, "first"), &entriesSeen, &bytesSeen)
	if found || resolution != GitCeilingPathResolved {
		t.Fatalf("first probe = found %v, resolution %v; want false, resolved", found, resolution)
	}
	found, resolution = resolver.hasGitEntryWithBudget(filepath.Join(canonicalRoot, "second"), &entriesSeen, &bytesSeen)
	if found || resolution != GitCeilingPathUnsafe {
		t.Fatalf("second probe = found %v, resolution %v; want false, unsafe after shared bound", found, resolution)
	}
}

func TestGitWorktreePreflightDeclinesNestedGitMarkerBeforeLister(t *testing.T) {
	repo := t.TempDir()
	initRepo(t, repo)
	writeFile(t, repo, "pkg/source.go", "package pkg\nfunc PresentOnFallback() {}\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "source")
	// The marker is deliberately case-varied and contains a raw UNC target. The
	// preflight decides from its directory entry name and must never read or
	// resolve these bytes before declining Git's recursive untracked listing.
	writeFile(t, repo, "pkg/.GiT", `gitdir: \\192.0.2.1\share\repository`+"\n")

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
	if !containsWorktreePath(paths, "pkg/source.go") {
		t.Fatalf("fallback paths = %q, want pkg/source.go", paths)
	}
}

func TestGitWorktreePreflightDeclinesEveryNestedGitMarkerKind(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		create func(*testing.T, string)
	}{
		{"file", func(t *testing.T, marker string) {
			if err := os.WriteFile(marker, []byte("not read"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"directory", func(t *testing.T, marker string) {
			if err := os.Mkdir(marker, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink", func(t *testing.T, marker string) {
			if err := os.Symlink("missing-target", marker); err != nil {
				t.Skipf("create symlink: %v", err)
			}
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repo := t.TempDir()
			if err := os.Mkdir(filepath.Join(repo, "nested"), 0o755); err != nil {
				t.Fatal(err)
			}
			testCase.create(t, filepath.Join(repo, "nested", ".gIt"))
			if err := gitWorktreeSafeBeforeListing(t.Context(), repo); err == nil {
				t.Fatal("nested .git marker passed the worktree preflight")
			} else if errors.Is(err, errGitWorktreeFallbackUnsafe) {
				t.Fatalf("local nested .git marker incorrectly disabled the safe filesystem fallback: %v", err)
			}
		})
	}
}

func TestGitWorktreePreflightTraversalCeilingIsFallbackUnsafe(t *testing.T) {
	repo := t.TempDir()
	anchor, resolvedRepo, err := newPathTraversalAnchor(repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	root, err := newSweepDirectoryRoot(resolvedRepo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	budget := gitWorktreePreflightBudget{traversalSteps: maxWorktreeWalkDirectoryBytes}
	if !budget.admitDirectory("") {
		t.Fatal("failed to admit the selected root")
	}
	err = gitWorktreeSafeBeforeListingFromDirectories(
		t.Context(), root, anchor, &budget, []string{""},
	)
	if !errors.Is(err, errGitWorktreeFallbackUnsafe) {
		t.Fatalf("traversal-ceiling error = %v, want %v", err, errGitWorktreeFallbackUnsafe)
	}
}

func TestGitWorktreePreflightPreservesCancellationIdentity(t *testing.T) {
	repo := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := EnsureWorktreeSafeForFilesystemTraversal(ctx, repo)
	if !errors.Is(err, errGitWorktreeFallbackUnsafe) {
		t.Fatalf("cancelled preflight error = %v, want %v", err, errGitWorktreeFallbackUnsafe)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preflight error = %v, want %v identity", err, context.Canceled)
	}
}

func hasProviderWarning(warnings []ProviderWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

func containsWorktreePath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
