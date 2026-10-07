package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ariesworx/starfix/internal/agentsetup"
	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// cmdSetup prints, or with --write makes, the MCP registration for an
// agent. It edits the project's own config unless --global says the
// person's home config may be touched.
func cmdSetup(_ context.Context, r *runner, args []string) error {
	const usage = "setup claude-code|codex|gemini [--write|--check|--remove] [--global] [--command PATH]"
	fs := r.newFlags("setup")
	write := fs.Bool("write", false, "edit the config file")
	check := fs.Bool("check", false, "fail unless the registration is in place")
	remove := fs.Bool("remove", false, "take the registration out")
	global := fs.Bool("global", false, "the user's config in the home directory, not the project's")
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

	path, err := r.setupPath(agent, *global)
	if err != nil {
		return err
	}
	content, mode, err := readConfig(path)
	if err != nil {
		return err
	}
	rel := agent.Project // shown relative to the repository root
	if *global {
		rel = path
	}
	again := "starfix setup " + agent.Name
	if *global {
		again += " --global"
	}

	var out []byte
	var res agentsetup.Result
	switch {
	case *check:
		if !agent.Registered(content, entry) {
			return proto.Errf(proto.CodeNotFound, "run `"+again+" --write`",
				fmt.Sprintf("%s does not register starfix for %s", rel, agent.Title))
		}
		r.report(map[string]string{"path": rel, "result": "registered"}, rel+": registered")
		return nil
	case *remove:
		out, res, err = agent.Remove(content)
	case *write:
		out, res, err = agent.Apply(content, entry)
	default:
		r.printSnippet(agent, entry, rel, again, *global)
		return nil
	}
	if err != nil {
		return proto.Errf(proto.CodeInvalid, "fix the file by hand, or move it aside and rerun",
			fmt.Sprintf("cannot edit %s: %v", rel, err))
	}
	if res != agentsetup.Unchanged {
		if err := writeConfig(path, out, mode); err != nil {
			return err
		}
	}
	msg := rel + ": " + res.String()
	if res == agentsetup.Added && agent.Note != "" && !*global {
		msg += "\n" + agent.Note
	}
	r.report(map[string]string{"path": rel, "result": res.String()}, msg)
	return nil
}

func (r *runner) report(doc map[string]string, text string) {
	if r.json {
		r.emit(doc)
		return
	}
	_, _ = fmt.Fprintln(r.env.Stdout, text)
}

func (r *runner) printSnippet(agent agentsetup.Agent, entry agentsetup.Entry, rel, again string, global bool) {
	snippet := agent.Snippet(entry)
	if r.json {
		r.emit(map[string]string{"path": rel, "snippet": snippet})
		return
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(r.env.Stdout, format, a...) }
	p("# %s: add to %s, or run `%s --write`\n%s", agent.Title, rel, again, snippet)
	if agent.Note != "" && !global {
		p("# %s\n", agent.Note)
	}
	if agent.SessionEnv != "" {
		p("# session id: read from %s\n", agent.SessionEnv)
	} else {
		p("# session id: %s sets none; the server assigns one unless STARFIX_SESSION is set\n", agent.Title)
	}
}

// setupPath is the config file to edit: the project's, at the root of the
// repository that holds .starfix.yaml, or with global the user's.
func (r *runner) setupPath(agent agentsetup.Agent, global bool) (string, error) {
	if global {
		home, err := r.env.UserHomeDir()
		if err != nil || home == "" {
			return "", proto.Errf(proto.CodeInvalid, "set HOME, or drop --global", "cannot find the home directory")
		}
		return filepath.Join(home, filepath.FromSlash(agent.Global)), nil
	}
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return "", err
		}
		return "", proto.Errf(proto.CodeInvalid, "correct "+client.ConfigFile+"; see the README's quick start", err.Error())
	}
	return filepath.Join(cfg.Root, filepath.FromSlash(agent.Project)), nil
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

// writeConfig replaces path atomically, keeping its mode. A project
// config is meant to be committed, so a new one is 0644; it holds no
// secret.
func writeConfig(path string, b []byte, mode fs.FileMode) error {
	fail := func(err error) error {
		return proto.Errf(proto.CodeInvalid, "check the directory's permissions", fmt.Sprintf("write %s: %v", path, err))
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // .codex/ or .gemini/ in the repository
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
