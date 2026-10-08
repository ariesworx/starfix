package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ariesworx/starfix/internal/agentsetup"
	"github.com/ariesworx/starfix/internal/proto"
)

// setupDesktop sets a desktop app up for the project setup runs in. The
// app's config is the person's own, at a per-platform path, and the app
// starts servers with no working directory and without the shell's PATH,
// so the entry pins this checkout's root and an absolute sfx. command is
// --command, or "" for this sfx. --global changes nothing: the config is
// always the person's.
func (r *runner) setupDesktop(agent agentsetup.Agent, command string, mode setupMode) error {
	usage := "setup " + agent.Name + " [--write|--check|--remove] [--command ABSOLUTE-PATH]"
	goos := r.env.GOOS
	home, _ := r.env.UserHomeDir()
	path, err := agent.ConfigPath(goos, home, r.env.Getenv("APPDATA"))
	switch {
	case errors.Is(err, agentsetup.ErrNoDesktop):
		return proto.Errf(proto.CodeInvalid,
			"run it on the Mac or Windows PC that runs "+agent.Title+"; here, set up a terminal or IDE agent such as `sfx setup claude-code`",
			agent.Title+" runs only on macOS and Windows")
	case err != nil:
		return proto.Errf(proto.CodeInvalid, "set HOME on macOS, or APPDATA on Windows",
			fmt.Sprintf("cannot find %s's config: %v", agent.Title, err))
	}
	if command != "" && !absPath(goos, command) {
		return usagef(usage, "--command must be an absolute path: %s starts servers without your shell's PATH", agent.Title)
	}
	root, err := r.setupRoot(false)
	if err != nil {
		return err
	}
	if command == "" {
		if command, err = r.env.Executable(); err != nil || command == "" {
			return proto.Errf(proto.CodeInvalid, "pass --command with the absolute path of sfx",
				"cannot find the path of this sfx")
		}
	}
	entry := agentsetup.DesktopEntry(command, root)
	again := "sfx setup " + agent.Name

	// The config is the person's: new, it is private, since it holds every
	// server's env, other servers' tokens among them. On Windows it is under
	// %APPDATA%, which need not be under home: then only its directory and
	// the file itself are checked.
	base := fileBase{dir: home, user: true, home: home}
	if rel, err := filepath.Rel(home, path); home == "" || err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		base.dir = filepath.Dir(filepath.Dir(path))
	}
	d := &diskFile{path: path, base: base}
	if err := d.read(); err != nil {
		return err
	}
	if dir, ok := agent.ProjectDir(d.orig, entry.Server); ok && dir != root && mode != (setupMode{}) {
		return proto.Errf(proto.CodeExists,
			fmt.Sprintf("remove it with `sfx -C %s setup %s --remove`, or rename this checkout's directory", dir, agent.Name),
			fmt.Sprintf("%s already starts %s, not this checkout (in %s)", entry.Server, dir, path))
	}
	plan := setupPlan{Agent: agent, files: []setupFile{{Target: agent.DesktopTarget(path), disk: d, rel: path}}}
	return r.applySetup([]setupPlan{plan}, entry, mode, false, false, again, []*diskFile{d})
}

// absPath reports whether p is absolute on goos, which need not be the
// platform sfx runs on.
func absPath(goos, p string) bool {
	if goos != "windows" {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return true // a UNC path
	}
	return len(p) >= 3 && (p[0]|0x20 >= 'a' && p[0]|0x20 <= 'z') && p[1] == ':' && (p[2] == '\\' || p[2] == '/')
}

// sfxPath is the absolute path a desktop app should start sfx by. It
// prefers sfx's entry on PATH, when that is this program, over the
// program's own path: a package manager links a stable name on PATH
// (Homebrew's /opt/homebrew/bin/sfx) to a versioned file it deletes on
// upgrade, so the resolved path would break and the link does not.
// `sfx upgrade` replaces the file in place, so either path survives it.
func sfxPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := exec.LookPath("sfx"); err == nil {
		if p, err = filepath.Abs(p); err == nil && sameFile(p, exe) {
			return p, nil
		}
	}
	return exe, nil
}

// sameFile reports whether a and b, symlinks followed, are one file.
func sameFile(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	return err == nil && os.SameFile(sa, sb)
}
