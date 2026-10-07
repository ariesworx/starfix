package cli

import (
	"context"
	"io"

	"github.com/ariesworx/starfix/internal/mcpserver"
)

func cmdPrime(ctx context.Context, r *runner, args []string) error {
	const usage = "prime"
	fs := r.newFlags("prime")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "prime takes no arguments")
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	p, err := mcpserver.BuildPrime(ctx, c, r.env.Version)
	if err != nil {
		return err
	}
	if r.json {
		r.emit(p)
		return nil
	}
	_, _ = io.WriteString(r.env.Stdout, p.Text())
	return nil
}

// cmdMCP serves MCP on stdin and stdout until the agent disconnects. It
// connects to starfixd on the first tool call, not at start, so a broken
// config shows up as a tool error the agent can relay rather than as a
// server that failed to start.
func cmdMCP(ctx context.Context, r *runner, args []string) error {
	const usage = "mcp"
	fs := r.newFlags("mcp")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "mcp takes no arguments")
	}
	opts := r.clientOptions()
	srv := mcpserver.New(mcpserver.Options{Version: r.env.Version,
		Dial: func(ctx context.Context) (mcpserver.Conn, error) {
			return mcpserver.DialRepo(ctx, r.dir, opts)
		}})
	return srv.Serve(ctx, io.NopCloser(r.env.Stdin), nopWriteCloser{r.env.Stdout})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
