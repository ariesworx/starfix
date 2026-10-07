package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
		names = agentsetup.Names()
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
	if entry.Command == "" || strings.ContainsAny(entry.Command, "\x00\n") {
		return usagef(usage, "--command must be a program name or path")
	}
	var plans []setupPlan
	for _, name := range names {
		agent, ok := agentsetup.Agents[name]
		if !ok {
			return usagef(usage, "unknown agent %q; supported: %s", name, strings.Join(agentsetup.Names(), ", "))
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
	// Read each file once: two parts may share one (Gemini's settings,
	// AGENTS.md for Codex and Junie), and each sees the last one's edit.
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
				d = &diskFile{path: path}
				if d.orig, d.mode, err = readConfig(path); err != nil {
					return err
				}
				d.cur = d.orig
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

	switch {
	case *check:
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
		r.report(plans, *all, "registered")
		return nil
	case !*write && !*remove:
		r.printPlans(plans, entry, *all, *global)
		return nil
	}
	// Work out every edit before writing any, so a file that cannot be
	// parsed leaves all of them as they were.
	for i := range plans {
		for j := range plans[i].files {
			f := &plans[i].files[j]
			var out []byte
			if *remove {
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
			err = removeConfig(d.path)
		default:
			err = writeConfig(d.path, d.cur, d.mode)
		}
		if err != nil {
			return err
		}
	}
	for i := range plans {
		p := &plans[i]
		for _, f := range p.files {
			if f.res == agentsetup.Added && !*global {
				p.note = p.Steps(entry)
			}
		}
	}
	r.report(plans, *all, "")
	return nil
}

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
	path      string
	orig, cur []byte
	mode      fs.FileMode
	// drop: the last edit emptied a file that held only starfix's part.
	drop bool
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
	return cfg.Root, nil
}

// readConfig reads a config file; a missing one is empty, mode 0644.
func readConfig(path string) ([]byte, fs.FileMode, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the agent's config file, chosen by name
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0o644, nil
	}
	if err != nil {
		return nil, 0, proto.Errf(proto.CodeInvalid, "check the file's permissions", fmt.Sprintf("read %s: %v", path, err))
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, 0, proto.Errf(proto.CodeInvalid, "check the file's permissions", fmt.Sprintf("stat %s: %v", path, err))
	}
	return b, st.Mode().Perm(), nil
}

// removeConfig deletes a file setup emptied.
func removeConfig(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return proto.Errf(proto.CodeInvalid, "check the directory's permissions", fmt.Sprintf("remove %s: %v", path, err))
	}
	return nil
}

// writeConfig replaces path atomically, keeping its mode. A project
// config is meant to be committed, so a new one is 0644; it holds no
// secret.
func writeConfig(path string, b []byte, mode fs.FileMode) error {
	fail := func(err error) error {
		return proto.Errf(proto.CodeInvalid, "check the directory's permissions", fmt.Sprintf("write %s: %v", path, err))
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // .codex/, .claude/ and the like, in the repository
		return fail(err)
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fail(err)
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(tmp, mode)); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	return nil
}
