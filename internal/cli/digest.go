package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ariesworx/starfix/internal/proto"
)

func cmdDigest(ctx context.Context, r *runner, args []string) error {
	const usage = "digest [--since 24h|7d|DATE|TIME] [--by PRINCIPAL] [--label L]"
	fs := r.newFlags("digest")
	var in proto.DigestArgs
	fs.StringVar(&in.Since, "since", "", "window start: a duration back from now (24h, 7d), a date or an RFC 3339 time")
	fs.StringVar(&in.By, "by", "", "only what this principal did, and the issues assigned to them")
	fs.StringVar(&in.Label, "label", "", "only issues with this label")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "digest takes no arguments")
	}
	var out proto.DigestResult
	if err := r.call(ctx, proto.OpDigest, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printDigest(r.env.Stdout, out)
	return nil
}

// digestNoteLen is how much of a handoff note the terminal shows.
const digestNoteLen = 60

// printDigest renders a digest: a header line, then each section that has
// anything, with its total and one line per issue listed.
func printDigest(w io.Writer, d proto.DigestResult) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("since %s (%s): %d events", when(d.Since), proto.Span(d.Until.Sub(d.Since)), d.Totals.Events)
	if d.Truncated {
		p(", some totals partial")
	}
	p("\n")
	ago := func(it proto.DigestItem) string {
		return "\t" + esc(it.By) + "\t" + proto.Span(d.Until.Sub(it.At)) + " ago"
	}
	t := d.Totals
	for _, sec := range []struct {
		name  string
		total int
		items []proto.DigestItem
		extra func(proto.DigestItem) string
	}{
		{"closed", t.Closed, d.Closed, ago},
		{"started", t.Started, d.Started, ago},
		{"in progress", t.InProgress, d.InProgress, func(it proto.DigestItem) string {
			return "\t" + esc(it.By) + "\tfor " + proto.Span(d.Until.Sub(it.At))
		}},
		{"stalled", t.Stalled, d.Stalled, func(it proto.DigestItem) string {
			return "\t" + esc(it.By) + "\tidle " + proto.Span(d.Until.Sub(it.At))
		}},
		{"blocked", t.Blocked, d.Blocked, func(it proto.DigestItem) string {
			return "\tby " + strings.Join(escAll(it.BlockedBy), ", ")
		}},
		{"handed off", t.HandedOff, d.HandedOff, func(it proto.DigestItem) string {
			note := strings.Join(strings.Fields(it.Note), " ")
			if len([]rune(note)) > digestNoteLen {
				note = string([]rune(note)[:digestNoteLen]) + "…"
			}
			return ago(it) + "\t" + esc(note)
		}},
		{"created", t.Created, d.Created, ago},
		{"discovered", t.Discovered, d.Discovered, func(it proto.DigestItem) string {
			return ago(it) + "\tfrom " + esc(it.From)
		}},
	} {
		if sec.total == 0 {
			continue
		}
		p("%s %d", sec.name, sec.total)
		if len(sec.items) < sec.total {
			p(", %d shown", len(sec.items))
		}
		p("\n")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, it := range sec.items {
			_, _ = fmt.Fprintf(tw, "  %s\tP%d\t%s%s\n", esc(it.ID), it.Priority, esc(it.Title), sec.extra(it))
		}
		_ = tw.Flush()
	}
}
