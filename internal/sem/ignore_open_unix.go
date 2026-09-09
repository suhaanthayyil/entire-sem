//go:build !windows

package sem

import (
	"os"
	"syscall"
)

// openBoundedRegularFile uses a non-blocking open so a path raced from a
// regular file to a writerless FIFO cannot park before the descriptor-level
// type and identity checks run. POSIX ignores O_NONBLOCK for regular files.
func openBoundedRegularFile(file string) (*os.File, error) {
	return os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// openRepoIgnoreFile opens a REPOSITORY-controlled ignore file (.gitignore,
// .graphignore) without traversing a final-component symlink. The caller has
// already Lstat'd this path and refused a link; O_NOFOLLOW is what makes that
// refusal hold for the object actually read, because between the Lstat and the
// open a process writing in the repository can replace the regular file with a
// link and a following open would hand an outside file's lines to the
// repo_ignored disclosure as the repository's own rules. O_NONBLOCK for the same
// reason openBoundedRegularFile uses it.
func openRepoIgnoreFile(file string) (*os.File, error) {
	return os.OpenFile(file, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}

func openRootBoundedRegularFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func openedFileIsRegular(_ *os.File, info os.FileInfo) (bool, error) {
	return info.Mode().IsRegular(), nil
}
