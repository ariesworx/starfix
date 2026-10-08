package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// maxStdin bounds text read from standard input with "-".
const maxStdin = 1 << 20

// text returns v or, when v is "-", standard input without its trailing
// newlines. Input longer than maxStdin is a usage error.
func (r *runner) text(v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.env.Stdin, maxStdin+1))
	if err != nil {
		return "", fmt.Errorf("read standard input: %w", err)
	}
	if len(b) > maxStdin {
		return "", usagef("", "standard input is larger than 1 MiB")
	}
	return strings.TrimRight(string(b), "\n"), nil
}

// fields are the issue fields create and update share.
type fields struct {
	title, body, design, acceptance, notes, status, typ, assignee, owner, parent, prio, account string
}

// register adds the field flags to fs. withTitle adds --title, for
// update; create takes its title as arguments.
func (f *fields) register(fs *flag.FlagSet, withTitle bool) {
	if withTitle {
		fs.StringVar(&f.title, "title", "", "title")
	}
	fs.StringVar(&f.body, "body", "", "description, or - for standard input")
	fs.StringVar(&f.design, "design", "", "design notes, or -")
	fs.StringVar(&f.acceptance, "acceptance", "", "acceptance criteria, or -")
	fs.StringVar(&f.notes, "notes", "", "notes, or -")
	fs.StringVar(&f.status, "status", "", "open, blocked or deferred (`sfx start` sets in_progress)")
	fs.StringVar(&f.typ, "t", "", "type: bug, feature, task, epic or chore")
	fs.StringVar(&f.typ, "type", "", "type")
	fs.StringVar(&f.prio, "p", "", "priority 0-4")
	fs.StringVar(&f.prio, "priority", "", "priority 0-4")
	fs.StringVar(&f.assignee, "assignee", "", "assignee (empty clears)")
	fs.StringVar(&f.owner, "owner", "", "owner (empty clears)")
	fs.StringVar(&f.parent, "parent", "", "parent issue id (empty clears)")
	fs.StringVar(&f.account, "account", "", "account code name for time and tokens (empty inherits the parent's)")
}

// readTexts resolves "-" in the long text fields; only one may use it.
func (r *runner) readTexts(f *fields) error {
	n := 0
	for _, p := range []*string{&f.body, &f.design, &f.acceptance, &f.notes} {
		if *p == "-" {
			n++
			if n > 1 {
				return usagef("", "only one field can read standard input")
			}
			v, err := r.text(*p)
			if err != nil {
				return err
			}
			*p = v
		}
	}
	return nil
}

func cmdCreate(ctx context.Context, r *runner, args []string) error {
	const usage = "create TITLE... [-p N] [-t TYPE] [--body TEXT|-] [--parent ID] [--account NAME] [--label L]..."
	fs := r.newFlags("create")
	var f fields
	f.register(fs, false)
	var labels listFlag
	var id string
	fs.Var(&labels, "label", "label (repeatable, or comma-separated)")
	fs.Var(&labels, "l", "label")
	fs.StringVar(&id, "id", "", "issue id (default: generated)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		return usagef(usage, "create needs a title")
	}
	if err := r.readTexts(&f); err != nil {
		return err
	}
	idem := proto.NewIdem("cli")
	in := proto.CreateArgs{ID: id, Idem: idem, Title: strings.Join(pos, " "), Body: f.body, Design: f.design,
		Acceptance: f.acceptance, Notes: f.notes, Status: f.status, Type: f.typ, Assignee: f.assignee,
		Owner: f.owner, Parent: f.parent, Labels: labels, Account: f.account}
	if f.prio != "" {
		p, err := priority(f.prio)
		if err != nil {
			return usagef(usage, "%v", err)
		}
		in.Priority = &p
	}
	var out proto.CreateResult
	if err := r.call(ctx, proto.OpCreate, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintln(r.env.Stdout, esc(out.ID))
	// Standard error, so `id=$(sfx create …)` still captures the id alone.
	printSimilar(r.env.Stderr, out.Similar)
	return nil
}

// printSimilar lists similar closed issues, if any.
func printSimilar(w io.Writer, sim []proto.Summary) {
	if len(sim) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "similar closed issues:")
	for _, s := range sim {
		_, _ = fmt.Fprintf(w, "  %s  %s\n", esc(s.ID), esc(s.Title))
	}
}

