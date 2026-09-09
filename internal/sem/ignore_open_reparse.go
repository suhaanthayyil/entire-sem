package sem

import "os"

// windowsFileFlagOpenReparsePoint is CreateFile's FILE_FLAG_OPEN_REPARSE_POINT.
// With it set, a final component that is a reparse point — a symlink, a
// directory junction, a mount point, an AppExec link — is OPENED ITSELF rather
// than traversed, which is what Windows offers in place of the O_NOFOLLOW the
// Unix side uses.
//
// It is spelled here, in a file with no build tag, rather than taken from
// syscall on the Windows leg alone, so the DECISION to set it is compiled and
// asserted on every host. ignore_open_windows.go pins the literal to
// syscall.FILE_FLAG_OPEN_REPARSE_POINT at compile time, so a wrong value is a
// build failure on Windows rather than an open that quietly follows.
const windowsFileFlagOpenReparsePoint = 0x00200000

// repoIgnoreOpenFlagsWindows is the flag word openRepoIgnoreFile hands
// os.OpenFile on Windows. Go's syscall.Open reads FILE_FLAG_* out of the high 12
// bits of the flag and passes them to CreateFile, so this is how a no-follow open
// is expressed there.
//
// Read-only, because this reads a repository's ignore rules and nothing more,
// and no-follow, because the caller has already Lstat'd the path and refused a
// link: without this flag the open resolves the path a SECOND time, and a link
// raced in between the two is followed — the check and the use stop being the
// same object. On Windows that link may name a UNC share, so the following open
// contacts a remote host before os.SameFile ever gets to reject what came back,
// and the no-egress contract is broken by the open itself rather than by the
// bytes it returns.
func repoIgnoreOpenFlagsWindows() int {
	return os.O_RDONLY | windowsFileFlagOpenReparsePoint
}
