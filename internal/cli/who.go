package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// defaultWho is who's window when --since is not given; the server's
// default (store.AgentActiveFor).
const defaultWho = "5m"

func cmdWho(ctx context.Context, r *runner, args []string) error {
	const usage = "who [--since DURATION]"
	fs := r.newFlags("who")
	var in proto.WhoArgs
	fs.StringVar(&in.Since, "since", "", "how far back to look (default 5m, at most 7d)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "who takes no arguments")
	}
	if in.Since != "" {
		if _, err := proto.ParseDuration(in.Since); err != nil {
			return usagef(usage, "%q: %v", in.Since, err)
		}
	}
	var out proto.WhoResult
	if err := r.call(ctx, proto.OpWho, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printWho(r.env.Stdout, in.Since, out)
	return nil
}

// printWho prints one line per agent, most recently seen first:
// principal/session on machine (harness) seen 2m ago, holds ID ID.
func printWho(w io.Writer, since string, res proto.WhoResult) {
	if len(res.Agents) == 0 {
		_, _ = fmt.Fprintf(w, "nobody seen in the last %s\n", cmp.Or(since, defaultWho))
		return
	}
	for _, a := range res.Agents {
		line := esc(a.Principal) + "/" + esc(a.Session) + " on " + esc(a.Machine)
		if a.Harness != "" {
			line += " (" + esc(a.Harness) + ")"
		}
		if ago := res.Now.Sub(a.LastSeen); ago < time.Minute {
			line += " seen just now"
		} else {
			line += " seen " + proto.Span(ago) + " ago"
		}
		if len(a.Claims) > 0 {
			line += ", holds " + strings.Join(escAll(a.Claims), " ")
		}
		_, _ = fmt.Fprintln(w, line)
	}
}
