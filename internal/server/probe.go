package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// ProbePrincipal is the principal a health probe connects as.
const ProbePrincipal = "starfixd-upgrade"

// Probe connects to the daemon's socket as the daemon's own user, as the
// bridge does, completes the handshake for project and returns the
// daemon's version. It is the health check after an upgrade: a daemon that
// answers a welcome has opened its store and applied its migrations.
// Probe gives up after 10 seconds.
func Probe(ctx context.Context, socket, project, clientVersion string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return "", fmt.Errorf("connect to %s: %w", socket, err)
	}
	defer func() { _ = c.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	enc := proto.NewEncoder(c)
	if err := enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: ProbePrincipal}); err != nil {
		return "", err
	}
	if err := enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Version: clientVersion,
		Project: project, Session: "upgrade-probe", Machine: "localhost"}); err != nil {
		return "", err
	}
	w, err := proto.NewDecoder(c).Decode()
	if err != nil {
		return "", fmt.Errorf("handshake: %w", err)
	}
	if w.T != proto.FrameWelcome {
		return "", fmt.Errorf("handshake: got a %q frame, not a welcome", w.T)
	}
	if w.Err != nil {
		return "", errors.Join(errors.New("handshake refused"), w.Err)
	}
	return w.Version, nil
}
