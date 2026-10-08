package mcpserver

import (
	"context"
	"time"

	"github.com/ariesworx/starfix/internal/gitx"
)

// Files to issues (design §12 item 3), client side: the paths an issue's
// work touched are read from git (gitx.IssuePaths), by sfx, and sent with
// renew, finish and handoff. The agent never passes them, so no tool
// schema carries them.

// DefaultPathsEvery is how often Renew sends each held issue's paths: a
// renewal runs every minute, and git need not.
const DefaultPathsEvery = 5 * time.Minute

// paths returns the paths the work on id touched, from the server's
// repository, or nil when it has none.
func (s *Server) paths(ctx context.Context, id string) []string {
	if s.opts.Dir == "" {
		return nil
	}
	return gitx.IssuePaths(ctx, s.opts.Dir, []string{id})[id]
}
