//go:build windows

package sem

import (
	"os"
	"syscall"
)

func openBoundedRegularFile(file string) (*os.File, error) {
	return os.Open(file)
}

// windowsFileFlagOpenReparsePoint is CreateFile's own constant. Asserted rather
// than imported so the neutral spelling the decision function uses cannot drift
// from it: a mismatch is a build failure on this leg, not an open that follows.
var _ [0]struct{} = [windowsFileFlagOpenReparsePoint - syscall.FILE_FLAG_OPEN_REPARSE_POINT]struct{}{}

// openRepoIgnoreFile opens a REPOSITORY-controlled ignore file (.gitignore,
// .graphignore) without traversing a final-component reparse point, which is
// Windows' form of the Unix side's O_NOFOLLOW. Go's syscall.Open takes FILE_FLAG_*
// out of the high bits of the flag word and hands them to CreateFile, so
// FILE_FLAG_OPEN_REPARSE_POINT opens a raced-in symlink, junction or mount point
// AS ITSELF and the caller's os.SameFile check then refuses it.
//
// It has to be the open, not the check after it. os.Open resolved the path a
// second time, and on Windows the reparse point a repository can race over
// `.graphignore` may name a UNC share: following it CONTACTS a remote host, and
// no verdict reached afterwards takes that back. os.SameFile still runs — it is
// what refuses a plain regular file swapped for another — but it is no longer
// the only thing standing between a repository-authored link and an egress.
//
// What this does NOT buy, and the Unix side does not either: the flag governs
// the FINAL component only. An intermediate directory replaced by a junction is
// still traversed, and containment for the path above the file rests on the
// caller resolving the repository root once.
func openRepoIgnoreFile(file string) (*os.File, error) {
	return os.OpenFile(file, repoIgnoreOpenFlagsWindows(), 0)
}

func openRootBoundedRegularFile(root *os.Root, name string) (*os.File, error) {
	return root.Open(name)
}

// Windows reports reserved DOS devices such as NUL as regular through parts of
// the os.Stat surface. Check the opened handle too, so only disk files reach the
// bounded read.
func openedFileIsRegular(file *os.File, info os.FileInfo) (bool, error) {
	if !info.Mode().IsRegular() {
		return false, nil
	}
	fileType, err := syscall.GetFileType(syscall.Handle(file.Fd()))
	if err != nil {
		return false, err
	}
	return fileType == syscall.FILE_TYPE_DISK, nil
}
