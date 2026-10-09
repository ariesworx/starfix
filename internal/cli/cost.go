package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// costBys are the groupings sfx cost takes.
var costBys = []string{"account", "issue", "epic", "person", "model"}

func cmdCost(ctx context.Context, r *runner, args []string) error {
	const usage = "cost --since 7d|DATE|TIME [--until DATE|TIME] [--by account|issue|epic|person|model] [-n N]"
	fs := r.newFlags("cost")
	var in proto.CostArgs
	fs.StringVar(&in.By, "by", "account", "group by account, issue, epic, person or model")
	fs.StringVar(&in.Since, "since", "", "window start: a duration back from now (7d), a date or an RFC 3339 time")
	fs.StringVar(&in.Until, "until", "", "window end, not included: a date or an RFC 3339 time (default now)")
	fs.IntVar(&in.Limit, "n", 0, "groups to list, the rest summed (default 50)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	switch {
	case len(pos) > 0:
		return usagef(usage, "cost takes no arguments")
	case in.Since == "":
		return usagef(usage, "cost needs --since, such as 7d or 2026-10-01")
	case !slices.Contains(costBys, in.By):
		return usagef(usage, "--by must be account, issue, epic, person or model")
	}
	var out proto.CostResult
	if err := r.call(ctx, proto.OpCost, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printCost(r.env.Stdout, out)
	return nil
}

// costTitle is how many runes of an issue's or epic's title a report
// shows.
const costTitle = 40

// printCost renders a cost report: a header, a line per group with its
// cost, tokens and marks, the total, and what the marks mean.
func printCost(w io.Writer, c proto.CostResult) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("cost by %s, %s to %s (%s)", esc(c.By), when(c.Since), when(c.Until), proto.Span(c.Until.Sub(c.Since)))
	if len(c.Groups) == 0 {
		p(": no tokens\n")
		return
	}
	p(", list price\n")
	titled := slices.ContainsFunc(c.Groups, func(g proto.CostGroup) bool { return g.Title != "" })
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, g := range c.Groups {
		_, _ = fmt.Fprintf(tw, "  %s\t", esc(g.Key))
		if titled {
			title := []rune(strings.Join(strings.Fields(g.Title), " "))
			if len(title) > costTitle {
				title = append(title[:costTitle], '…')
			}
			_, _ = fmt.Fprintf(tw, "%s\t", esc(string(title)))
		}
		var marks []string
		if g.Split {
			marks = append(marks, "split")
		}
		if g.Unpriced {
			marks = append(marks, "unpriced")
		}
		usd := proto.Dollars(g.CostUSD)
		if g.Unpriced && isZero(g.CostUSD) {
			usd = "-" // no token had a price: not free
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s tokens\t%s\n", usd, proto.TokenCount(tokens(g.Tokens)), strings.Join(marks, " "))
	}
	_ = tw.Flush()
	for line := range strings.Lines(b.String()) {
		p("%s\n", strings.TrimRight(line, " \n"))
	}
	total := proto.Dollars(c.Total.CostUSD)
	if c.Total.Unpriced && isZero(c.Total.CostUSD) {
		total = "unpriced"
	}
	p("total %s, %s tokens\n", total, proto.TokenCount(tokens(c.Total.Tokens)))
	if slices.ContainsFunc(c.Groups, func(g proto.CostGroup) bool { return g.Split }) {
		p("split: shared by time with other work, so an estimate\n")
	}
	if len(c.Unpriced) > 0 {
		p("unpriced: no price for %s; an admin sets one with `sfx admin prices set`\n", strings.Join(escAll(c.Unpriced), ", "))
	}
	if c.Truncated {
		p("partial: more records than the server reads; narrow the window\n")
	}
}

// tokens is the sum of the counts t knows: input, output, cache write and
// cache read (the one-hour writes are part of cache write).
func tokens(t proto.Tokens) int64 {
	var n int64
	for _, c := range []*int64{t.Input, t.Output, t.CacheWrite, t.CacheRead} {
		if c != nil {
			n += *c
		}
	}
	return n
}

// printCostLine prints show's or digest's cost line: "cost unpriced"
// when no token had a price, never $0.00. A server that sends no cost
// prints nothing.
func printCostLine(w io.Writer, usd string, unpriced bool) {
	switch {
	case usd == "":
		return
	case unpriced && isZero(usd):
		_, _ = fmt.Fprintln(w, "cost unpriced")
		return
	}
	_, _ = fmt.Fprintf(w, "cost %s list price", proto.Dollars(usd))
	if unpriced {
		_, _ = fmt.Fprint(w, "; some tokens unpriced")
	}
	_, _ = fmt.Fprintln(w)
}

// isZero reports whether the decimal usd is zero.
func isZero(usd string) bool { return strings.Trim(usd, "0.") == "" }

// rateFlags are the five rate flags of prices set, in the order the
// usage gives them.
var rateFlags = []string{"input", "output", "cache-write", "cache-write-1h", "cache-read"}

func cmdAdmin(ctx context.Context, r *runner, args []string) error {
	const usage = "admin prices [set MODEL --from DATE|TIME --input USD --output USD --cache-write USD --cache-write-1h USD --cache-read USD]"
	fs := r.newFlags("admin")
	var from string
	fs.StringVar(&from, "from", "", "when the rates take effect: a date (midnight UTC) or an RFC 3339 time")
	rates := map[string]*string{}
	for _, f := range rateFlags {
		rates[f] = fs.String(f, "", "US dollars per million tokens")
	}
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	switch {
	case len(pos) == 0:
		return usagef(usage, "admin needs prices")
	case pos[0] != "prices":
		return usagef(usage, "unknown admin command %q", pos[0])
	case len(pos) == 1:
		if set(fs, "from") || slices.ContainsFunc(rateFlags, func(f string) bool { return set(fs, f) }) {
			return usagef(usage, "only prices set takes rates and --from")
		}
		return listPrices(ctx, r)
	case pos[1] != "set":
		return usagef(usage, "unknown prices action %q", pos[1])
	case len(pos) != 3:
		return usagef(usage, "prices set needs one model, spelled as the harness reports it")
	}
	in := proto.PriceSetArgs{Model: pos[2], From: from}
	var missing []string
	for _, f := range rateFlags {
		if !set(fs, f) {
			missing = append(missing, "--"+f)
		}
	}
	if from == "" {
		missing = append([]string{"--from"}, missing...)
	}
	if len(missing) > 0 {
		return usagef(usage, "prices set needs %s; give 0 for what the model does not charge", strings.Join(missing, ", "))
	}
	for i, dst := range []*int64{&in.Input, &in.Output, &in.CacheWrite, &in.CacheWrite1h, &in.CacheRead} { // rateFlags' order
		if *dst, err = proto.ParseRate(*rates[rateFlags[i]]); err != nil {
			return usagef(usage, "--%s: %v", rateFlags[i], err)
		}
	}
	var out proto.PriceSetResult
	if err := r.call(ctx, proto.OpPriceSet, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s from %s: %s\n", esc(in.Model), esc(in.From), esc(out.Change))
	return nil
}

func listPrices(ctx context.Context, r *runner) error {
	var out proto.PricesResult
	if err := r.call(ctx, proto.OpPrices, proto.PricesArgs{}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printPrices(r.env.Stdout, out)
	return nil
}

// printPrices renders the prices table in US dollars per million tokens;
// a price from midnight UTC shows its date alone.
func printPrices(w io.Writer, res proto.PricesResult) {
	if len(res.Prices) == 0 {
		_, _ = fmt.Fprintln(w, "no prices; an admin sets one with `sfx admin prices set`")
		return
	}
	_, _ = fmt.Fprintln(w, "USD per million tokens")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tFROM\tINPUT\tOUTPUT\tCACHE WRITE\tCACHE WRITE 1H\tCACHE READ\tSET BY")
	for _, p := range res.Prices {
		from := p.From.UTC().Format("2006-01-02 15:04")
		if p.From.Equal(p.From.Truncate(24 * time.Hour)) {
			from = p.From.UTC().Format(time.DateOnly)
		}
		r := p.Rates
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", esc(p.Model), from, proto.FormatRate(r.Input), proto.FormatRate(r.Output),
			proto.FormatRate(r.CacheWrite), proto.FormatRate(r.CacheWrite1h), proto.FormatRate(r.CacheRead), esc(p.SetBy))
	}
	_ = tw.Flush()
}