// printItems prints an issue's acceptance checklist, if it has one.
func printItems(w io.Writer, items []proto.AcceptanceItem) {
	if len(items) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nacceptance:")
	for _, it := range items {
		mark := " "
		switch it.State {
		case "ticked":
			mark = "x"
		case "waived":
			mark = "~"
		}
		line := fmt.Sprintf("  %d [%s] %s", it.N, mark, esc(it.Text))
		switch {
		case it.State == "waived":
			line += fmt.Sprintf(" (waived by %s: %s)", esc(it.By), esc(it.Reason))
		case it.By != "":
			line += " (" + esc(it.By) + ")"
		}
		_, _ = fmt.Fprintln(w, line)
	}
}

// one returns the issue id that must be pos's only element; what names
// the command in the usage error.
func one(usage string, pos []string, what string) (string, error) {
	if len(pos) != 1 {
		return "", usagef(usage, "%s needs exactly one issue id", what)
	}
	return pos[0], nil
}

func cmdShow(ctx context.Context, r *runner, args []string) error {
	const usage = "show ID [--compact]"
	fs := r.newFlags("show")
	compact := fs.Bool("compact", false, "cut long text fields")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "show")
	if err != nil {
		return err
	}
	var out proto.ShowResult
	if err := r.call(ctx, proto.OpShow, proto.ShowArgs{ID: id, Full: !*compact}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printIssue(r.env.Stdout, out)
	printIssueUsage(r.env.Stdout, out.Issue.Account, out.Usage)
	printItems(r.env.Stdout, out.Items)
	printHandoff(r.env.Stdout, out.Handoff)
	if len(out.Similar) > 0 {
		_, _ = fmt.Fprintln(r.env.Stdout)
		printSimilar(r.env.Stdout, out.Similar)
	}
	return nil
}

// when formats t for text output: UTC, to the minute.
func when(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

// printIssue prints an issue's header lines, then its text. Single-line
// fields go through esc; the multi-line text is printed as it is, so w
// must escape it, as the runner's Stdout does.
func printIssue(w io.Writer, s proto.ShowResult) {
	is := s.Issue
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("%s  P%d  %s  %s  rev %d\n%s\n", esc(is.ID), is.Priority, esc(is.Status), esc(is.Type), is.Rev, esc(is.Title))
	var who []string
	for _, kv := range [][2]string{{"parent", is.ParentID}, {"assignee", is.Assignee}, {"owner", is.Owner}} {
		if kv[1] != "" {
			who = append(who, kv[0]+": "+esc(kv[1]))
		}
	}
	if len(who) > 0 {
		p("%s\n", strings.Join(who, "  "))
	}
	if len(is.Labels) > 0 {
		p("labels: %s\n", strings.Join(escAll(is.Labels), ", "))
	}
	if c := s.Claim; c != nil {
		p("claimed by %s (%s on %s), epoch %d, until %s\n", esc(c.By), esc(c.Session), esc(c.Machine), c.Epoch, when(c.ExpiresAt))
	}
	p("created %s by %s; updated %s\n", when(is.CreatedAt), esc(is.CreatedBy), when(is.UpdatedAt))
	if is.ClosedAt != nil {
		p("closed %s", when(*is.ClosedAt))
		if is.CloseReason != "" {
			p(": %s", esc(is.CloseReason))
		}
		p("\n")
	}
	var out, in []string
	for _, d := range s.Deps {
		if d.From == is.ID {
			out = append(out, fmt.Sprintf("%s (%s)", esc(d.To), esc(d.Type)))
		} else {
			in = append(in, fmt.Sprintf("%s (%s)", esc(d.From), esc(d.Type)))
		}
	}
	if len(out) > 0 {
		p("depends on: %s\n", strings.Join(out, ", "))
	}
	if len(in) > 0 {
		p("needed by: %s\n", strings.Join(in, ", "))
	}
	if s.DepsMore > 0 {
		p("(%d more dependencies not shown)\n", s.DepsMore)
	}
	if is.Body != "" {
		p("\n%s\n", is.Body)
	}
	acceptance := is.Acceptance
	if len(s.Items) > 0 {
		acceptance = "" // printItems shows it as a checklist
	}
	for _, kv := range [][2]string{{"design", is.Design}, {"acceptance", acceptance}, {"notes", is.Notes}} {
		if kv[1] != "" {
			p("\n%s:\n%s\n", kv[0], kv[1])
		}
	}
	if is.Truncated {
		p("\n(text cut; run without --compact for all of it)\n")
	}
}

// printSummaries prints one aligned line per issue: id, priority, status
// and title, then extra(i) for issues[i] when extra is set.
func printSummaries(w io.Writer, issues []proto.Summary, extra func(i int) string) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for i, is := range issues {
		line := fmt.Sprintf("%s\tP%d\t%s\t%s", esc(is.ID), is.Priority, esc(is.Status), esc(is.Title))
		if extra != nil {
			line += extra(i)
		}
		_, _ = fmt.Fprintln(tw, line)
	}
	_ = tw.Flush()
}

