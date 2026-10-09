package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
)

// memoryScopes are the scopes a memory may have; empty is project.
var memoryScopes = []string{"project", "user", "team"}

// scopeFlag registers --scope.
func scopeFlag(fs *flag.FlagSet) *string {
	return fs.String("scope", "", "project (default), user (yours alone) or team")
}

// checkScope refuses a scope that is none of memoryScopes.
func checkScope(usage, scope string) error {
	if scope != "" && !slices.Contains(memoryScopes, scope) {
		return usagef(usage, "scope must be project, user or team, not %q", scope)
	}
	return nil
}

// oneKey returns the single memory key in pos.
func oneKey(usage string, pos []string, what string) (string, error) {
	if len(pos) != 1 {
		return "", usagef(usage, "%s needs exactly one key", what)
	}
	return pos[0], nil
}

// orProject names an empty scope as the server does.
func orProject(scope string) string {
	if scope == "" {
		return "project"
	}
	return scope
}

func cmdRemember(ctx context.Context, r *runner, args []string) error {
	const usage = "remember KEY TEXT...|- [--scope S] [--tag T]... [--issue ID] [--rev N] [--pin]"
	fs := r.newFlags("remember")
	scope := scopeFlag(fs)
	var tags listFlag
	fs.Var(&tags, "tag", "tag it; repeat or separate with commas")
	issue := fs.String("issue", "", "link it to an issue")
	rev := fs.Int64("rev", 0, "replace the memory at this revision (default: create a new key)")
	pin := fs.Bool("pin", false, "pin it, so prime always shows it")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return usagef(usage, "remember needs a key and text (or - for standard input)")
	}
	if err := checkScope(usage, *scope); err != nil {
		return err
	}
	body, err := r.text(strings.Join(pos[1:], " "))
	if err != nil {
		return err
	}
	in := proto.RememberArgs{Scope: *scope, Key: pos[0], Body: body, Tags: tags, Issue: *issue, Rev: *rev,
		Idem: proto.NewIdem("cli")}
	if set(fs, "pin") {
		in.Pinned = pin
	}
	var out proto.WriteResult
	if err := r.call(ctx, proto.OpRemember, in, &out); err != nil {
		return err
	}
	return r.wroteMemory(in.Key, in.Scope, out)
}

// wroteMemory prints a memory write's result: KEY (SCOPE) rev N.
func (r *runner) wroteMemory(key, scope string, out proto.WriteResult) error {
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s (%s) rev %d\n", esc(key), esc(orProject(scope)), out.Rev)
	return nil
}

func cmdRecall(ctx context.Context, r *runner, args []string) error {
	const usage = "recall [TEXT...] [--key K] [--tag T] [--scope S] [-n N]"
	fs := r.newFlags("recall")
	var in proto.RecallArgs
	fs.StringVar(&in.Key, "key", "", "only the memory with this key")
	fs.StringVar(&in.Tag, "tag", "", "only memories with this tag")
	scope := scopeFlag(fs)
	fs.IntVar(&in.Limit, "n", 0, "show at most N (default 20, at most 500)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if err := checkScope(usage, *scope); err != nil {
		return err
	}
	in.Scope, in.Text = *scope, strings.Join(pos, " ")
	return r.recall(ctx, in)
}

func cmdMemories(ctx context.Context, r *runner, args []string) error {
	const usage = "memories [--scope S] [-n N]"
	fs := r.newFlags("memories")
	var in proto.RecallArgs
	scope := scopeFlag(fs)
	fs.IntVar(&in.Limit, "n", 0, "show at most N (default 20, at most 500)")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "memories takes no arguments; search with `sfx recall TEXT`")
	}
	if err := checkScope(usage, *scope); err != nil {
		return err
	}
	in.Scope = *scope
	return r.recall(ctx, in)
}

// recall runs a recall and prints its memories.
func (r *runner) recall(ctx context.Context, in proto.RecallArgs) error {
	var out proto.RecallResult
	if err := r.call(ctx, proto.OpRecall, in, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	printMemories(r.env.Stdout, out)
	return nil
}

func cmdForget(ctx context.Context, r *runner, args []string) error {
	const usage = "forget KEY [--scope S] [--rev N]"
	fs := r.newFlags("forget")
	scope := scopeFlag(fs)
	rev := fs.Int64("rev", 0, "refuse if the memory changed since this revision")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	key, err := oneKey(usage, pos, "forget")
	if err != nil {
		return err
	}
	if err := checkScope(usage, *scope); err != nil {
		return err
	}
	var out proto.WriteResult
	if err := r.call(ctx, proto.OpForget, proto.ForgetArgs{Scope: *scope, Key: key, Rev: *rev, Idem: proto.NewIdem("cli")}, &out); err != nil {
		return err
	}
	if r.json {
		r.emit(out)
		return nil
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "forgot %s (%s)\n", esc(key), esc(orProject(*scope)))
	return nil
}

func cmdPin(ctx context.Context, r *runner, args []string) error {
	return pinMemory(ctx, r, args, "pin", true)
}

func cmdUnpin(ctx context.Context, r *runner, args []string) error {
	return pinMemory(ctx, r, args, "unpin", false)
}

// pinMemory is pin and unpin.
func pinMemory(ctx context.Context, r *runner, args []string, name string, pinned bool) error {
	usage := name + " KEY [--scope S]"
	fs := r.newFlags(name)
	scope := scopeFlag(fs)
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	key, err := oneKey(usage, pos, name)
	if err != nil {
		return err
	}
	if err := checkScope(usage, *scope); err != nil {
		return err
	}
	var out proto.WriteResult
	if err := r.call(ctx, proto.OpPin, proto.PinArgs{Scope: *scope, Key: key, Pinned: pinned}, &out); err != nil {
		return err
	}
	return r.wroteMemory(key, *scope, out)
}

// printMemories prints each memory's header, its tags and issue, and its
// body indented. Single-line fields go through esc; the body is printed
// as it is, so w must escape it, as the runner's Stdout does.
func printMemories(w io.Writer, res proto.RecallResult) {
	if len(res.Memories) == 0 {
		_, _ = fmt.Fprintln(w, "no memories")
		return
	}
	for _, m := range res.Memories {
		mark := esc(m.Scope)
		if m.Pinned {
			mark += ", pinned"
		}
		if m.Relevant {
			mark += ", relevant"
		}
		_, _ = fmt.Fprintf(w, "%s (%s) rev %d, %s, %s\n", esc(m.Key), mark, m.Rev, esc(m.UpdatedBy), when(m.UpdatedAt))
		var about []string
		if len(m.Tags) > 0 {
			about = append(about, "tags: "+strings.Join(escAll(m.Tags), ", "))
		}
		if m.Issue != "" {
			about = append(about, "issue: "+esc(m.Issue))
		}
		if len(about) > 0 {
			_, _ = fmt.Fprintf(w, "  %s\n", strings.Join(about, "; "))
		}
		for line := range strings.SplitSeq(m.Body, "\n") {
			_, _ = fmt.Fprintf(w, "  %s\n", line)
		}
	}
	if res.More > 0 {
		_, _ = fmt.Fprintf(w, "and %d more; narrow the search, or raise -n\n", res.More)
	}
}
