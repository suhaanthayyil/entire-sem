//go:build !windows

package sem

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestIgnoreSurfacesRejectCharacterDevice(t *testing.T) {
	repo := t.TempDir()
	device := filepath.Join(repo, "device.ignore")
	if err := os.Symlink(os.DevNull, device); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var matcher ignoreMatcher
	checks := []struct {
		name string
		run  func() error
	}{
		{"loader", func() error { return matcher.loadRequired(device, false, callerIgnoreOrigin("explicit-ignore")) }},
		{"search key", func() error {
			_, err := searchSnapshotKey(repo, "repo", "version", "tree", ProviderSnapshotOptions{IgnoreFiles: []string{device}})
			return err
		}},
		{"records key", func() error {
			_, err := providerRecordsKey(repo, "repo", "version", "commit", "tree", "snapshot", ProviderSnapshotOptions{IgnoreFiles: []string{device}})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("device error = %v", err)
			}
		})
	}
}

func TestIgnoreSurfacesRejectWriterlessFIFOWithoutBlocking(t *testing.T) {
	repo := t.TempDir()
	fifo := filepath.Join(repo, "fifo.ignore")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}

	var matcher ignoreMatcher
	checks := []struct {
		name string
		run  func() error
	}{
		{"loader", func() error { return matcher.loadRequired(fifo, false, callerIgnoreOrigin("explicit-ignore")) }},
		{"search key", func() error {
			_, err := searchSnapshotKey(repo, "repo", "version", "tree", ProviderSnapshotOptions{IgnoreFiles: []string{fifo}})
			return err
		}},
		{"records key", func() error {
			_, err := providerRecordsKey(repo, "repo", "version", "commit", "tree", "snapshot", ProviderSnapshotOptions{IgnoreFiles: []string{fifo}})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			returned := make(chan error, 1)
			go func() { returned <- check.run() }()
			select {
			case err := <-returned:
				if err == nil || !strings.Contains(err.Error(), "not a regular file") {
					t.Fatalf("FIFO error = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("ignore surface blocked on a writerless FIFO")
			}
		})
	}
}

func TestNonblockingIgnoreOpenReturnsOnWriterlessFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	type result struct {
		file *os.File
		err  error
	}
	returned := make(chan result, 1)
	go func() {
		file, err := openBoundedRegularFile(fifo)
		returned <- result{file: file, err: err}
	}()
	select {
	case got := <-returned:
		if got.err != nil {
			t.Fatalf("nonblocking FIFO open: %v", got.err)
		}
		defer got.file.Close()
		regular := filepath.Join(dir, "regular")
		if err := os.WriteFile(regular, []byte("rule\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		expected, err := os.Stat(regular)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readOpenedBoundedRegularFile(got.file, expected, regular, "ignore file", 32); err == nil ||
			!strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("raced FIFO handle error = %v", err)
		}
	case <-time.After(2 * time.Second):
		// Unblock a regressed plain read-only open so the test does not leak a
		// parked goroutine before reporting the failure.
		if descriptor, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = syscall.Close(descriptor)
		}
		t.Fatal("regular-file opener blocked before it could reject the FIFO handle")
	}
}

func TestGitCommonDirRejectsWriterlessFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "commondir")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	returned := make(chan bool, 1)
	go func() {
		_, ok := gitCommonDir(dir)
		returned <- ok
	}()
	select {
	case ok := <-returned:
		if ok {
			t.Fatal("gitCommonDir accepted a FIFO commondir")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gitCommonDir blocked on a writerless FIFO")
	}
}
