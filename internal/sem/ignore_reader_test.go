package sem

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoreSurfacesShareExactByteLimit(t *testing.T) {
	repo := t.TempDir()
	ignore := filepath.Join(repo, "bounded.ignore")
	exact := bytes.Repeat([]byte("# x\n"), maxIgnoreFileBytes/4)
	if len(exact) != maxIgnoreFileBytes {
		t.Fatalf("fixture length = %d, want %d", len(exact), maxIgnoreFileBytes)
	}
	if err := os.WriteFile(ignore, exact, 0o600); err != nil {
		t.Fatal(err)
	}

	var matcher ignoreMatcher
	if err := matcher.loadRequired(ignore, false, callerIgnoreOrigin("explicit-ignore")); err != nil {
		t.Fatalf("loader rejected exact limit: %v", err)
	}
	options := ProviderSnapshotOptions{IgnoreFiles: []string{ignore}}
	for _, keyer := range ignoreInputKeyers(repo) {
		if _, err := keyer.key(options); err != nil {
			t.Fatalf("%s key rejected exact limit: %v", keyer.name, err)
		}
	}

	if err := os.WriteFile(ignore, append(exact, '#'), 0o600); err != nil {
		t.Fatal(err)
	}
	matcher = ignoreMatcher{}
	if err := matcher.loadRequired(ignore, false, callerIgnoreOrigin("explicit-ignore")); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("loader cap+1 error = %v", err)
	}
	for _, keyer := range ignoreInputKeyers(repo) {
		if _, err := keyer.key(options); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("%s key cap+1 error = %v", keyer.name, err)
		}
	}
}

func TestIgnoreSurfacesAllowSymlinkToRegularFile(t *testing.T) {
	repo := t.TempDir()
	target := filepath.Join(repo, "shared-ignore")
	linked := filepath.Join(repo, "linked-ignore")
	if err := os.WriteFile(target, []byte("vendor/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var matcher ignoreMatcher
	if err := matcher.loadRequired(linked, false, callerIgnoreOrigin("explicit-ignore")); err != nil {
		t.Fatalf("loader rejected regular symlink target: %v", err)
	}
	if !matcher.Ignored("vendor/pkg/file.go", false) {
		t.Fatal("loader did not apply rule read through symlink")
	}
	options := ProviderSnapshotOptions{IgnoreFiles: []string{linked}}
	for _, keyer := range ignoreInputKeyers(repo) {
		before, err := keyer.key(options)
		if err != nil {
			t.Fatalf("%s key rejected regular symlink target: %v", keyer.name, err)
		}
		if err := os.WriteFile(target, []byte("dist/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		after, err := keyer.key(options)
		if err != nil {
			t.Fatalf("%s key rejected edited regular symlink target: %v", keyer.name, err)
		}
		if before == after {
			t.Fatalf("%s key ignored an edit through a symlink", keyer.name)
		}
		if err := os.WriteFile(target, []byte("vendor/\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadOpenedBoundedRegularFileRejectsIdentityAndGrowth(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.WriteFile(first, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("same"), 0o600); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOpenedBoundedRegularFile(opened, expected, first, "ignore file", 4); err == nil ||
		!strings.Contains(err.Error(), "changed while opening") {
		t.Fatalf("identity-swap error = %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	expected, err = os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	opened, err = os.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := os.WriteFile(first, []byte("same+"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOpenedBoundedRegularFile(opened, expected, first, "ignore file", 4); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("post-stat growth error = %v", err)
	}
}

func TestOpenRootBoundedRegularFileRejectsEscapingAncestorSymlink(t *testing.T) {
	repo := t.TempDir()
	external := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "nested", ".gitignore"), []byte("inside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, ".gitignore"), []byte("outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// Model the race after the confined Lstat: replace an already-inspected
	// ancestor with a symlink that points outside the repository.
	if err := os.RemoveAll(filepath.Join(repo, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(repo, "nested")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	opened, err := openRootBoundedRegularFile(root, filepath.Join("nested", ".gitignore"))
	if opened != nil {
		_ = opened.Close()
	}
	if err == nil {
		t.Fatal("root-confined nested ignore open followed an escaping ancestor symlink")
	}
}

func TestCacheKeysPreserveIgnoreInputOrderAndRepeatability(t *testing.T) {
	repo := t.TempDir()
	for name, content := range map[string]string{
		"first.ignore":  "target.go\n",
		"second.ignore": "!target.go\n",
	} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	forward := ProviderSnapshotOptions{IgnoreFiles: []string{"first.ignore", "second.ignore"}}
	reverse := ProviderSnapshotOptions{IgnoreFiles: []string{"second.ignore", "first.ignore"}}
	for _, keyer := range ignoreInputKeyers(repo) {
		first, err := keyer.key(forward)
		if err != nil {
			t.Fatal(err)
		}
		repeat, err := keyer.key(forward)
		if err != nil {
			t.Fatal(err)
		}
		if repeat != first {
			t.Fatalf("%s key was not deterministic", keyer.name)
		}
		reversed, err := keyer.key(reverse)
		if err != nil {
			t.Fatal(err)
		}
		if reversed == first {
			t.Fatalf("%s key erased semantic ignore-file order", keyer.name)
		}
	}
}
