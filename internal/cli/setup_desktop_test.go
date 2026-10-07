package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// desktopEnv is a macOS or Windows machine: its home, %APPDATA% and the
// running sfx are injected, never read from the test's environment.
type desktopEnv struct {
	goos, home, appdata, exe string
}

func (d desktopEnv) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, Env{Stdout: &out, Stderr: &errb, Version: "v0.3.0", GOOS: d.goos,
		Getenv: func(k string) string {
			if k == "APPDATA" {
				return d.appdata
			}
			return ""
		},
		UserHomeDir: func() (string, error) { return d.home, nil },
		Executable:  func() (string, error) { return d.exe, nil }})
	return code, out.String(), errb.String()
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // the test's temp files
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// sfx setup claude-desktop writes one entry per project into the
// app's per-user config: the absolute sfx, -C the repository root, and
// the harness. Writing, checking and removing are each idempotent, two
// projects coexist, and the person's own servers and keys stay.
func TestSetupClaudeDesktop(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  func(home string) desktopEnv
		path func(home string) string
	}{
		{name: "macOS",
			env: func(home string) desktopEnv {
				return desktopEnv{goos: "darwin", home: home, exe: "/opt/homebrew/bin/sfx"}
			},
			path: func(home string) string {
				return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
			}},
		{name: "Windows",
			env: func(home string) desktopEnv {
				return desktopEnv{goos: "windows", home: home, appdata: filepath.Join(home, "AppData", "Roaming"), exe: `C:\Users\alice\bin\sfx.exe`}
			},
			// The test runs on the host, so the Windows separator is
			// only right on Windows; the host opens the file either way.
			path: func(home string) string {
				return filepath.Join(home, "AppData", "Roaming") + `\Claude\claude_desktop_config.json`
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			d := tc.env(home)
			cfg := tc.path(home)
			widget, _ := repoWithConfig(t)
			gadget, _ := repoWithConfig(t)

			// Printing writes nothing and shows the pinned entry.
			code, out, errb := d.run(t, "-C", widget, "setup", "claude-desktop")
			if code != ExitOK || len(snapshot(t, home)) != 0 {
				t.Fatalf("print: exit %d, files %v\n%s%s", code, snapshot(t, home), out, errb)
			}
			for _, want := range []string{"# MCP server: add to " + cfg, `"command": ` + jsonString(d.exe), `"-C"`, jsonString(widget),
				`"STARFIX_HARNESS": "claude-desktop"`, "Quit Claude Desktop", "session id: Claude Desktop gives sfx mcp none"} {
				if !strings.Contains(out, want) {
					t.Errorf("print lacks %q:\n%s", want, out)
				}
			}
			if code, _, errb := d.run(t, "-C", widget, "setup", "claude-desktop", "--check"); code != ExitFailure ||
				!strings.Contains(errb, "fix: run `sfx setup claude-desktop --write`") {
				t.Fatalf("check before write: exit %d %s", code, errb)
			}

			// The person's file, with another server and another key.
			mine := `{"mcpServers": {"files": {"command": "npx", "args": ["-y", "x"]}}, "globalShortcut": "Alt+Space"}`
			if err := os.MkdirAll(filepath.Dir(cfg), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg, []byte(mine), 0o600); err != nil {
				t.Fatal(err)
			}
			code, out, errb = d.run(t, "-C", widget, "setup", "claude-desktop", "--write")
			if code != ExitOK || out != cfg+": added\nQuit Claude Desktop completely and reopen it to start the server. Each launch is a new starfix session.\n" {
				t.Fatalf("write: exit %d\n%s%s", code, out, errb)
			}
			first := readFile(t, cfg)
			for _, want := range []string{`"files": {`, `"globalShortcut": "Alt+Space"`, `"starfix-` + filepath.Base(widget) + `": {`} {
				if !strings.Contains(first, want) {
					t.Errorf("config lacks %s:\n%s", want, first)
				}
			}
			if code, out, _ := d.run(t, "-C", widget, "setup", "claude-desktop", "--write"); code != ExitOK || out != cfg+": unchanged\n" ||
				readFile(t, cfg) != first {
				t.Fatalf("second write: exit %d %s", code, out)
			}
			if code, out, errb := d.run(t, "-C", widget, "setup", "claude-desktop", "--check"); code != ExitOK || out != cfg+": registered\n" {
				t.Fatalf("check: exit %d %s%s", code, out, errb)
			}

			// A second project gets its own entry beside the first.
			if code, _, errb := d.run(t, "-C", gadget, "setup", "claude-desktop", "--write"); code != ExitOK {
				t.Fatalf("gadget: exit %d %s", code, errb)
			}
			both := readFile(t, cfg)
			if !strings.Contains(both, jsonString(widget)) || !strings.Contains(both, jsonString(gadget)) {
				t.Fatalf("two projects:\n%s", both)
			}
			if code, _, _ := d.run(t, "-C", widget, "setup", "claude-desktop", "--check"); code != ExitOK {
				t.Fatal("widget lost its entry to gadget")
			}

			// Removing one leaves the other and the person's own.
			if code, out, _ := d.run(t, "-C", gadget, "setup", "claude-desktop", "--remove"); code != ExitOK || out != cfg+": removed\n" {
				t.Fatalf("remove: exit %d %s", code, out)
			}
			if got := readFile(t, cfg); got != first {
				t.Fatalf("after removing gadget:\n%s\nwant\n%s", got, first)
			}
			if code, out, _ := d.run(t, "-C", gadget, "setup", "claude-desktop", "--remove"); code != ExitOK || out != cfg+": unchanged\n" ||
				readFile(t, cfg) != first {
				t.Fatalf("second remove: exit %d %s", code, out)
			}
			if code, _, _ := d.run(t, "-C", widget, "setup", "claude-desktop", "--remove"); code != ExitOK ||
				strings.Contains(readFile(t, cfg), "starfix") || !strings.Contains(readFile(t, cfg), `"globalShortcut"`) {
				t.Fatalf("remove widget: exit %d\n%s", code, readFile(t, cfg))
			}
		})
	}
}

