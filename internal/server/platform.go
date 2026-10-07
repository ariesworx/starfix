package server

import "errors"

// ErrNoPeerCheck is RequirePeerCheck's refusal off Linux, unless dev mode
// is on.
var ErrNoPeerCheck = errors.New("starfixd serve runs on Linux only: elsewhere nothing checks which user " +
	"connects to the socket; fix: run it on Linux (WSL2 on Windows), or pass --dev on a single-user machine")

// RequirePeerCheck refuses to serve where CheckPeer cannot verify the
// connecting user, unless dev mode accepts that risk.
func RequirePeerCheck(dev bool) error { return requirePeerCheck(PeerChecked, dev) }

// requirePeerCheck is RequirePeerCheck with the platform's answer as a
// parameter, so a test on any platform can reach both outcomes.
func requirePeerCheck(checked, dev bool) error {
	if checked || dev {
		return nil
	}
	return ErrNoPeerCheck
}