func cmdList(ctx context.Context, r *runner, args []string) error {
	const usage = "list [--status S,S] [--all] [-t TYPE] [-p N,N] [--assignee A] [--parent ID] [--label L]... [-n N] [--cursor C]"
	fs := r.newFlags("list")
	var status, types, prios, labels listFlag
	var in proto.ListArgs
	fs.Var(&status, "status", "statuses (comma-separated)")
	fs.Var(&types, "t", "types")
	fs.Var(&types, "type", "types")
	fs.Var(&prios, "p", "priorities")
	fs.Var(&prios, "priority", "priorities")
	fs.Var(&labels, "label", "labels, all required")
	fs.Var(&labels, "l", "labels")
	all := fs.Bool("all", false, "include closed issues")
	fs.StringVar(&in.Assignee, "assignee", "", "assignee")
	fs.StringVar(&in.Parent, "parent", "", "parent issue id")
	fs.IntVar(&in.Limit, "n", 0, "page size (default 50)")
	fs.StringVar(&in.Cursor, "cursor", "", "continue from a previous page")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "list takes no arguments")
	}
	in.Status, in.Type, in.Labels = status, types, labels
	if len(in.Status) == 0 && !*all {
		in.Status = []string{"open", "in_progress", "blocked", "deferred"}
	}
	for _, s := range prios {
		p, err := priority(s)
		if err != nil {
			return usagef(usage, "%v", err)
		}
		in.Priority = append(in.Priority, p)
	}
	var out proto.ListResult
	if err := r.call(ctx, proto.OpList, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printSummaries(r.env.Stdout, out.Issues, nil)
	if out.Next != "" {
		_, _ = fmt.Fprintf(r.env.Stderr, "more: sfx list --cursor %s\n", esc(out.Next))
	}
	return nil
}

// limitArgs parses the arguments of a command whose only flag is -n.
func limitArgs(r *runner, name string, args []string) (proto.LimitArgs, error) {
	usage := name + " [-n N]"
	fs := r.newFlags(name)
	var in proto.LimitArgs
	fs.IntVar(&in.Limit, "n", 0, "how many")
	pos, err := parse(fs, args, usage)
	if err == nil && len(pos) > 0 {
		err = usagef(usage, "%s takes no arguments", name)
	}
	return in, err
}

