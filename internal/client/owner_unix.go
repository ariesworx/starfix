//go:build unix

package client

import (
	"io/fs"
	"os"
	"syscall"
)

// OwnedByUser reports whether the current user owns the file fi
// describes.
func OwnedByUser(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Getuid())
}

// groupOrWorldWritable reports whether anyone but the owner may write
// the file.
func groupOrWorldWritable(fi fs.FileInfo) bool { return fi.Mode().Perm()&0o022 != 0 }
