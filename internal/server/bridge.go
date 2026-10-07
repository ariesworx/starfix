package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Bridge is `starfixd stdio`, the forced command of every developer key. It
// connects to the daemon's socket, sends a bridge frame naming principal
// (fixed by sshd from the key that authenticated), then copies in to the
// daemon and the daemon to out until the daemon closes the connection.
//
// When the daemon cannot be reached, Bridge writes a refusing welcome to
// out, so the client reports a typed error rather than a dropped session.
func Bridge(ctx context.Context, socket, principal string, in io.Reader, out io.Writer) error {
	if !PrincipalPattern.MatchString(principal) || store.Reserved(principal) {
		e := proto.Errf(proto.CodeAuth, "the server admin should fix this key's authorized_keys line",
			"this key's forced command names an invalid or reserved principal")
		return errors.Join(e, refuse(out, e))
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		e := proto.Errf(proto.CodeUnavailable, "ask the server admin to start it (`systemctl start starfixd`)",
			"starfixd is not running on the server")
		return errors.Join(fmt.Errorf("connect to %s: %w", socket, err), refuse(out, e))
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	if err := proto.NewEncoder(c).Encode(&proto.Frame{T: proto.FrameBridge, Principal: principal}); err != nil {
		return err
	}
	go func() {
		_, _ = io.Copy(c, in)
		if uc, ok := c.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	if _, err := io.Copy(out, c); err != nil && ctx.Err() == nil {
		return fmt.Errorf("bridge: %w", err)
	}
	return nil
}

func refuse(out io.Writer, e *proto.Error) error {
	return proto.NewEncoder(out).Encode(&proto.Frame{T: proto.FrameWelcome, Err: e})
}
