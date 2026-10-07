package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A hostile repository commits links that make setup copy a secret into
// a file the person then commits, or write their global hook without
// --global. Setup reads and writes no project file through a link (C-1).
func TestSetupRefusesProjectLinks(t *testing.T) {
	const secret = `{"auths": {"registry.example.com": {"auth": "c2VjcmV0"}}}` //nolint:gosec // an invented stand-in for a secret
	tests := []struct {
		name, agent string
		// link is relative to the repository root, target to the
		// outside directory unless inside is set.
		link, target string
		inside       bool
	}{
		{name: "config file to a secret", agent: "claude-code", link: ".mcp.json", target: "secret.json"},
		{name: "settings directory to the home one", agent: "claude-code", link: ".claude", target: "dot-claude"},
		{name: "instruction file", agent: "claude-code", link: "CLAUDE.md", target: "notes.md"},
		{name: "codex directory", agent: "codex", link: ".codex", target: "dot-codex"},
		{name: "nested directory", agent: "cursor", link: ".cursor/rules", target: "rules"},
		{name: "a link inside the repository", agent: "claude-code", link: ".mcp.json", target: "other.json", inside: true},
	}
	for _, tc := range tests {
		for _, mode := range []string{"--write", "--check", "--remove", ""} {
			t.Run(tc.name+" "+mode, func(t *testing.T) {
				root, sub := repoWithConfig(t)
				outside := t.TempDir()
				base := outside
				if tc.inside {
					base = root
				}
				target := filepath.Join(base, tc.target)
				if filepath.Ext(tc.target) != "" {
					writeFile(t, target, secret)
				} else {
					writeFile(t, filepath.Join(target, "settings.json"), secret)
				}
				symlink(t, target, filepath.Join(root, filepath.FromSlash(tc.link)))
				before, beforeOut := snapshot(t, root), snapshot(t, outside)

				args := []string{"-C", sub, "setup", tc.agent}
				if mode != "" {
					args = append(args, mode)
				}
				code, out, errb := runIn(t, t.TempDir(), args...)
				if mode == "" { // printing reads nothing and writes nothing
					if code != ExitOK || strings.Contains(out, "c2VjcmV0") {
						t.Fatalf("print: exit %d\n%s%s", code, out, errb)
					}
					return
				}
				if code != ExitFailure || !strings.Contains(errb, "symbolic link") || !strings.Contains(errb, "fix: ") {
					t.Fatalf("%v: exit %d\n%s%s", args, code, out, errb)
				}
				if strings.Contains(out+errb, "c2VjcmV0") {
					t.Fatal("the secret reached the output")
				}
				sameFiles(t, before, snapshot(t, root))
				sameFiles(t, beforeOut, snapshot(t, outside))
				if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(tc.link))); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
					t.Fatalf("the link was replaced: %v", err)
				}
			})
		}
	}
}

// The person's own files may be links (a dotfiles repository), but only
// to a file of theirs inside their home: setup then edits the target and
// keeps the link.
func TestSetupGlobalLinks(t *testing.T) {
	tests := []struct {
		name         string
		link, target string // link relative to home; target to home, or to outside if outsideTarget
		outside      bool
		ok           bool
	}{
		{name: "settings directory in dotfiles", link: ".claude", target: "dotfiles/claude", ok: true},
		{name: "config file in dotfiles", link: ".claude.json", target: "dotfiles/claude.json", ok: true},
		{name: "settings directory outside home", link: ".claude", target: "claude", outside: true},
		{name: "config file outside home", link: ".claude.json", target: "claude.json", outside: true},
		{name: "a link to a link outside home", link: ".claude.json", target: "dotfiles/hop.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			base := home
			if tc.outside {
				base = outside
			}
			target := filepath.Join(base, filepath.FromSlash(tc.target))
			isDir := filepath.Ext(tc.target) == ""
			if isDir {
				if err := os.MkdirAll(target, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if tc.target == "dotfiles/hop.json" {
				writeFile(t, filepath.Join(outside, "far.json"), "{}")
				symlink(t, filepath.Join(outside, "far.json"), target)
			} else {
				writeFile(t, target, "{}")
			}
			symlink(t, target, filepath.Join(home, tc.link))
			beforeOut := snapshot(t, outside)

			code, out, errb := runIn(t, home, "-C", t.TempDir(), "setup", "claude-code", "--global", "--write")
			if !tc.ok {
				if code != ExitFailure || !strings.Contains(errb, "symbolic link") || !strings.Contains(errb, "fix: ") {
					t.Fatalf("exit %d\n%s%s", code, out, errb)
				}
				sameFiles(t, beforeOut, snapshot(t, outside))
				return
			}
			if code != ExitOK {
				t.Fatalf("exit %d\n%s%s", code, out, errb)
			}
			if fi, err := os.Lstat(filepath.Join(home, tc.link)); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
				t.Fatalf("the link was replaced: %v", err)
			}
			want := target
			if isDir {
				want = filepath.Join(target, "settings.json")
			}
			if !strings.Contains(readFile(t, want), "prime --hook") && !strings.Contains(readFile(t, want), "starfix") {
				t.Fatalf("the link's target was not edited:\n%s", readFile(t, want))
			}
			code, out, _ = runIn(t, home, "-C", t.TempDir(), "setup", "claude-code", "--global", "--check")
			if code != ExitOK {
				t.Fatalf("check after write: exit %d %s", code, out)
			}
		})
	}
}

