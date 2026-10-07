package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/ariesworx/starfix/internal/agentsetup"
	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// cmdSetup prints, or with --write makes, an agent's starfix setup: the
// MCP registration, the pointer block in its instruction file and, where
// the harness has one, the SessionStart hook. With --all it does so for
// every agent. It edits the project's own files unless --global says the
// person's home ones may be touched.
func cmdSetup(_ context.Context, r *runner, args []string) error {
	const usage = "setup AGENT|--all [--write|--check|--remove] [--global] [--command PATH]"
	fs := r.newFlags("setup")
	write := fs.Bool("write", false, "edit the files")
	check := fs.Bool("check", false, "fail unless everything is in place")
	remove := fs.Bool("remove", false, "take starfix out of the files")
	global := fs.Bool("global", false, "the user's files in the home directory, not the project's")
	all := fs.Bool("all", false, "every supported agent")
	entry := agentsetup.DefaultEntry
	fs.StringVar(&entry.Command, "command", entry.Command, "how the agent runs starfix")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	names := pos
	switch {
	case *all && len(pos) > 0:
		return usagef(usage, "--all takes no agent")
	case *all:
		// A desktop app's config is the person's, outside the
		// repository, and pins this checkout: it is set up by name only.
		names = nil
		for _, n := range agentsetup.Names() {
			if !agentsetup.Agents[n].Desktop {
				names = append(names, n)
			}
		}
	case len(pos) != 1:
		return usagef(usage, "setup needs one agent, or --all: %s", strings.Join(agentsetup.Names(), ", "))
	}
	n := 0
	for _, b := range []bool{*write, *check, *remove} {
		if b {
			n++
		}
	}
	if n > 1 {
		return usagef(usage, "give at most one of --write, --check and --remove")
	}
	mode := setupMode{write: *write, check: *check, remove: *remove}
	if entry.Command == "" || strings.ContainsAny(entry.Command, "\x00\n") {
		return usagef(usage, "--command must be a program name or path")
	}
	var plans []setupPlan
	for _, name := range names {
		agent, ok := agentsetup.Agents[name]
		if !ok {
			return usagef(usage, "unknown agent %q; supported: %s", name, strings.Join(agentsetup.Names(), ", "))
		}
		if agent.Desktop {
			commandSet := false
			fs.Visit(func(f *flag.Flag) { commandSet = commandSet || f.Name == "command" })
			if commandSet {
				return r.setupDesktop(agent, entry.Command, mode)
			}
			return r.setupDesktop(agent, "", mode)
		}
		p := setupPlan{Agent: agent}
		if *global && agent.Global == "" {
			if !*all {
				return proto.Errf(proto.CodeInvalid, agent.NoGlobal, noGlobal(agent))
			}
			p.skipped = agent.NoGlobal
		}
		plans = append(plans, p)
	}

	root, err := r.setupRoot(*global)
	if err != nil {
		return err
	}
	again := "sfx setup " + names[0]
	if *all {
		again = "sfx setup --all"
	}
	if *global {
		again += " --global"
	}
	base := fileBase{dir: root, user: *global, home: root}
	// Read each file once: two parts may share one (Gemini's settings,
	// AGENTS.md for Codex and Junie), and each sees the last one's edit.
	// Printing reads nothing.
	disk := map[string]*diskFile{}
	var order []*diskFile
	for i := range plans {
		p := &plans[i]
		if p.skipped != "" {
			continue
		}
		for _, t := range p.Targets(*global) {
			path := filepath.Join(root, filepath.FromSlash(t.Path))
			d, ok := disk[path]
			if !ok {
				d = &diskFile{path: path, base: base}
				if mode != (setupMode{}) {
					if err := d.read(); err != nil {
						return err
					}
				}
				disk[path] = d
				order = append(order, d)
			}
			f := setupFile{Target: t, disk: d, rel: t.Path}
			if *global {
				f.rel = path
			}
			p.files = append(p.files, f)
		}
	}
	return r.applySetup(plans, entry, mode, *all, *global, again, order)
}

// setupMode is what setup was asked to do; none set means print.
type setupMode struct{ write, check, remove bool }

