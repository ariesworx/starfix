package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/ariesworx/starfix/internal/gitx"
	"github.com/ariesworx/starfix/internal/proto"
)

// started is start's JSON output: the server's result and the suggested
// branch.
type started struct {
	proto.StartResult
	Branch string `json:"branch"`
}

// cliLease is how long a claim taken from a terminal lasts: nothing renews
// it, so it covers a working day. `sfx away` extends it.
const cliLease = "8h"

func cmdStart(ctx context.Context, r *runner, args []string) error {
	const usage = "start [ID] [--for DURATION] [--take] [--branch | --worktree DIR]"
	fs := r.newFlags("start")
	lease := fs.String("for", cliLease, "how long the claim lasts (1m to 24h); `sfx away` extends it")
	take := fs.Bool("take", false, "take it over from another session of yours that holds it")
	branch := fs.Bool("branch", false, "create or switch to the issue's branch here")
	worktree := fs.String("worktree", "", "create a worktree at DIR on the issue's branch")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usagef(usage, "start takes at most one issue id")
	}
	if *branch && *worktree != "" {
		return usagef(usage, "give --branch or --worktree, not both")
	}
	in := proto.StartArgs{Lease: *lease, Take: *take}
	if len(pos) == 1 {
		in.ID = pos[0]
	}
	var out started
	if err := r.call(ctx, proto.OpStart, in, &out.StartResult); err != nil {
		return err
	}
	is := out.Issue
	out.Branch = gitx.Branch(is.Type, is.ID, is.Title)
	note := ""
	switch {
	case *branch:
		err = gitx.Switch(ctx, r.dir, out.Branch)
		note = " (checked out)"
	case *worktree != "":
		err = gitx.AddWorktree(ctx, r.dir, *worktree, out.Branch)
		note = " (worktree " + *worktree + ")"
	}
	if err != nil {
		return proto.Errf(proto.CodeUnavailable,
			fmt.Sprintf("the issue is yours; fix that, then rerun `sfx start %s` with the same flag", is.ID),
			fmt.Sprintf("started %s, but %v", is.ID, err))
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printIssue(r.env.Stdout, proto.ShowResult{Issue: is, Claim: out.Claim, Items: out.Items})
	printItems(r.env.Stdout, out.Items)
	printHandoff(r.env.Stdout, out.Handoff)
	_, _ = fmt.Fprintf(r.env.Stdout, "\nbranch: %s%s\n", esc(out.Branch), esc(note))
	return nil
}

// repeated collects a repeatable flag whose values may hold commas.
type repeated []string

func (l *repeated) String() string { return strings.Join(*l, "; ") }

func (l *repeated) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func cmdFinish(ctx context.Context, r *runner, args []string) error {
	const usage = "finish ID [--reason TEXT] [--handoff TEXT|-] [--state S] [--next TEXT] [--branch B] [--worktree DIR] [--to P] [--discovered TITLE]... [--tick N,N] [--waive N=REASON]... [--epoch N]"
	fs := r.newFlags("finish")
	var in proto.FinishArgs
	var found, waive repeated
	var tick listFlag
	fs.Var(&tick, "tick", "acceptance items met (comma-separated or repeatable)")
	fs.Var(&waive, "waive", "N=REASON: waive acceptance item N (repeatable)")
	fs.Int64Var(&in.Epoch, "epoch", 0, "refuse unless this is still the claim's epoch")
	fs.StringVar(&in.Reason, "reason", "", "what was done")
	fs.StringVar(&in.Handoff, "handoff", "", "a note for whoever comes next, or - for standard input")
	handoffFlags(fs, &in.HandoffFields)
	fs.Var(&found, "discovered", "title of new work found on the way, filed as a task (repeatable)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if in.ID, err = one(usage, pos, "finish"); err != nil {
		return err
	}
	if in.Handoff, err = r.text(in.Handoff); err != nil {
		return err
	}
	for _, t := range found {
		in.Discovered = append(in.Discovered, proto.Discovered{Title: t})
	}
	if in.Ticked, err = itemNumbers(usage, tick); err != nil {
		return err
	}
	for _, w := range waive {
		ns, reason, ok := strings.Cut(w, "=")
		n, err := strconv.Atoi(ns)
		if !ok || err != nil || n < 1 || reason == "" {
			return usagef(usage, "--waive %q is not N=REASON", w)
		}
		if in.Waived == nil {
			in.Waived = map[int]string{}
		}
		in.Waived[n] = reason
	}
	in.Idem = proto.NewIdem("cli")
	var out proto.FinishResult
	if err := r.withPaths(ctx, in.ID, &in.Paths, func() error { return r.call(ctx, proto.OpFinish, in, &out) }); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s rev %d\n", esc(out.ID), out.Rev)
	if len(out.Created) > 0 {
		_, _ = fmt.Fprintf(r.env.Stdout, "created %s\n", strings.Join(escAll(out.Created), " "))
	}
	return nil
}

func cmdHandoff(ctx context.Context, r *runner, args []string) error {
	const usage = "handoff ID NOTE...|- [--state S] [--next TEXT] [--branch B] [--worktree DIR] [--to P] [--release] [--epoch N]"
	fs := r.newFlags("handoff")
	var in proto.HandoffArgs
	fs.Int64Var(&in.Epoch, "epoch", 0, "refuse unless this is still the claim's epoch")
	fs.BoolVar(&in.Release, "release", false, "also let the issue go: back to open, unassigned")
	handoffFlags(fs, &in.HandoffFields)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(usage, "handoff needs an issue id and a note (or - for standard input)")
	}
	in.ID = pos[0]
	if in.Note, err = r.text(strings.Join(pos[1:], " ")); err != nil {
		return err
	}
	in.Idem = proto.NewIdem("cli")
	return r.withPaths(ctx, in.ID, &in.Paths, func() error { return r.write(ctx, proto.OpHandoff, in) })
}