// A link in home to a file another user owns is refused, even inside home.
func TestSetupGlobalLinkToOthersFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix owners")
	}
	home := t.TempDir()
	target := filepath.Join(home, "shared", "claude.json")
	writeFile(t, target, "{}")
	if err := os.Chown(target, 65534, 65534); err != nil {
		t.Skipf("cannot give the file to another user here: %v", err)
	}
	symlink(t, target, filepath.Join(home, ".claude.json"))
	code, _, errb := runIn(t, home, "-C", t.TempDir(), "setup", "claude-code", "--global", "--write")
	if code != ExitFailure || !strings.Contains(errb, "another user") || !strings.Contains(errb, "fix: ") {
		t.Fatalf("exit %d\n%s", code, errb)
	}
}

// The person's new files are private; the project's, meant to be
// committed, are not (C-9). An existing file keeps its mode.
func TestSetupModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix modes")
	}
	home := t.TempDir()
	if code, out, errb := runIn(t, home, "-C", t.TempDir(), "setup", "codex", "--global", "--write"); code != ExitOK {
		t.Fatalf("exit %d %s %s", code, out, errb)
	}
	for _, f := range []string{".codex/config.toml", ".codex/AGENTS.md", ".codex/hooks.json"} {
		if fi, err := os.Stat(filepath.Join(home, filepath.FromSlash(f))); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v; want 0600", f, fi.Mode().Perm(), err)
		}
	}
	if fi, err := os.Stat(filepath.Join(home, ".codex")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf(".codex: %v, %v; want 0700", fi.Mode().Perm(), err)
	}

	kept := filepath.Join(home, ".gemini", "settings.json")
	writeFile(t, kept, "{}")
	if err := os.Chmod(kept, 0o640); err != nil { //nolint:gosec // a mode setup must keep
		t.Fatal(err)
	}
	if code, _, errb := runIn(t, home, "-C", t.TempDir(), "setup", "gemini", "--global", "--write"); code != ExitOK {
		t.Fatalf("gemini: exit %d %s", code, errb)
	}
	if fi, err := os.Stat(kept); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("existing file: %v, %v; want 0640 kept", fi.Mode().Perm(), err)
	}

	root, sub := repoWithConfig(t)
	if code, _, errb := runIn(t, t.TempDir(), "-C", sub, "setup", "codex", "--write"); code != ExitOK {
		t.Fatalf("project: exit %d %s", code, errb)
	}
	if fi, err := os.Stat(filepath.Join(root, ".codex", "config.toml")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("project config: %v, %v; want 0644", fi.Mode().Perm(), err)
	}
}

// Claude Desktop's config directory may not be a link out of home.
func TestSetupClaudeDesktopLink(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	_, sub := repoWithConfig(t)
	symlink(t, outside, filepath.Join(home, "Library", "Application Support", "Claude"))
	d := desktopEnv{goos: "darwin", home: home, exe: "/usr/local/bin/sfx"}
	code, _, errb := d.run(t, "-C", sub, "setup", "claude-desktop", "--write")
	if code != ExitFailure || !strings.Contains(errb, "symbolic link") || !strings.Contains(errb, "fix: ") {
		t.Fatalf("exit %d\n%s", code, errb)
	}
	if len(snapshot(t, outside)) != 0 {
		t.Error("wrote through the link")
	}
}

// A global hook dials whatever server a repository names, so setup says
// so when it adds one (C-14).
func TestSetupGlobalHookNote(t *testing.T) {
	home := t.TempDir()
	code, out, errb := runIn(t, home, "-C", t.TempDir(), "setup", "claude-code", "--global", "--write")
	if code != ExitOK || !strings.Contains(out, "every repository") {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	if code, out, _ := runIn(t, home, "-C", t.TempDir(), "setup", "claude-code", "--global"); code != ExitOK || !strings.Contains(out, "every repository") {
		t.Fatalf("print: exit %d\n%s", code, out)
	}
}
