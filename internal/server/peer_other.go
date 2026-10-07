//go:build !linux

package server

import (
	"io/fs"
	"net"
)

// CheckPeer accepts every connection: off Linux, the 0700 socket directory
// is what keeps other users out.
func CheckPeer(net.Conn) error { return nil }

// PeerChecked reports whether CheckPeer verifies the connecting user.
const PeerChecked = false

func ownedByMe(fs.FileInfo) error { return nil }