// withPaths sets *paths to the paths the work on issue id touched, read
// from git, and runs call; if the server refuses the request as invalid
// while it carried paths, it runs call again without them
// (proto.RetryWithoutPaths), since paths are only a hint.
func (r *runner) withPaths(ctx context.Context, id string, paths *[]string, call func() error) error {
	ps := gitx.IssuePaths(ctx, r.dir, []string{id})[id]
	return proto.RetryWithoutPaths(ps != nil, func(with bool) error {
		*paths = nil
		if with {
			*paths = ps
		}
		return call()
	})
}

// sendClaimPaths sends, after away has renewed them, the paths of the
// work on each claim, read from git: one issue per renew, so the server
// refusing one issue's paths loses only those. A refusal of them as
// invalid is passed over, since paths are only a hint and the claims are
// already renewed.
func (r *runner) sendClaimPaths(ctx context.Context, lease string, claims []proto.Claim) error {
	ids := make([]string, len(claims))
	for i, c := range claims {
		ids[i] = c.ID
	}
	paths := gitx.IssuePaths(ctx, r.dir, ids)
	for _, id := range ids {
		ps, ok := paths[id]
		if !ok {
			continue
		}
		err := proto.RetryWithoutPaths(true, func(with bool) error {
			if !with {
				return nil // renewed already; nothing else to send
			}
			in := proto.RenewArgs{Lease: lease, All: true, Paths: map[string][]string{id: ps}}
			return r.call(ctx, proto.OpRenew, in, &proto.ClaimsResult{})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// itemNumbers parses acceptance item numbers.
func itemNumbers(usage string, args []string) ([]int, error) {
	var out []int
	for _, a := range args {
		n, err := strconv.Atoi(a)
		if err != nil || n < 1 {
			return nil, usagef(usage, "%q is not an item number", a)
		}
		out = append(out, n)
	}
	return out, nil
}

func cmdAccept(ctx context.Context, r *runner, args []string) error {
	const usage = "accept ID N... [--undo | --waive REASON]"
	fs := r.newFlags("accept")
	undo := fs.Bool("undo", false, "untick the items instead")
	waive := fs.String("waive", "", "waive the items, for this reason, instead of ticking them")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(usage, "accept needs an issue id and item numbers")
	}
	if *undo && set(fs, "waive") {
		return usagef(usage, "give --undo or --waive, not both")
	}
	var ns []int
	for _, a := range pos[1:] {
		more, err := itemNumbers(usage, strings.Split(a, ","))
		if err != nil {
			return err
		}
		ns = append(ns, more...)
	}
	in := proto.AcceptArgs{ID: pos[0]}
	switch {
	case *undo:
		in.Untick = ns
	case set(fs, "waive"):
		in.Waive = map[int]string{}
		for _, n := range ns {
			in.Waive[n] = *waive
		}
	default:
		in.Tick = ns
	}
	var out proto.AcceptResult
	if err := r.call(ctx, proto.OpAccept, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printItems(r.env.Stdout, out.Items)
	if len(out.Open) == 0 {
		_, _ = fmt.Fprintf(r.env.Stdout, "\n%s: every item is ticked or waived\n", esc(out.ID))
	}
	return nil
}

// handoffFlags adds a handoff's structured fields to finish and handoff.
func handoffFlags(fs *flag.FlagSet, f *proto.HandoffFields) {
	fs.StringVar(&f.State, "state", "", "how far the work got: done, partial or blocked")
	fs.StringVar(&f.Next, "next", "", "the next step")
	fs.StringVar(&f.Branch, "branch", "", "the branch the work is on")
	fs.StringVar(&f.Worktree, "worktree", "", "the worktree the work is in")
	fs.StringVar(&f.To, "to", "", "the principal it is handed to; they find it in their inbox")
}

// printHandoff prints an issue's latest handoff, if any.
func printHandoff(w io.Writer, h *proto.Handoff) {
	if h == nil {
		return
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	var tags []string
	if h.State != "" {
		tags = append(tags, h.State)
	}
	if h.To != "" {
		tags = append(tags, "to "+h.To)
	}
	p("\nhandoff from %s, %s", esc(h.Author), when(h.CreatedAt))
	if len(tags) > 0 {
		p(" (%s)", esc(strings.Join(tags, ", ")))
	}
	p(":\n%s\n", h.Body)
	for _, kv := range [][2]string{{"next", h.Next}, {"on branch", h.Branch}, {"in worktree", h.Worktree}} {
		if kv[1] != "" {
			p("%s: %s\n", kv[0], esc(kv[1]))
		}
	}
}

func cmdAway(ctx context.Context, r *runner, args []string) error {
	const usage = "away DURATION"
	fs := r.newFlags("away")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(usage, "away needs exactly one duration")
	}
	d := pos[0]
	if _, err := proto.ParseDuration(d); err != nil {
		return usagef(usage, "%q: %v", d, err)
	}
	var out proto.ClaimsResult
	if err := r.call(ctx, proto.OpRenew, proto.RenewArgs{Lease: d, All: true}, &out); err != nil {
		return err
	}
	if err := r.sendClaimPaths(ctx, d, out.Claims); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	if len(out.Claims) == 0 {
		_, _ = fmt.Fprintln(r.env.Stdout, "you hold no claims")
		return nil
	}
	for _, c := range out.Claims {
		_, _ = fmt.Fprintf(r.env.Stdout, "%s held until %s (%s)\n", esc(c.ID), when(c.ExpiresAt), esc(c.Session))
	}
	return nil
}