// jsonString is s as a JSON string literal.
func jsonString(s string) string {
	return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"`
}

// A new config, with its directory, is private to the person: the file
// holds every server's env, other servers' tokens among them.
func TestSetupClaudeDesktopNewFile(t *testing.T) {
	home := t.TempDir()
	root, sub := repoWithConfig(t)
	d := desktopEnv{goos: "darwin", home: home, exe: "/usr/local/bin/sfx"}
	if code, _, errb := d.run(t, "-C", sub, "setup", "claude-desktop", "--write", "--global"); code != ExitOK {
		t.Fatalf("exit %d %s", code, errb)
	}
	cfg := filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	st, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(filepath.Dir(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Errorf("modes: file %v, dir %v; want 0600 and 0700", st.Mode().Perm(), dir.Mode().Perm())
	}
	// -C is the repository root, not the directory setup ran in.
	if got := readFile(t, cfg); !strings.Contains(got, jsonString(root)+",") || strings.Contains(got, jsonString(sub)) {
		t.Errorf("entry does not pin the root %s:\n%s", root, got)
	}
	if len(snapshot(t, root)) != 1 {
		t.Error("wrote into the repository")
	}
}

func TestSetupClaudeDesktopRefusals(t *testing.T) {
	_, sub := repoWithConfig(t)
	mac := desktopEnv{goos: "darwin", home: t.TempDir(), exe: "/usr/local/bin/sfx"}
	tests := []struct {
		name string
		env  desktopEnv
		args []string
		code int
		want string
	}{
		{name: "Linux", env: desktopEnv{goos: "linux", home: t.TempDir(), exe: "/usr/bin/sfx"},
			args: []string{"-C", sub, "setup", "claude-desktop"}, code: ExitFailure, want: "Claude Desktop runs only on macOS and Windows"},
		{name: "Windows without APPDATA", env: desktopEnv{goos: "windows", home: t.TempDir(), exe: `C:\sfx.exe`},
			args: []string{"-C", sub, "setup", "claude-desktop", "--write"}, code: ExitFailure, want: "APPDATA"},
		{name: "outside a starfix repository", env: mac,
			args: []string{"-C", t.TempDir(), "setup", "claude-desktop", "--write"}, code: ExitFailure, want: ".starfix.yaml not found"},
		{name: "relative command", env: mac,
			args: []string{"-C", sub, "setup", "claude-desktop", "--command", "sfx"}, code: ExitUsage, want: "absolute path"},
		{name: "relative Windows command", env: desktopEnv{goos: "windows", home: t.TempDir(), appdata: t.TempDir(), exe: `C:\sfx.exe`},
			args: []string{"-C", sub, "setup", "claude-desktop", "--command", `bin\sfx.exe`}, code: ExitUsage, want: "absolute path"},
		{name: "executable not found", env: desktopEnv{goos: "darwin", home: t.TempDir()},
			args: []string{"-C", sub, "setup", "claude-desktop"}, code: ExitFailure, want: "--command"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errb := tc.env.run(t, tc.args...)
			if code != tc.code || !strings.Contains(errb, tc.want) || !strings.Contains(errb, "fix: ") {
				t.Errorf("%v: exit %d, want %d and %q\n%s", tc.args, code, tc.code, tc.want, errb)
			}
			if len(snapshot(t, tc.env.home)) != 0 {
				t.Error("a refused setup wrote files")
			}
		})
	}
}

