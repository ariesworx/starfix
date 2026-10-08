package cli

import (
	"context"
	"fmt"

	"github.com/ariesworx/starfix/internal/board"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// cmdTUI shows the live board (internal/board) until q. It needs a
// terminal on standard input and output that is not TERM=dumb, and
// refuses anything else before it dials, pointing at the listing
// commands. The first connection is made before the board takes the
// terminal, so a refusal prints as any command's does; the ones after it
// are the board's to redial.
func cmdTUI(ctx context.Context, r *runner, args []string) error {
	const usage = "tui"
	fs := r.newFlags("tui")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "tui takes no arguments")
	}
	if r.json {
		return usagef(usage, "tui draws a board and prints no JSON; `sfx ready --json` and `sfx list --json` do")
	}
	const notHere = "run it in a terminal; for a list that does not update, `sfx ready`, `sfx blocked` or `sfx list`"
	// Emacs's shell and some CI runners are terminals, but say with
	// TERM=dumb that they cannot move the cursor the board needs.
	if r.env.Getenv("TERM") == "dumb" {
		return proto.Errf(proto.CodeInvalid, notHere, "sfx tui needs a terminal that can move the cursor, and TERM is dumb")
	}
	if !board.IsTerminal(r.env.Stdin, r.rawOut) {
		return proto.Errf(proto.CodeInvalid, notHere, "sfx tui needs a terminal, and standard input or output is not one")
	}
	live := board.NewLive(func(ctx context.Context, onPush func(proto.Push)) (board.Conn, error) {
		opts := r.clientOptions()
		opts.OnPush = onPush
		c, err := mcpserver.DialRepo(ctx, r.dir, opts)
		if err != nil {
			return nil, err // not a nil *RepoConn in a non-nil Conn
		}
		return c, nil
	})
	conn, err := live.Connect(ctx)
	if err != nil {
		return err
	}
	rc := conn.(*mcpserver.RepoConn) // the only Conn the dialer above returns
	if w := rc.Warning(r.env.Version); w != "" {
		// Shown again once the board gives the screen back.
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\n", esc(w))
		r.flush()
	}
	screen, err := board.OpenScreen(r.env.Stdin, r.rawOut)
	if err != nil {
		_ = conn.Close()
		return proto.Errf(proto.CodeUnavailable, "run it in another terminal, or use `sfx ready` and `sfx list`",
			fmt.Sprintf("cannot take over the terminal: %v", err))
	}
	t, stop := screen.Terminal(r.env.Getenv("NO_COLOR") == "")
	defer stop()
	title := rc.Project()
	if p := rc.Principal(); p != "" {
		title += " as " + p
	}
	return board.Run(ctx, title, t, live, conn)
}
