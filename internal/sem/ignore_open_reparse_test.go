package sem

import (
	"os"
	"testing"
)

// TestRepoIgnoreOpenFlagsWindowsRefuseAFinalReparsePoint pins the Windows half of
// "the check and the use are the same object".
//
// The Unix side gets O_NOFOLLOW. Windows has no O_NOFOLLOW, and the open here
// used to be a plain os.Open: it re-resolved the path the Lstat had already
// approved, so a reparse point raced over `.graphignore` between the two was
// FOLLOWED. os.SameFile then refused the bytes, but the refusal comes after the
// open, and on Windows a reparse point may name a UNC path — by then the process
// has already contacted a remote host, which is the no-egress contract broken
// whatever the reader does next.
//
// This asserts the DECISION, which is a pure function and runs on every host. The
// syscall it feeds runs only on Windows: that leg is covered by
// TestOpenRepoIgnoreFileWillNotTraverseALinkOnWindows in the Windows CI job, and
// by nothing on this one.
func TestRepoIgnoreOpenFlagsWindowsRefuseAFinalReparsePoint(t *testing.T) {
	t.Parallel()
	flags := repoIgnoreOpenFlagsWindows()
	if flags&windowsFileFlagOpenReparsePoint == 0 {
		t.Errorf("repoIgnoreOpenFlagsWindows() = %#x, without FILE_FLAG_OPEN_REPARSE_POINT (%#x): the"+
			" open follows a final-component reparse point raced in after the Lstat, so a UNC target is"+
			" contacted before os.SameFile can reject it",
			flags, windowsFileFlagOpenReparsePoint)
	}
	if flags&(os.O_WRONLY|os.O_RDWR) != 0 {
		t.Errorf("repoIgnoreOpenFlagsWindows() = %#x asks for write access to a file it only reads", flags)
	}
	if flags&(os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		t.Errorf("repoIgnoreOpenFlagsWindows() = %#x may create or modify the repository's ignore file", flags)
	}
}
