package sem

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCaptureRefusesASymlinkedGraphIgnoreLikeTheUncachedPath pins the two
// readers of a repository-controlled ignore file to ONE rule.
//
// loadPath refuses a .graphignore that is itself a symlink, because the
// repo_ignored disclosure echoes the matched rule's pattern TEXT: following the
// link would report an arbitrary local file's lines as the repository's own
// rules. A cache-enabled search does not go through loadPath at all -- it
// captures the file in captureIgnorePolicy -- and that reader stat'd through the
// link, so the same repository enforced one policy uncached and another cached,
// with the cached one carrying the leak.
func TestCaptureRefusesASymlinkedGraphIgnoreLikeTheUncachedPath(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.rules")
	if err := os.WriteFile(outside, []byte("SECRET-RULE-FROM-OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(repo, graphIgnoreFileName)
	if err := os.Symlink(outside, linked); err != nil {
		t.Skipf("filesystem does not support the symlinked ignore file: %v", err)
	}

	var direct ignoreMatcher
	uncachedErr := direct.loadPath(linked, false, false, graphIgnoreOrigin())
	if uncachedErr == nil {
		t.Fatal("the uncached reader stopped refusing a symlinked .graphignore; this test no longer pins anything")
	}

	policy, cachedErr := captureIgnorePolicy(repo, nil, nil)
	if cachedErr == nil {
		content := ""
		if policy != nil {
			content = string(policy.graphIgnore.content)
		}
		t.Fatalf("capture followed the symlink and took %q as repository-controlled rules", content)
	}
	if policy != nil && strings.Contains(string(policy.graphIgnore.content), "SECRET-RULE-FROM-OUTSIDE") {
		t.Errorf("capture returned the outside file's content alongside its error")
	}
	// One rule, so one message: a reader who sees either failure sees the same
	// reason.
	if uncachedErr.Error() != cachedErr.Error() {
		t.Errorf("the two readers disagree:\n uncached %v\n cached   %v", uncachedErr, cachedErr)
	}
}

// A real, regular .graphignore must still be captured -- the gate refuses the
// link, not the feature.
func TestCaptureStillReadsARegularGraphIgnore(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, graphIgnoreFileName), []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	policy, err := captureIgnorePolicy(repo, nil, nil)
	if err != nil {
		t.Fatalf("captureIgnorePolicy on a regular file: %v", err)
	}
	if !policy.graphIgnore.present || !strings.Contains(string(policy.graphIgnore.content), "vendor/") {
		t.Fatalf("regular .graphignore was not captured: present=%v content=%q", policy.graphIgnore.present, policy.graphIgnore.content)
	}
}
