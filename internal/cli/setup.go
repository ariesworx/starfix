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
// the harness has one, the SessionStart hook. It edits the project's own
// files unless --global says the person's home ones may be touched.
func cmdSetup(_ context.Context, r *runner, args []string) error {
	const usage = "setup claude-code|codex|cursor|gemini|vscode [--write|--check|--remove] [--global] [--command PATH]"
	fs := r.newFlags("setup")
	write := fs.Bool("write", false, "edit the files")
	check := fs.Bool("check", false, "fail unless everything is in place")
	remove := fs.Bool("remove", false, "take starfix out of the files")
	global := fs.Bool("global", false, "the user's files in the home directory, not the project's")
	entry := agentsetup.DefaultEntry
	fs.StringVar(&entry.Command, "command", entry.Command, "how the agent runs starfix")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(usage, "setup needs one agent: %s", strings.Join(agentsetup.Names(), ", "))
	}
	agent, ok := agentsetup.Agents[pos[0]]
	if !ok {
		return usagef(usage, "unknown agent %q; supported: %s", pos[0], strings.Join(agentsetup.Names(), ", "))
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
	if *global && agent.Global == "" {
		return proto.Errf(proto.CodeInvalid, agent.NoGlobal,
			agent.Title+" keeps its user MCP config in a per-platform profile that setup does not edit")
	}

	root, err := r.setupRoot(*global)
	if err != nil {
		return err
	}
	again := "sfx setup " + agent.Name
	if *global {
		again += " --global"
	}
	var files []setupFile
	for _, t := range agent.Targets(*global) {
		f := setupFile{Target: t, path: filepath.Join(root, filepath.FromSlash(t.Path)), rel: t.Path}
		if *global {
			f.rel = f.path
		}
		if f.content, f.mode, err = readConfig(f.path); err != nil {
			return err
		}
		files = append(files, f)
	}

	switch {
	case *check:
		var missing []string
		for _, f := range files {
			if !f.Registered(f.content, entry) {
				missing = append(missing, f.rel)
			}
		}
		if len(missing) > 0 {
			return proto.Errf(proto.CodeNotFound, "run `"+again+" --write`",
				fmt.Sprintf("starfix is not set up for %s in %s", agent.Title, strings.Join(missing, ", ")))
		}
		r.reportFiles(files, "registered", "")
		return nil
	case !*write && !*remove:
		r.printSnippets(agent, entry, files, again, *global)
		return nil
	}
	// Work out every edit before writing any, so a file that cannot be
	// parsed leaves all of them as they were.
	for i := range files {
		f := &files[i]
		if *remove {
			f.out, f.res, err = f.Remove(f.content, entry)
		} else {
			f.out, f.res, err = f.Apply(f.content, entry)
		}
		if err != nil {
			return proto.Errf(proto.CodeInvalid, "fix the file by hand, or move it aside and rerun",
				fmt.Sprintf("cannot edit %s: %v", f.rel, err))
		}
	}
	note := ""
	for _, f := range files {
		switch {
		case f.res == agentsetup.Unchanged:
			continue
		case f.res == agentsetup.Removed && f.Kind == agentsetup.KindPointer && len(f.out) == 0:
			err = removeConfig(f.path) // the file held only the pointer
		default:
			err = writeConfig(f.path, f.out, f.mode)
		}
		if err != nil {
			return err
		}
		if f.Kind == agentsetup.KindMCP && f.res == agentsetup.Added && !*global {
			note = agent.Note
		}
	}
	r.reportFiles(files, "", note)
	return nil
}

// setupFile is one target and its state on disk.
type setupFile struct {
	agentsetup.Target
	path, rel string
	content   []byte
	mode      fs.FileMode
	out       []byte
	res       agentsetup.Result
}

// reportFiles prints each file's result, or result for all of them.
func (r *runner) reportFiles(files []setupFile, result, note string) {
	type line struct {
		Path   string `json:"path"`
		Kind   string `json:"kind"`
		Result string `json:"result"`
	}
	doc := struct {
		Files []line `json:"files"`
	}{Files: []line{}}
	var b strings.Builder
	for _, f := range files {
		res := result
		if res == "" {
			res = f.res.String()
		}
		doc.Files = append(doc.Files, line{f.rel, f.Kind.String(), res})
		fmt.Fprintf(&b, "%s: %s\n", f.rel, res)
	}
	if note != "" {
		b.WriteString(note + "\n")
	}
	if r.json {
		r.emit(doc)
		return
	}
	_, _ = io.WriteString(r.env.Stdout, b.String())
}

func (r *runner) printSnippets(agent agentsetup.Agent, entry agentsetup.Entry, files []setupFile, again string, global bool) {
	if r.json {
		type snip struct {
			Path    string `json:"path"`
			Kind    string `json:"kind"`
			Snippet string `json:"snippet"`
		}
		doc := struct {
			Files []snip `json:"files"`
		}{}
		for _, f := range files {
			doc.Files = append(doc.Files, snip{f.rel, f.Kind.String(), f.Snippet(entry)})
		}
		r.emit(doc)
		return
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	p("# %s: run `%s --write` to make these edits, or make them by hand\n", agent.Title, again)
	for _, f := range files {
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
		p("\n# session id: %s sets none; the server assigns one unless STARFIX_SESSION is set\n", agent.Title)
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
