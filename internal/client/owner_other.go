//go:build !unix

package client

import "io/fs"

// OwnedByUser reports whether the current user owns the file fi
// describes. Windows keeps ownership in ACLs, which this does not read,
// so it reports true; LoadConfig's search boundary is the guard there.
func OwnedByUser(fs.FileInfo) bool { return true }

// groupOrWorldWritable is false where there are no Unix mode bits.
func groupOrWorldWritable(fs.FileInfo) bool { return false }
