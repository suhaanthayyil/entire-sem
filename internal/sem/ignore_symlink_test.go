package sem

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Reproduces the finding: RenderRepoIgnoreDisclosure and its JSON twin echo
// the deciding rule's pattern TEXT into the response. Ignore files were opened
// via os.Stat, which follows symlinks, so a repository-controlled .gitignore
// (or .graphignore, or a nested .gitignore) that is ITSELF a symlink pointing
// outside the repo let a crafted matching path exfiltrate up to ~200 bytes of
// an arbitrary local file — e.g. a sibling .env — into the disclosed rule
// text. The fix: gate every ignore-file load on Lstat, so a symlinked ignore
// file hits the SAME "not a regular file" hard failure a non-regular file
// (e.g. a directory named .gitignore) already produced — it is never opened,
// so its target's content is never read into a rule.
func TestSymlinkedGitignoreIsRejectedNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows CI")
	}
	t.Parallel()

	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.env")
	if err := os.WriteFile(secretPath, []byte("SECRET_TOKEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	gitignore := filepath.Join(repo, ".gitignore")
	if err := os.Symlink(secretPath, gitignore); err != nil {
		t.Fatal(err)
	}

	_, err := loadWorktreeIgnoreMatcher(repo, nil, nil)
	if err == nil {
		t.Fatal("a symlinked .gitignore must be rejected as not-a-regular-file, not silently followed")
	}
	if strings.Contains(err.Error(), "SECRET_TOKEN") {
		t.Fatalf("the rejection error must not itself carry the symlink target's content: %v", err)
	}
}

// TestSymlinkedNestedGitignoreIsSkippedNotFollowed pins the same fix at the
// nested .gitignore loader (enterCharged), which used its own os.Stat gate
// separate from loadOptional/loadRequired.
func TestSymlinkedNestedGitignoreIsSkippedNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows CI")
	}
	t.Parallel()

	outside := t.TempDir()
	secretPath := filepath.Join(outside, "secret.env")
	if err := os.WriteFile(secretPath, []byte("SECRET_TOKEN\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, filepath.Join(repo, "sub", ".gitignore")); err != nil {
		t.Fatal(err)
	}

	stack := &nestedIgnoreStack{repo: repo}
	ok, err := stack.enterCharged(nil, "sub")
	if err != nil {
		t.Fatalf("a symlinked nested .gitignore must be treated as absent, not as a hard error: %v", err)
	}
	if !ok {
		t.Fatal("a symlinked nested .gitignore must be treated as absent (continue descending), not as an unreadable-path stop")
	}
	for _, level := range stack.levels {
		for _, rule := range level.matcher.rules {
			if rule.pattern == "SECRET_TOKEN" {
				t.Fatalf("nested matcher picked up a rule sourced from the symlink target's content: %q", rule.pattern)
			}
		}
	}
}

// TestSymlinkedCallerControlledIgnoreSourcesAreFollowedNotRejected reproduces
// the re-review finding on loadOptional/loadRequired: the Lstat guard above is
// scoped to REPOSITORY-controlled sources, because ignoreOrigin's own doc says
// only a repo-controlled rule is ever disclosed (repoExclusion's Rule field).
// A CALLER-controlled source -- .git/info/exclude (the checkout's own local
// exclude list, via loadOptional) and an explicit --ignore-file/--include-file
// (via loadRequired) -- carries none of that leak, and Git itself follows a
// symlinked .git/info/exclude. Rejecting either turned "a user shares their
// local exclude file via symlink" into a fatal error on every worktree
// search, for content that was never echoed anywhere.
func TestSymlinkedCallerControlledIgnoreSourcesAreFollowedNotRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.Symlink requires elevated privileges on Windows CI")
	}
	t.Parallel()

	t.Run(".git/info/exclude via loadOptional", func(t *testing.T) {
		t.Parallel()
		shared := t.TempDir()
		sharedExclude := filepath.Join(shared, "shared-exclude")
		if err := os.WriteFile(sharedExclude, []byte("*.local\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		repo := t.TempDir()
		if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sharedExclude, filepath.Join(repo, ".git", "info", "exclude")); err != nil {
			t.Fatal(err)
		}

		var matcher ignoreMatcher
		exclude := filepath.Join(repo, ".git", "info", "exclude")
		if err := matcher.loadOptional(exclude, false, localIgnoreOrigin(".git/info/exclude")); err != nil {
			t.Fatalf("a symlinked .git/info/exclude must be followed like Git itself follows it, got error: %v", err)
		}
		found := false
		for _, rule := range matcher.rules {
			if rule.pattern == "*.local" {
				found = true
			}
		}
		if !found {
			t.Fatal("the symlinked exclude file's rule was not loaded")
		}
	})

	t.Run("--ignore-file via loadRequired", func(t *testing.T) {
		t.Parallel()
		shared := t.TempDir()
		sharedIgnore := filepath.Join(shared, "shared-ignore")
		if err := os.WriteFile(sharedIgnore, []byte("*.local\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		repo := t.TempDir()
		linked := filepath.Join(repo, "explicit-ignore")
		if err := os.Symlink(sharedIgnore, linked); err != nil {
			t.Fatal(err)
		}

		var matcher ignoreMatcher
		if err := matcher.loadRequired(linked, false, callerIgnoreOrigin("explicit-ignore")); err != nil {
			t.Fatalf("a symlinked --ignore-file is the caller's own choice and must be followed, got error: %v", err)
		}
		found := false
		for _, rule := range matcher.rules {
			if rule.pattern == "*.local" {
				found = true
			}
		}
		if !found {
			t.Fatal("the symlinked --ignore-file's rule was not loaded")
		}
	})
}
