package sem

import (
	"errors"
	"fmt"
	"testing"
)

func TestSweepQueueCapacityDoesNotOverflowAtMaxInt(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if got := sweepQueueCapacity(0, maxInt); got != 1 {
		t.Fatalf("sweepQueueCapacity(0, MaxInt) = %d, want 1", got)
	}
	if got := sweepQueueCapacity(2, maxInt); got != 3 {
		t.Fatalf("sweepQueueCapacity(2, MaxInt) = %d, want 3", got)
	}
}

func TestTrackedDirSetBoundsAncestorExpansion(t *testing.T) {
	files := []string{"a/b/file.go", "c/file.go"}
	dirs, err := trackedDirSetFromFiles(files, 3, 5)
	if err != nil {
		t.Fatalf("exact tracked-directory bounds were rejected: %v", err)
	}
	for _, want := range []string{"a/b", "a", "c"} {
		if _, ok := dirs[want]; !ok {
			t.Errorf("tracked directory %q missing from %v", want, dirs)
		}
	}
	if _, err := trackedDirSetFromFiles(files, 2, 5); !errors.Is(err, errTrackedDirectoryBound) {
		t.Fatalf("tracked directory count overflow err = %v, want errTrackedDirectoryBound", err)
	}
	if _, err := trackedDirSetFromFiles(files, 3, 4); !errors.Is(err, errTrackedDirectoryBound) {
		t.Fatalf("tracked directory byte overflow err = %v, want errTrackedDirectoryBound", err)
	}
}

func TestWorktreeWalkBudgetExactBoundaries(t *testing.T) {
	rawCount := worktreeWalkBudget{rawEntries: maxWorktreeWalkRawEntries - 1}
	if !rawCount.admitRawEntry("x") || rawCount.admitRawEntry("y") {
		t.Fatal("raw-entry count boundary was not enforced exactly")
	}
	rawBytes := worktreeWalkBudget{rawBytes: maxWorktreeWalkRawBytes - 2}
	if !rawBytes.admitRawEntry("x") || rawBytes.admitRawEntry("") {
		t.Fatal("raw-entry aggregate-byte boundary was not enforced exactly")
	}
	dirCount := worktreeWalkBudget{directories: maxWorktreeWalkDirectories - 1}
	if !dirCount.admitDirectory("x") || dirCount.admitDirectory("y") {
		t.Fatal("directory count boundary was not enforced exactly")
	}
	dirBytes := worktreeWalkBudget{directoryBytes: maxWorktreeWalkDirectoryBytes - 2}
	if !dirBytes.admitDirectory("x") || dirBytes.admitDirectory("") {
		t.Fatal("directory aggregate-byte boundary was not enforced exactly")
	}
}

func TestFilesystemWalkReadsMoreThanOneDirectoryBatch(t *testing.T) {
	repo := t.TempDir()
	for index := 0; index < 300; index++ {
		writeFile(t, repo, fmt.Sprintf("p-%03d.go", index), "package sample\n")
	}
	paths, _, err := walkWorktreeFiles(t.Context(), repo, ignoreMatcher{}, func(string) bool { return false }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 300 {
		t.Fatalf("batched filesystem walk returned %d paths, want 300", len(paths))
	}
}
