//go:build windows

package sem

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenRepoIgnoreFileWillNotTraverseALinkOnWindows is the Windows leg of the
// structural pin TestOpenRepoIgnoreFileWillNotTraverseALink makes on Unix: the
// OPEN itself must refuse a reparse point rather than trust an earlier stat of
// the path.
//
// It runs in the Windows CI job and nowhere else. On the developer hosts this
// repository is written on, the only coverage of this decision is
// TestRepoIgnoreOpenFlagsWindowsRefuseAFinalReparsePoint, which asserts the flag
// word and cannot execute the syscall.
//
// Creating a symlink on Windows needs SeCreateSymbolicLinkPrivilege or developer
// mode, so a runner without either skips rather than fails: this is a pin on the
// open, not a test of the runner's privileges.
func TestOpenRepoIgnoreFileWillNotTraverseALinkOnWindows(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.env")
	if err := os.WriteFile(outside, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, graphIgnoreFileName)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("this runner cannot create symlinks: %v", err)
	}

	opened, err := openRepoIgnoreFile(link)
	if err != nil {
		// The open refused outright, which is the strongest outcome.
		return
	}
	defer func() { _ = opened.Close() }()
	// It opened the reparse point ITSELF, so it is not the target: the identity
	// check the caller runs against the Lstat'd file rejects it, and no read ever
	// reached outside.env.
	openedInfo, err := opened.Stat()
	if err != nil {
		return
	}
	targetInfo, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(openedInfo, targetInfo) {
		t.Fatalf("openRepoIgnoreFile followed a symlinked %s to %q; on Windows that target may be a UNC"+
			" share, so the traversal alone is the egress", graphIgnoreFileName, outside)
	}
}

// TestOpenRepoIgnoreFileStillReadsARegularFileOnWindows is the kind-(b) guard:
// the refusal above must be the link, not the reader.
func TestOpenRepoIgnoreFileStillReadsARegularFileOnWindows(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, graphIgnoreFileName)
	if err := os.WriteFile(regular, []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened, err := openRepoIgnoreFile(regular)
	if err != nil {
		t.Fatalf("openRepoIgnoreFile refused a regular file: %v", err)
	}
	_ = opened.Close()
}
