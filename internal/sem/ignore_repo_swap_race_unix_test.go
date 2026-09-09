//go:build !windows

package sem

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenRepoIgnoreFileWillNotTraverseALink is the STRUCTURAL half of the
// swap-race pin: it fixes the property the race test can only observe
// statistically, that the OPEN itself refuses a link rather than trusting an
// earlier stat of the path. Everything upstream of it -- the Lstat gate, the
// order of the two readers -- can be rearranged; a link must still never be
// opened here.
func TestOpenRepoIgnoreFileWillNotTraverseALink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.env")
	if err := os.WriteFile(outside, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, graphIgnoreFileName)
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("filesystem does not support symlinks: %v", err)
	}

	opened, err := openRepoIgnoreFile(link)
	if err == nil {
		name := opened.Name()
		_ = opened.Close()
		t.Fatalf("openRepoIgnoreFile followed a symlinked %s to %q", graphIgnoreFileName, name)
	}

	// The refusal is the link, not the reader: a regular file still opens.
	regular := filepath.Join(dir, "regular"+graphIgnoreFileName)
	if err := os.WriteFile(regular, []byte("vendor/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opened, err = openRepoIgnoreFile(regular)
	if err != nil {
		t.Fatalf("openRepoIgnoreFile refused a regular file: %v", err)
	}
	_ = opened.Close()
}
