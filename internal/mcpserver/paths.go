package mcpserver

import (
	"context"
	"time"

	"github.com/ariesworx/starfix/internal/gitx"
	"github.com/ariesworx/starfix/internal/proto"
)

// Files to issues (design §12 item 3), client side: the paths an issue's
// work touched are read from git here, by sfx, and sent with renew,
// finish and handoff. The agent never passes them, so no tool schema
// carries them.

// DefaultPathsEvery is how often Renew sends each held issue's paths: a
// renewal runs every minute, and git need not.
const DefaultPathsEvery = 5 * time.Minute

// gitTimeout bounds one look at the repository, so a slow one never holds
// up a renewal or a finish.
const gitTimeout = 5 * time.Second

// RepoPaths returns the paths the work on each issue in ids touched, in
// the repository holding dir (gitx.ReadChanges): only paths the server
// accepts (proto.CheckPath, and no directory prefix), most recent first,
// at most proto.MaxPaths in all, filled in ids' order. With no ids it
// takes the issues the repository names (gitx.Changes.IDs). It returns
// nil when git tells nothing: no git, no repository, a detached HEAD, or a
// look that takes longer than 5 seconds.
func RepoPaths(ctx context.Context, dir string, ids []string) map[string][]string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	ch, err := gitx.ReadChanges(ctx, dir)
	if err != nil {
		return nil
	}
	if ids == nil {
		ids = ch.IDs()
	}
	var out map[string][]string
	budget := proto.MaxPaths
	for _, id := range ids {
		var ps []string
		for _, p := range ch.Paths(id) {
			if len(ps) == budget {
				break
			}
			if proto.CheckPath(p) == nil && !proto.IsPathPrefix(p) {
				ps = append(ps, p)
			}
		}
		if len(ps) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[id] = ps
		budget -= len(ps)
	}
	return out
}

// paths returns the paths the work on id touched, from the server's
// repository, or nil when it has none.
func (s *Server) paths(ctx context.Context, id string) []string {
	if s.opts.Dir == "" {
		return nil
	}
	return RepoPaths(ctx, s.opts.Dir, []string{id})[id]
}