func cmdReady(ctx context.Context, r *runner, args []string) error {
	in, err := limitArgs(r, "ready", args)
	if err != nil {
		return err
	}
	var out proto.ListResult
	if err := r.call(ctx, proto.OpReady, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printSummaries(r.env.Stdout, out.Issues, nil)
	return nil
}

func cmdBlocked(ctx context.Context, r *runner, args []string) error {
	in, err := limitArgs(r, "blocked", args)
	if err != nil {
		return err
	}
	var out proto.BlockedResult
	if err := r.call(ctx, proto.OpBlocked, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printBlocked(r.env.Stdout, out.Issues)
	return nil
}

// printBlocked lists blocked issues with what holds each back.
func printBlocked(w io.Writer, issues []proto.BlockedIssue) {
	sums := make([]proto.Summary, len(issues))
	for i, b := range issues {
		sums[i] = b.Summary
	}
	printSummaries(w, sums, func(i int) string {
		b := issues[i]
		s := "\tblocked by " + strings.Join(escAll(b.BlockedBy), ", ")
		if b.More > 0 {
			s += fmt.Sprintf(" and %d more", b.More)
		}
		if b.Via != "" {
			s += " (via " + esc(b.Via) + ")"
		}
		return s
	})
}

func cmdUpdate(ctx context.Context, r *runner, args []string) error {
	const usage = "update ID [--rev N] [--title T] [--body TEXT|-] [--status S] [-p N] [-t TYPE] [--assignee A] [--parent ID] [--account NAME] ..."
	fs := r.newFlags("update")
	var f fields
	f.register(fs, true)
	rev := fs.Int64("rev", 0, "revision you read (default: the current one)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "update")
	if err != nil {
		return err
	}
	if err := r.readTexts(&f); err != nil {
		return err
	}
	in := proto.UpdateArgs{ID: id, Rev: *rev}
	n := 0
	str := func(names []string, v string, dst **string) {
		for _, name := range names {
			if set(fs, name) {
				s := v
				*dst = &s
				n++
				return
			}
		}
	}
	str([]string{"title"}, f.title, &in.Title)
	str([]string{"body"}, f.body, &in.Body)
	str([]string{"design"}, f.design, &in.Design)
	str([]string{"acceptance"}, f.acceptance, &in.Acceptance)
	str([]string{"notes"}, f.notes, &in.Notes)
	str([]string{"status"}, f.status, &in.Status)
	str([]string{"t", "type"}, f.typ, &in.Type)
	str([]string{"assignee"}, f.assignee, &in.Assignee)
	str([]string{"owner"}, f.owner, &in.Owner)
	str([]string{"parent"}, f.parent, &in.Parent)
	str([]string{"account"}, f.account, &in.Account)
	if set(fs, "p") || set(fs, "priority") {
		p, err := priority(f.prio)
		if err != nil {
			return usagef(usage, "%v", err)
		}
		in.Priority = &p
		n++
	}
	if n == 0 {
		return usagef(usage, "nothing to update: give at least one field flag")
	}
	if in.Rev == 0 {
		var cur proto.ShowResult
		if err := r.call(ctx, proto.OpShow, proto.ShowArgs{ID: id}, &cur); err != nil {
			return err
		}
		in.Rev = cur.Issue.Rev
	}
	return r.write(ctx, proto.OpUpdate, in)
}

// write runs an issue write and prints "ID rev N".
func (r *runner) write(ctx context.Context, op string, in any) error {
	var out proto.WriteResult
	if err := r.call(ctx, op, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
	} else {
		_, _ = fmt.Fprintf(r.env.Stdout, "%s rev %d\n", esc(out.ID), out.Rev)
	}
	return nil
}

func cmdClose(ctx context.Context, r *runner, args []string) error {
	const usage = "close ID [--reason TEXT] [--rev N] [--force]"
	fs := r.newFlags("close")
	var in proto.CloseArgs
	fs.StringVar(&in.Reason, "reason", "", "why it was closed")
	fs.BoolVar(&in.Force, "force", false, "admins only: close even with acceptance items open; the event records them")
	fs.Int64Var(&in.Rev, "rev", 0, "refuse if the issue changed since this revision")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if in.ID, err = one(usage, pos, "close"); err != nil {
		return err
	}
	return r.write(ctx, proto.OpClose, in)
}

func cmdReopen(ctx context.Context, r *runner, args []string) error {
	const usage = "reopen ID [--rev N]"
	fs := r.newFlags("reopen")
	var in proto.ReopenArgs
	fs.Int64Var(&in.Rev, "rev", 0, "refuse if the issue changed since this revision")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if in.ID, err = one(usage, pos, "reopen"); err != nil {
		return err
	}
	return r.write(ctx, proto.OpReopen, in)
}

// done prints the empty result of a write with nothing to report.
func (r *runner) done() {
	if r.json {
		r.emit(proto.Empty{})
	}
}

func cmdDep(ctx context.Context, r *runner, args []string) error {
	const usage = "dep add|rm FROM TO [--type T]"
	fs := r.newFlags("dep")
	var in proto.DepArgs
	fs.StringVar(&in.Type, "type", "blocks", "blocks, conditional-blocks, waits-for, related, discovered-from, duplicates or supersedes")
	fs.StringVar(&in.Type, "t", "blocks", "type")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) != 3 {
		return usagef(usage, "dep needs add or rm, then two issue ids")
	}
	op := map[string]string{"add": proto.OpDepAdd, "rm": proto.OpDepRm, "remove": proto.OpDepRm}[pos[0]]
	if op == "" {
		return usagef(usage, "unknown dep action %q", pos[0])
	}
	in.From, in.To = pos[1], pos[2]
	if err := r.call(ctx, op, in, nil); err != nil {
		return err
	}
	r.done()
	return nil
}

func cmdLabel(ctx context.Context, r *runner, args []string) error {
	const usage = "label add|rm ID LABEL..."
	fs := r.newFlags("label")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) < 3 {
		return usagef(usage, "label needs add or rm, an issue id and at least one label")
	}
	op := map[string]string{"add": proto.OpLabelAdd, "rm": proto.OpLabelRm, "remove": proto.OpLabelRm}[pos[0]]
	if op == "" {
		return usagef(usage, "unknown label action %q", pos[0])
	}
	for _, l := range pos[2:] {
		if err := r.call(ctx, op, proto.LabelArgs{ID: pos[1], Label: l}, nil); err != nil {
			return err
		}
	}
	r.done()
	return nil
}

func cmdComment(ctx context.Context, r *runner, args []string) error {
	const usage = "comment ID TEXT...|-"
	fs := r.newFlags("comment")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(usage, "comment needs an issue id and text (or - for standard input)")
	}
	body, err := r.text(strings.Join(pos[1:], " "))
	if err != nil {
		return err
	}
	idem := proto.NewIdem("cli")
	var out proto.CommentResult
	if err := r.call(ctx, proto.OpComment, proto.CommentArgs{ID: pos[0], Body: body, Idem: idem}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
	} else {
		_, _ = fmt.Fprintln(r.env.Stdout, esc(out.ID))
	}
	return nil
}

