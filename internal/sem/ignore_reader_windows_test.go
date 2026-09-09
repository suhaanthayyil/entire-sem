//go:build windows

package sem

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestIgnoreSurfacesRejectWindowsNULDevice(t *testing.T) {
	repo := t.TempDir()
	device := filepath.Join(repo, "NUL")
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
				t.Fatalf("NUL device error = %v", err)
			}
		})
	}
}