// Windows accepts an absolute --command; it is written as given.
func TestSetupClaudeDesktopCommand(t *testing.T) {
	_, sub := repoWithConfig(t)
	d := desktopEnv{goos: "windows", home: t.TempDir(), appdata: t.TempDir(), exe: `C:\ignored.exe`}
	code, out, errb := d.run(t, "-C", sub, "setup", "claude-desktop", "--command", `D:\tools\sfx.exe`)
	if code != ExitOK || !strings.Contains(out, `"command": "D:\\tools\\sfx.exe"`) {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
}

// Two checkouts with the same directory name would share an entry: setup
// refuses to repoint or remove the other one's.
func TestSetupClaudeDesktopNameClash(t *testing.T) {
	home := t.TempDir()
	d := desktopEnv{goos: "darwin", home: home, exe: "/usr/local/bin/sfx"}
	one, two := filepath.Join(t.TempDir(), "widget"), filepath.Join(t.TempDir(), "widget")
	for _, dir := range []string{one, two} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".starfix.yaml"), []byte(testConfig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if code, _, errb := d.run(t, "-C", one, "setup", "claude-desktop", "--write"); code != ExitOK {
		t.Fatalf("exit %d %s", code, errb)
	}
	before := snapshot(t, home)
	for _, mode := range []string{"--write", "--remove", "--check"} {
		code, _, errb := d.run(t, "-C", two, "setup", "claude-desktop", mode)
		if code != ExitFailure || !strings.Contains(errb, "starfix-widget already starts "+one) || !strings.Contains(errb, "fix: ") {
			t.Errorf("%s: exit %d %s", mode, code, errb)
		}
	}
	sameFiles(t, before, snapshot(t, home))
}

// --all sets up the agents a repository holds files for; Claude
// Desktop's file is the person's, outside the repository, so --all
// leaves it out.
func TestSetupAllSkipsDesktop(t *testing.T) {
	_, sub := repoWithConfig(t)
	home := t.TempDir()
	d := desktopEnv{goos: "darwin", home: home, exe: "/usr/local/bin/sfx"}
	for _, args := range [][]string{{"setup", "--all", "--write"}, {"setup", "--all", "--global", "--write"}} {
		code, out, errb := d.run(t, append([]string{"-C", sub}, args...)...)
		if code != ExitOK || strings.Contains(out, "claude-desktop") {
			t.Fatalf("%v: exit %d\n%s%s", args, code, out, errb)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "Library")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("--all touched Claude Desktop's config: %v", err)
	}
}
