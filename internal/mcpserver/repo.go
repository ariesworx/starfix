package mcpserver

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// RepoConn is a *client.Conn that knows its repository.
type RepoConn struct {
	*client.Conn
	name string
}

// Project is the repository directory's name: more use to an agent than
// the project UUID.
func (c *RepoConn) Project() string { return c.name }

// DialRepo connects to the server named by the .starfix.yaml at or above
// dir. The config is read on every dial, so a fix to it takes effect on
// the next tool call without restarting the agent.
func DialRepo(ctx context.Context, dir string, opts client.Options) (*RepoConn, error) {
	cfg, err := client.LoadConfig(dir)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return nil, err
		}
		return nil, proto.Errf(proto.CodeInvalid, "correct "+client.ConfigFile+"; see the README's quick start", err.Error())
	}
	c, err := client.Dial(ctx, cfg, opts)
	if err != nil {
		return nil, err
	}
	return &RepoConn{Conn: c, name: filepath.Base(cfg.Root)}, nil
}
