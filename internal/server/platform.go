package server

import "errors"

// ErrNoPeerCheck is returned by RequirePeerCheck off Linux.
var ErrNoPeerCheck = errors.New("starfixd serve runs on Linux only: elsewhere nothing checks which user " +
	"connects to the socket; fix: run it on Linux (WSL2 on Windows), or pass --dev on a single-user machine")

// RequirePeerCheck refuses to serve where CheckPeer cannot verify the
// connecting user, unless dev mode accepts that risk.
func RequirePeerCheck(dev bool) error { return requirePeerCheck(PeerChecked, dev) }

func requirePeerCheck(checked, dev bool) error {
	if checked || dev {
		return nil
	}
	return ErrNoPeerCheck
}
