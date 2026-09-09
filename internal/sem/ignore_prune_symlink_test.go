package sem

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrunedReadRefusesADirectorySwappedForASymlink is the review finding.
//
// The pruned-directory accounting enumerates an ignored tree so the disclosure
// can say what a repository rule removed, and it NAMES what it enumerates:
// every entry becomes a repo_ignored.sample[].path. It reached that tree with a
// plain os.Open, which re-resolves the path the outer walk had already
// classified as a directory. A directory replaced by a symlink in between was
// therefore followed, and the filenames of whatever it pointed at — outside the
// checkout — were emitted as paths the repository's own ignore rules had
// removed.
//
// The fix is the one directoryReadable already applies to its own probe: reach
// the directory through the held os.Root, so the object validated and the object
// read are the same one and no component can resolve outside the repository.
// This test puts the filesystem in the state the race produces rather than
// trying to win the race.
func TestPrunedReadRefusesADirectorySwappedForASymlink(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside-secret.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "hidden")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	ledger := &repoIgnoreLedger{}
	entries, err := readDirBounded(ledger, root, repo, filepath.Join(repo, "hidden"))
	if err == nil {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the prune accounting enumerated %d entries through a symlinked directory and would"+
			" have disclosed them as repository exclusions: %q", len(entries), names)
	}

	// Positive control: a real directory in the same repository still enumerates,
	// so the assertion above is about the symlink and not about the confinement
	// refusing everything.
	write(t, repo, "real/keep.go", "package real\n")
	entries, err = readDirBounded(ledger, root, repo, filepath.Join(repo, "real"))
	if err != nil {
		t.Fatalf("a real directory inside the repository must still be readable: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep.go" {
		t.Fatalf("entries = %+v, want the one real child", entries)
	}
}