// applySetup checks, prints or edits the plans' files, read already,
// and reports. again is the command that would set them up; order
// holds each file once, in the order read.
func (r *runner) applySetup(plans []setupPlan, entry agentsetup.Entry, mode setupMode, all, global bool, again string, order []*diskFile) error {
	var err error
	switch {
	case mode.check:
		var missing []string
		for _, p := range plans {
			var m []string
			for _, f := range p.files {
				if !f.Registered(f.disk.orig, entry) {
					m = append(m, f.rel)
				}
			}
			if len(m) > 0 {
				missing = append(missing, p.Title+" in "+strings.Join(m, ", "))
			}
		}
		if len(missing) > 0 {
			return proto.Errf(proto.CodeNotFound, "run `"+again+" --write`",
				"starfix is not set up for "+strings.Join(missing, "; "))
		}
		r.report(plans, all, "registered")
		return nil
	case !mode.write && !mode.remove:
		r.printPlans(plans, entry, all, global)
		return nil
	}
	// Work out every edit before writing any, so a file that cannot be
	// parsed leaves all of them as they were.
	for i := range plans {
		files := plans[i].files
		for j := range files {
			f := &files[j]
			var out []byte
			if mode.remove {
				out, f.res, err = f.Remove(f.disk.cur, entry)
			} else {
				out, f.res, err = f.Apply(f.disk.cur, entry)
			}
			if err != nil {
				return proto.Errf(proto.CodeInvalid, "fix the file by hand, or move it aside and rerun",
					fmt.Sprintf("cannot edit %s: %v", f.rel, err))
			}
			if f.res == agentsetup.Unchanged {
				continue
			}
			f.disk.cur = out
			// An emptied pointer or hook file was starfix's alone; an
			// emptied MCP config is kept, as the person's file.
			f.disk.drop = len(out) == 0 && f.res == agentsetup.Removed && f.Kind != agentsetup.KindMCP
		}
	}
	for _, d := range order {
		switch {
		case string(d.cur) == string(d.orig):
			continue
		case d.drop:
			err = removeConfig(d.real)
		default:
			err = d.base.writeConfig(d.path, d.real, d.cur, d.mode)
		}
		if err != nil {
			return err
		}
	}
	for i := range plans {
		p := &plans[i]
		for _, f := range p.files {
			switch {
			case f.res != agentsetup.Added:
			case !global:
				p.note = p.Steps(entry)
			case f.Kind == agentsetup.KindHook:
				p.note = globalHookNote
			}
		}
	}
	r.report(plans, all, "")
	return nil
}

// globalHookNote is said after setup adds or prints a hook in the home
// directory. The hook runs prime, which dials the server named by the
// .starfix.yaml of whatever repository the agent opens (C-14 in the
// security review): a cloned repository chooses the host. Per-project
// hooks have the same reach but are part of the repository the person
// chose to set up.
const globalHookNote = "This hook runs in every repository with a .starfix.yaml that you open with the agent, and connects to the server that file names. Turn it off before opening a repository you do not trust, or drop --global and set the hook up per project."

func noGlobal(a agentsetup.Agent) string {
	if a.ManualMCP() {
		return a.Title + " keeps its MCP servers in the IDE's settings, not a file setup edits"
	}
	return a.Title + " keeps its user MCP config in a per-platform profile that setup does not edit"
}

// setupPlan is one agent's files and what setup did to them.
type setupPlan struct {
	agentsetup.Agent
	files []setupFile
	// skipped is the fix for an agent --all --global passes over.
	skipped string
	// note is said after the agent's results.
	note string
}

// shared reports whether another of the agent's parts is in f's file.
func (p setupPlan) shared(f setupFile) bool {
	for _, g := range p.files {
		if g.disk == f.disk && g.Kind != f.Kind {
			return true
		}
	}
	return false
}

// diskFile is one file on disk: as read, and with the edits so far.
type diskFile struct {
	// path is the file as setup names it; real is where it is read
	// and written, once base has checked the way there.
	path, real string
	base       fileBase
	orig, cur  []byte
	mode       fs.FileMode
	// drop: the last edit emptied a file that held only starfix's part.
	drop bool
}

// read checks the way to the file and reads it.
func (d *diskFile) read() error {
	var err error
	if d.real, err = d.base.resolve(d.path); err != nil {
		return err
	}
	if d.orig, d.mode, err = d.base.readConfig(d.real); err != nil {
		return err
	}
	d.cur = d.orig
	return nil
}

// setupFile is one target and the file it lives in.
type setupFile struct {
	agentsetup.Target
	disk *diskFile
	rel  string
	res  agentsetup.Result
}

func (f setupFile) result(fixed string) string {
	if fixed != "" {
		return fixed
	}
	return f.res.String()
}