// pageFlag registers -n, the newest entries to show (0: all).
func pageFlag(r *runner, name string) (*flag.FlagSet, *int) {
	fs := r.newFlags(name)
	n := fs.Int("n", 0, "show only the newest N (default all)")
	return fs, n
}

// pages reads an issue's comments or history newest page first, until it
// has want entries (0: all), and returns them oldest first with how many
// earlier ones it left out. page asks for one page before the cursor and
// returns its entries, the cursor of the page before it and the total.
func pages[T any](want int, page func(before string, limit int) ([]T, string, int, error)) ([]T, int, error) {
	var all []T
	before, total := "", 0
	for {
		limit := 0
		if want > 0 {
			limit = want - len(all)
		}
		got, earlier, n, err := page(before, limit)
		if err != nil {
			return nil, 0, err
		}
		all, total = append(got, all...), max(n, total)
		if earlier == "" || (want > 0 && len(all) >= want) {
			break
		}
		before = earlier
	}
	if want > 0 && len(all) > want {
		all = all[len(all)-want:]
	}
	return all, max(total-len(all), 0), nil
}

func cmdComments(ctx context.Context, r *runner, args []string) error {
	const usage = "comments ID [-n N]"
	fs, n := pageFlag(r, "comments")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "comments")
	if err != nil {
		return err
	}
	cs, earlier, err := pages(*n, func(before string, limit int) ([]proto.Comment, string, int, error) {
		var out proto.CommentsResult
		err := r.call(ctx, proto.OpComments, proto.PageArgs{ID: id, Before: before, Limit: limit}, &out)
		return out.Comments, out.Earlier, out.Total, err
	})
	if err != nil {
		return err
	}
	if r.json {
		r.emit(proto.CommentsResult{Comments: cs, Total: len(cs) + earlier})
		return nil
	}
	if earlier > 0 {
		_, _ = fmt.Fprintf(r.env.Stdout, "(%d earlier not shown; -n 0 shows all)\n\n", earlier)
	}
	printComments(r.env.Stdout, cs)
	return nil
}

// printComments prints each comment's author, time and kind on one line,
// then its body.
func printComments(w io.Writer, cs []proto.Comment) {
	for i, c := range cs {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		kind := ""
		if c.Kind != "" {
			kind = "  (" + esc(c.Kind) + ")"
		}
		_, _ = fmt.Fprintf(w, "%s  %s%s\n%s\n", esc(c.Author), when(c.CreatedAt), kind, c.Body)
	}
}

func cmdHistory(ctx context.Context, r *runner, args []string) error {
	const usage = "history ID [-n N]"
	fs, n := pageFlag(r, "history")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	id, err := one(usage, pos, "history")
	if err != nil {
		return err
	}
	evs, earlier, err := pages(*n, func(before string, limit int) ([]proto.Event, string, int, error) {
		var out proto.HistoryResult
		err := r.call(ctx, proto.OpHistory, proto.PageArgs{ID: id, Before: before, Limit: limit}, &out)
		return out.Events, out.Earlier, out.Total, err
	})
	if err != nil {
		return err
	}
	if r.json {
		r.emit(proto.HistoryResult{Events: evs, Total: len(evs) + earlier})
		return nil
	}
	if earlier > 0 {
		_, _ = fmt.Fprintf(r.env.Stdout, "(%d earlier not shown; -n 0 shows all)\n", earlier)
	}
	return printHistory(r.env.Stdout, evs)
}

// printHistory prints one line per event.
func printHistory(w io.Writer, evs []proto.Event) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range evs {
		_, _ = fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\n", e.Seq, when(e.At), esc(e.Principal), esc(e.Op), esc(e.Changed()))
	}
	return tw.Flush()
}

func cmdVersion(_ context.Context, r *runner, _ []string) error {
	if r.json {
		r.emit(map[string]string{"version": r.env.Version, "protocol": strconv.Itoa(proto.Proto)})
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "starfix %s (protocol %d)\n", r.env.Version, proto.Proto)
	return nil
}
