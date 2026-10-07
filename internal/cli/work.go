package cli

import (
	"context"
	"fmt"
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
	const usage = "start [ID] [--for DURATION] [--branch | --worktree DIR]"
	fs := r.newFlags("start")
	lease := fs.String("for", cliLease, "how long the claim lasts (1m to 7d); `sfx away` extends it")
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
	in := proto.StartArgs{Lease: *lease}
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
	printIssue(r.env.Stdout, proto.ShowResult{Issue: is, Claim: out.Claim})
	if h := out.Handoff; h != nil {
		_, _ = fmt.Fprintf(r.env.Stdout, "\nhandoff from %s, %s:\n%s\n", h.Author, when(h.CreatedAt), h.Body)
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "\nbranch: %s%s\n", out.Branch, note)
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
	const usage = "finish ID [--reason TEXT] [--handoff TEXT|-] [--discovered TITLE]... [--epoch N]"
	fs := r.newFlags("finish")
	var in proto.FinishArgs
	var found repeated
	fs.Int64Var(&in.Epoch, "epoch", 0, "refuse unless this is still the claim's epoch")
	fs.StringVar(&in.Reason, "reason", "", "what was done")
	fs.StringVar(&in.Handoff, "handoff", "", "a note for whoever comes next, or - for standard input")
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
	var out proto.FinishResult
	if err := r.call(ctx, proto.OpFinish, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s rev %d\n", out.ID, out.Rev)
	if len(out.Created) > 0 {
		_, _ = fmt.Fprintf(r.env.Stdout, "created %s\n", strings.Join(out.Created, " "))
	}
	return nil
}

func cmdHandoff(ctx context.Context, r *runner, args []string) error {
	const usage = "handoff ID NOTE...|- [--release] [--epoch N]"
	fs := r.newFlags("handoff")
	var in proto.HandoffArgs
	fs.Int64Var(&in.Epoch, "epoch", 0, "refuse unless this is still the claim's epoch")
	fs.BoolVar(&in.Release, "release", false, "also let the issue go: back to open, unassigned")
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
	return r.write(ctx, proto.OpHandoff, in)
}

func cmdAway(ctx context.Context, r *runner, args []string) error {
	const usage = "away DURATION"
	fs := r.newFlags("away")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	d, err := one(usage, pos, "away")
	if err != nil {
		return err
	}
	if _, err := proto.ParseDuration(d); err != nil {
		return usagef(usage, "%q: %v", d, err)
	}
	var out proto.ClaimsResult
	if err := r.call(ctx, proto.OpRenew, proto.RenewArgs{Lease: d, All: true}, &out); err != nil {
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
		_, _ = fmt.Fprintf(r.env.Stdout, "%s held until %s (%s)\n", c.ID, when(c.ExpiresAt), c.Session)
	}
	return nil
}
