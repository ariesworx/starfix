package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
)

// idsFlag collects inbox ids, repeated or comma-separated.
type idsFlag []int64

func (l *idsFlag) String() string { return fmt.Sprint([]int64(*l)) }

func (l *idsFlag) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		p = strings.TrimPrefix(strings.TrimSpace(p), "#")
		if p == "" {
			continue
		}
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < 1 {
			return fmt.Errorf("%q is not an inbox id", p)
		}
		*l = append(*l, n)
	}
	return nil
}

func cmdInbox(ctx context.Context, r *runner, args []string) error {
	const usage = "inbox [--all] [-n N] [--ack ID]... [--ack-all]"
	fs := r.newFlags("inbox")
	var in proto.InboxArgs
	var ids idsFlag
	fs.BoolVar(&in.All, "all", false, "include items already read")
	fs.IntVar(&in.Limit, "n", 0, "how many to list (default 20, at most 100)")
	fs.Var(&ids, "ack", "mark these items read (repeatable, or comma-separated)")
	ackAll := fs.Bool("ack-all", false, "mark every item read")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "inbox takes no arguments; give ids with --ack")
	}
	if len(ids) > 0 || *ackAll {
		if len(ids) > 0 && *ackAll {
			return usagef(usage, "give --ack or --ack-all, not both")
		}
		if in.All || in.Limit != 0 {
			return usagef(usage, "--all and -n list; they do not go with --ack")
		}
		var out proto.AckResult
		if err := r.call(ctx, proto.OpAck, proto.AckArgs{IDs: ids, All: *ackAll}, &out); err != nil {
			return err
		}
		if r.json {
			r.emit(out)
			return nil
		}
		_, _ = fmt.Fprintf(r.env.Stdout, "acked %d\n", out.Acked)
		return nil
	}
	var out proto.InboxResult
	if err := r.call(ctx, proto.OpInbox, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	for _, it := range out.Items {
		printItem(r.env.Stdout, it)
	}
	switch {
	case out.Unread == 0 && len(out.Items) == 0:
		_, _ = fmt.Fprintln(r.env.Stdout, "no unread items")
	case out.Unread > 0:
		_, _ = fmt.Fprintf(r.env.Stdout, "%d unread; `sfx inbox --ack ID` marks one read\n", out.Unread)
	}
	return nil
}

// printItem prints one inbox item on one line:
// #12 claim.lost sf-a1b2 from bob, 2026-10-07 12:00 UTC: taken by bob/s-b.
func printItem(w io.Writer, it proto.InboxItem) {
	line := fmt.Sprintf("#%d %s", it.ID, esc(it.Kind))
	if it.Issue != "" {
		line += " " + esc(it.Issue)
	}
	line += fmt.Sprintf(" from %s, %s", esc(it.From), when(it.At))
	if it.ReadAt != nil {
		line += " (read)"
	}
	_, _ = fmt.Fprintf(w, "%s: %s\n", line, esc(it.Body))
}

// watchLine is one line of `sfx watch --json`.
type watchLine struct {
	Op   string           `json:"op"`
	Item *proto.InboxItem `json:"item,omitempty"`
}

// watchBuffer is how many pushed events `sfx watch` holds while printing.
const watchBuffer = 256

// cmdWatch prints each inbox item the server pushes, as it arrives, until
// interrupted. With --json each event is one JSON object on its own line:
// {"op":"inbox","item":{…}}, or {"op":"resync"} when items were missed
// (`sfx inbox` lists them). After a resync it watches again.
func cmdWatch(ctx context.Context, r *runner, args []string) error {
	const usage = "watch"
	fs := r.newFlags("watch")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "watch takes no arguments")
	}
	events := make(chan proto.Push, watchBuffer)
	r.onPush = func(p proto.Push) {
		select {
		case events <- p:
		default: // printing fell behind: say so as the server would
			select {
			case events <- proto.Push{Op: proto.EvResync}:
			default:
			}
		}
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	var w proto.WatchResult
	if err := c.Call(ctx, proto.OpWatch, proto.WatchArgs{}, &w); err != nil {
		return err
	}
	// On standard error, so --json output stays one object per line.
	_, _ = fmt.Fprintf(r.env.Stderr, "sfx: watching your inbox (%d unread); ctrl-c stops\n", w.Unread)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.Done():
			if ctx.Err() != nil { // interrupted: the connection went with it
				return nil
			}
			if err := c.Err(); err != nil {
				return err
			}
			return proto.Errf(proto.CodeUnavailable, "run `sfx watch` again", "the connection to the server ended")
		case p := <-events:
			switch {
			case r.json:
				r.emit(watchLine{Op: p.Op, Item: p.Item})
			case p.Item != nil:
				printItem(r.env.Stdout, *p.Item)
			case p.Op == proto.EvResync:
				_, _ = fmt.Fprintln(r.env.Stdout, "resync: some items were not shown; `sfx inbox` lists them")
			}
			if p.Op == proto.EvResync {
				if err := c.Call(ctx, proto.OpWatch, proto.WatchArgs{}, nil); err != nil && ctx.Err() == nil {
					return err
				}
			}
		}
	}
}
