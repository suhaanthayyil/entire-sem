package sem

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const swapRaceSecretRule = "SECRET-OUTSIDE-RULE"

// TestRepoIgnoreReadersRefuseASymlinkSwappedInAfterTheCheck is a RACE
// REPRODUCTION, not a structural stand-in: it runs the real readers against a
// .graphignore that another goroutine is renaming between a regular file and a
// symlink to a file outside the repository, exactly as a process writing in the
// repository could.
//
// Both repository-controlled readers used to decide on one object and read
// another -- an os.Lstat that refused a link, then a reader that resolved the
// path a SECOND time and followed whatever was there by then. Against that code
// this test observes the outside file's line arriving as a repository-authored
// ignore rule (through loadPath's matcher, and through the cached path's
// captured content) within a few thousand attempts, in well under a second. The
// pattern text of a repository rule is echoed in the repo_ignored disclosure, so
// each observation is an arbitrary local-file read.
//
// The read must now come from the descriptor that was checked, so no swap can be
// observed at all.
func TestRepoIgnoreReadersRefuseASymlinkSwappedInAfterTheCheck(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.env")
	if err := os.WriteFile(outside, []byte(swapRaceSecretRule+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, graphIgnoreFileName)
	if err := os.WriteFile(target, []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(parent, "probe.link")); err != nil {
		t.Skipf("filesystem does not support symlinks: %v", err)
	}

	// The swapper only ever renames a complete object over the path, so the
	// readers always see a valid .graphignore -- either the repository's regular
	// file or a link. Nothing here depends on a partially written file.
	var stop atomic.Bool
	swapperDone := make(chan struct{})
	go func() {
		defer close(swapperDone)
		linkStage := filepath.Join(parent, "stage.link")
		fileStage := filepath.Join(parent, "stage.file")
		for !stop.Load() {
			_ = os.Remove(linkStage)
			if err := os.Symlink(outside, linkStage); err != nil {
				return
			}
			if err := os.Rename(linkStage, target); err != nil {
				return
			}
			if err := os.WriteFile(fileStage, []byte("vendor/\n"), 0o644); err != nil {
				return
			}
			if err := os.Rename(fileStage, target); err != nil {
				return
			}
		}
	}()
	defer func() {
		stop.Store(true)
		<-swapperDone
	}()

	const minAttempts = 30000
	deadline := time.Now().Add(10 * time.Second)
	attempts, legitimateReads := 0, 0
	var leaks []string
	for attempts < minAttempts && time.Now().Before(deadline) {
		attempts++

		var matcher ignoreMatcher
		if err := matcher.loadPath(target, false, false, graphIgnoreOrigin()); err == nil {
			for _, rule := range matcher.rules {
				switch {
				case strings.Contains(rule.pattern, swapRaceSecretRule):
					leaks = append(leaks, "loadPath took the outside file's line as a repository rule: "+rule.pattern)
				case strings.Contains(rule.pattern, "vendor"):
					legitimateReads++
				}
			}
		}

		policy, err := captureIgnorePolicy(repo, nil, nil)
		if err == nil && policy != nil {
			captured := string(policy.graphIgnore.content)
			switch {
			case strings.Contains(captured, swapRaceSecretRule):
				leaks = append(leaks, "captureIgnorePolicy captured the outside file as repository rules: "+captured)
			case strings.Contains(captured, "vendor"):
				legitimateReads++
			}
		}
		if len(leaks) > 0 {
			break
		}
	}

	if len(leaks) > 0 {
		t.Fatalf("a .graphignore swapped to a symlink after the check was read anyway (attempt %d of %d):\n%s",
			attempts, minAttempts, strings.Join(leaks, "\n"))
	}
	// Without this the run above could report a clean result for having never
	// reached the reader at all.
	if legitimateReads == 0 {
		t.Fatalf("no attempt in %d ever read the real .graphignore, so nothing was exercised", attempts)
	}
}