// report prints each file's result, or fixed for all of them: a line per
// file for one agent, a line per agent for --all.
func (r *runner) report(plans []setupPlan, all bool, fixed string) {
	type line struct {
		Path   string `json:"path"`
		Kind   string `json:"kind"`
		Result string `json:"result"`
	}
	type agentDoc struct {
		Agent   string `json:"agent"`
		Files   []line `json:"files"`
		Note    string `json:"note,omitempty"`
		Skipped string `json:"skipped,omitempty"`
	}
	docs := make([]agentDoc, 0, len(plans))
	var b strings.Builder
	for _, p := range plans {
		d := agentDoc{Agent: p.Name, Files: []line{}, Note: p.note}
		if p.skipped != "" {
			d.Skipped = p.skipped
			fmt.Fprintf(&b, "%s: skipped; fix: %s\n", p.Name, p.skipped)
			docs = append(docs, d)
			continue
		}
		var parts []string
		for _, f := range p.files {
			res := f.result(fixed)
			d.Files = append(d.Files, line{f.rel, f.Kind.String(), res})
			label := f.rel
			if p.shared(f) {
				label += " (" + f.Kind.String() + ")" // Gemini's settings hold two parts
			}
			if all {
				parts = append(parts, label+" "+res)
			} else {
				fmt.Fprintf(&b, "%s: %s\n", label, res)
			}
		}
		if all {
			fmt.Fprintf(&b, "%s: %s\n", p.Name, strings.Join(parts, ", "))
		}
		if p.note != "" {
			note := strings.TrimSuffix(p.note, "\n")
			if all {
				note = "  " + strings.ReplaceAll(note, "\n", "\n  ")
			}
			b.WriteString(note + "\n")
		}
		docs = append(docs, d)
	}
	switch {
	case r.json && all:
		r.emit(struct {
			Agents []agentDoc `json:"agents"`
		}{docs})
	case r.json:
		r.emit(struct {
			Files []line `json:"files"`
		}{docs[0].Files})
	default:
		_, _ = io.WriteString(r.env.Stdout, b.String())
	}
}

func (r *runner) printPlans(plans []setupPlan, entry agentsetup.Entry, all, global bool) {
	if r.json {
		type snip struct {
			Path    string `json:"path"`
			Kind    string `json:"kind"`
			Snippet string `json:"snippet"`
		}
		type agentDoc struct {
			Agent   string `json:"agent"`
			Files   []snip `json:"files"`
			Skipped string `json:"skipped,omitempty"`
		}
		docs := make([]agentDoc, 0, len(plans))
		for _, p := range plans {
			d := agentDoc{Agent: p.Name, Files: []snip{}, Skipped: p.skipped}
			for _, f := range p.files {
				d.Files = append(d.Files, snip{f.rel, f.Kind.String(), f.Snippet(entry)})
			}
			docs = append(docs, d)
		}
		if all {
			r.emit(struct {
				Agents []agentDoc `json:"agents"`
			}{docs})
		} else {
			r.emit(struct {
				Files []snip `json:"files"`
			}{docs[0].Files})
		}
		return
	}
	for i, p := range plans {
		if i > 0 {
			_, _ = io.WriteString(r.env.Stdout, "\n")
		}
		r.printSnippets(p, entry, all, global)
	}
}

func (r *runner) printSnippets(plan setupPlan, entry agentsetup.Entry, all, global bool) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	agent := plan.Agent
	if plan.skipped != "" {
		p("# %s: skipped; fix: %s\n", agent.Title, plan.skipped)
		return
	}
	again := "sfx setup " + agent.Name
	if all {
		again = "sfx setup --all"
	}
	if global {
		again += " --global"
	}
	p("# %s: run `%s --write` to make these edits, or make them by hand\n", agent.Title, again)
	if agent.ManualMCP() && !global {
		p("\n# MCP server: %s\n%s", agent.Note, agent.Snippet(entry))
	}
	for _, f := range plan.files {
		switch f.Kind {
		case agentsetup.KindMCP:
			p("\n# MCP server: add to %s\n%s", f.rel, f.Snippet(entry))
			if agent.Note != "" && !global {
				p("# %s\n", agent.Note)
			}
		case agentsetup.KindPointer:
			p("\n# pointer: add to %s\n%s", f.rel, f.Snippet(entry))
		case agentsetup.KindHook:
			p("\n# SessionStart hook: merge into %s\n%s", f.rel, f.Snippet(entry))
			if global {
				p("# %s\n", globalHookNote)
			}
		}
	}
	if agent.SessionEnv != "" {
		p("\n# session id: read from %s\n", agent.SessionEnv)
	} else {
		p("\n# session id: %s gives sfx mcp none; the server assigns one unless STARFIX_SESSION is set\n", agent.Title)
	}
}

// setupRoot is the directory the targets are relative to: the root of the
// repository that holds .starfix.yaml or, with global, the home directory.
// For a repository, it warns when this machine lacks a program needed to
// reach the server (gcloud, for IAP).
func (r *runner) setupRoot(global bool) (string, error) {
	if global {
		home, err := r.env.UserHomeDir()
		if err != nil || home == "" {
			return "", proto.Errf(proto.CodeInvalid, "set HOME, or drop --global", "cannot find the home directory")
		}
		return home, nil
	}
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return "", err
		}
		return "", proto.Errf(proto.CodeInvalid, "correct "+client.ConfigFile+"; see the README's quick start", err.Error())
	}
	// The agent can be set up regardless, but it will not connect.
	var pe *proto.Error
	if errors.As(cfg.CheckTransport(), &pe) {
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\nfix: %s\n", esc(pe.Message), esc(pe.Fix))
	}
	return cfg.Root, nil
}
