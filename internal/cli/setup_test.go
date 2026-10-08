package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConfig = "project: 6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f\nserver:\n  host: starfix.example.com\n  host_key: SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU\n"

// repoWithConfig is a repository root holding .starfix.yaml, and a
// subdirectory to run from.
func repoWithConfig(t *testing.T) (root, sub string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".starfix.yaml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	sub = filepath.Join(root, "src")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, sub
}

func runIn(t *testing.T, home string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(t.Context(), args, Env{Stdout: &out, Stderr: &errb,
		Getenv: func(string) string { return "" }, Version: "v0.3.0",
		UserHomeDir: func() (string, error) { return home, nil }})
	return code, out.String(), errb.String()
}

// snapshot reads every file under dir, and where each link points.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 { // recorded, never followed
			target, err := os.Readlink(p)
			files[p] = "-> " + target
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // a test's own temp files
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestSetupWriteIsIdempotent(t *testing.T) {
	// files maps each file setup writes to a string it must hold; parts
	// is how many parts setup reports, when a file holds two; note is in
	// the output of the first write and the printed snippets.
	for _, agent := range []struct {
		name  string
		files map[string]string
		parts int
		note  string
	}{
		{name: "claude-code", files: map[string]string{".mcp.json": `"type": "stdio"`, "CLAUDE.md": "<!-- starfix:begin -->",
			".claude/settings.json": `"command": "sfx prime --hook"`}, note: "approve"},
		{name: "codex", files: map[string]string{".codex/config.toml": "[mcp_servers.starfix]", "AGENTS.md": "<!-- starfix:end -->",
			".codex/hooks.json": `"command": "sfx prime --hook=codex"`}, note: "/hooks"},
		{name: "gemini", files: map[string]string{".gemini/settings.json": `"command": "sfx prime --hook=gemini"`, "GEMINI.md": "`prime`"},
			parts: 3, note: "trusted folder"},
		{name: "cursor", files: map[string]string{".cursor/mcp.json": `"mcpServers"`, ".cursor/rules/starfix.mdc": "alwaysApply: true",
			".cursor/hooks.json": `"command": "sfx prime --hook=cursor"`}, note: "Settings › MCP"},
		{name: "vscode", files: map[string]string{".vscode/mcp.json": `"servers"`, ".github/copilot-instructions.md": "`finish`",
			".github/hooks/starfix.json": `"command": "sfx prime --hook=vscode"`}, note: "Preview"},
		{name: "junie", files: map[string]string{".junie/mcp/mcp.json": `"STARFIX_HARNESS": "junie"`, "AGENTS.md": "`start`"},
			note: "--global"},
		{name: "jetbrains", files: map[string]string{".aiassistant/rules/starfix.md": "apply: always"},
			note: `"STARFIX_HARNESS": "jetbrains"`},
	} {
		t.Run(agent.name, func(t *testing.T) {
			root, sub := repoWithConfig(t)
			home := t.TempDir()
			parts := agent.parts
			if parts == 0 {
				parts = len(agent.files)
			}

			// Without --write nothing is written; the output shows every file.
			code, out, errb := runIn(t, home, "-C", sub, "setup", agent.name)
			if code != ExitOK || !strings.Contains(out, "--write") || !strings.Contains(out, agent.note) {
				t.Fatalf("print: exit %d\n%s%s", code, out, errb)
			}
			for f, want := range agent.files {
				if !strings.Contains(out, "# ") || !strings.Contains(out, f) || !strings.Contains(out, want) {
					t.Fatalf("print lacks %s (%s):\n%s", f, want, out)
				}
			}
			if code, _, errb := runIn(t, home, "-C", sub, "setup", agent.name, "--check"); code != ExitFailure ||
				!strings.Contains(errb, "fix: run `sfx setup "+agent.name+" --write`") {
				t.Fatalf("check before write: exit %d %s", code, errb)
			}
			if len(snapshot(t, root)) != 1 {
				t.Fatal("printing wrote a file")
			}

			code, out, errb = runIn(t, home, "-C", sub, "setup", agent.name, "--write")
			if code != ExitOK || strings.Count(out, ": added\n") != parts || !strings.Contains(out, agent.note) {
				t.Fatalf("write: exit %d\n%s%s", code, out, errb)
			}
			first := snapshot(t, root)
			if len(first) != 1+len(agent.files) {
				t.Fatalf("wrote %d files, want %d", len(first)-1, len(agent.files))
			}
			for f, want := range agent.files {
				if b := first[filepath.Join(root, filepath.FromSlash(f))]; !strings.Contains(b, want) {
					t.Fatalf("%s:\n%s", f, b)
				}
			}
			code, out, _ = runIn(t, home, "-C", sub, "setup", agent.name, "--write")
			if code != ExitOK || strings.Count(out, ": unchanged\n") != parts {
				t.Fatalf("second write: exit %d %s", code, out)
			}
			sameFiles(t, first, snapshot(t, root))
			if code, out, errb := runIn(t, home, "-C", sub, "setup", agent.name, "--check"); code != ExitOK ||
				strings.Count(out, ": registered\n") != parts {
				t.Fatalf("check: %d %s %s", code, out, errb)
			}
			if len(snapshot(t, home)) != 0 {
				t.Fatal("wrote to the home directory without --global")
			}
			if code, out, _ := runIn(t, home, "-C", sub, "setup", agent.name, "--remove"); code != ExitOK ||
				strings.Count(out, ": removed\n") != parts {
				t.Fatalf("remove: %d %s", code, out)
			}
			// A file that held only starfix's part is gone; the configs stay.
			for f := range agent.files {
				_, err := os.Stat(filepath.Join(root, filepath.FromSlash(f)))
				own := strings.HasSuffix(f, ".md") || strings.HasSuffix(f, ".mdc") || f == ".github/hooks/starfix.json"
				if gone := errors.Is(err, fs.ErrNotExist); gone != own {
					t.Errorf("%s after remove: %v", f, err)
				}
			}
			removed := snapshot(t, root)
			if code, out, _ := runIn(t, home, "-C", sub, "setup", agent.name, "--remove"); code != ExitOK ||
				strings.Count(out, ": unchanged\n") != parts {
				t.Fatalf("second remove: %d %s", code, out)
			}
			sameFiles(t, removed, snapshot(t, root))
		})
	}
}

func sameFiles(t *testing.T, a, b map[string]string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("files changed: %v vs %v", a, b)
	}
	for p, c := range a {
		if b[p] != c {
			t.Fatalf("%s changed:\n%s\n---\n%s", p, c, b[p])
		}
	}
}

// A repository set up before usage capture has the SessionStart hook
// alone. --check names the missing usage hooks and the fix; --write adds
// them, reporting the hook file updated and nothing else changed, and is
// then idempotent; --remove takes every starfix hook out.
func TestSetupUpgradesPrimeOnlyHooks(t *testing.T) {
	root, sub := repoWithConfig(t)
	home := t.TempDir()
	if code, out, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--write"); code != ExitOK {
		t.Fatalf("write: exit %d\n%s%s", code, out, errb)
	}
	settings := filepath.Join(root, ".claude", "settings.json")
	primeOnly := `{
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "sfx prime --hook"
          }
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(settings, []byte(primeOnly), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--check")
	if want := "sfx: starfix is not set up for Claude Code in .claude/settings.json (Stop, SubagentStop and SessionEnd hooks)\n" +
		"fix: run `sfx setup claude-code --write`\n"; code != ExitFailure || errb != want {
		t.Fatalf("check of a prime-only install: exit %d\n%s\nwant %d\n%s", code, errb, ExitFailure, want)
	}
	code, out, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--write")
	if want := ".mcp.json: unchanged\nCLAUDE.md: unchanged\n.claude/settings.json: updated\n"; code != ExitOK || out != want {
		t.Fatalf("upgrade write: exit %d\n%s%s\nwant\n%s", code, out, errb, want)
	}
	first := snapshot(t, root)
	if b := first[settings]; strings.Count(b, `"command": "sfx usage --hook"`) != 3 || !strings.Contains(b, `"command": "sfx prime --hook"`) {
		t.Fatalf("settings after the upgrade:\n%s", b)
	}
	if code, out, _ := runIn(t, home, "-C", sub, "setup", "claude-code", "--write"); code != ExitOK || strings.Count(out, ": unchanged\n") != 3 {
		t.Fatalf("second write: exit %d %s", code, out)
	}
	sameFiles(t, first, snapshot(t, root))
	if code, _, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--check"); code != ExitOK {
		t.Fatalf("check after the upgrade: exit %d %s", code, errb)
	}
	if code, _, errb := runIn(t, home, "-C", sub, "setup", "claude-code", "--remove"); code != ExitOK {
		t.Fatalf("remove: exit %d %s", code, errb)
	}
	// Claude Code's settings file is the person's: emptied, it is kept.
	if b, err := os.ReadFile(settings); err != nil || string(b) != "{}\n" { //nolint:gosec // the test's temp repository
		t.Errorf("settings after remove = %q, %v; want {} with every starfix hook gone", b, err)
	}
}

// Setup adds its parts to files that hold other things, and removing them
// gives the files back as they were.
func TestSetupKeepsOtherContent(t *testing.T) {
	root, sub := repoWithConfig(t)
	before := map[string]string{
		"CLAUDE.md": "# Project\n\nUse tabs.\n",
		".claude/settings.json": `{
  "permissions": {
    "deny": [
      "Read(./.env)"
    ]
  },
  "hooks": {
    "SessionStart": [
      {
        "matcher": "startup",
        "hooks": [
          {
            "type": "command",
            "command": "echo hi"
          }
        ]
      }
    ]
  }
}
`,
		".mcp.json": `{
  "mcpServers": {
    "figma": {
      "type": "http",
      "url": "https://mcp.example.com"
    }
  }
}
`,
	}
	for f, c := range before {
		p := filepath.Join(root, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	orig := snapshot(t, root)
	if code, out, errb := runIn(t, t.TempDir(), "-C", sub, "setup", "claude-code", "--write"); code != ExitOK ||
		strings.Count(out, ": added\n") != 3 {
		t.Fatalf("write: %d %s %s", code, out, errb)
	}
	after := snapshot(t, root)
	for f, c := range before {
		got := after[filepath.Join(root, filepath.FromSlash(f))]
		if f == "CLAUDE.md" && !strings.HasPrefix(got, c+"\n<!-- starfix:begin -->\n") {
			t.Errorf("%s:\n%s", f, got)
		}
		for _, keep := range []string{"Use tabs.", "Read(./.env)", `"echo hi"`, `"figma"`} {
			if strings.Contains(c, keep) && !strings.Contains(got, keep) {
				t.Errorf("%s lost %s:\n%s", f, keep, got)
			}
		}
	}
	if st, err := os.Stat(filepath.Join(root, ".mcp.json")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("mode not kept: %v %v", st.Mode(), err)
	}
	if code, _, errb := runIn(t, t.TempDir(), "-C", sub, "setup", "claude-code", "--remove"); code != ExitOK {
		t.Fatalf("remove: %d %s", code, errb)
	}
	got := snapshot(t, root)
	for p, c := range orig {
		if filepath.Base(p) == ".mcp.json" {
			continue // keeps an empty mcpServers
		}
		if got[p] != c {
			t.Errorf("%s not restored:\n%s\nwant\n%s", p, got[p], c)
		}
	}
}

func TestSetupGlobal(t *testing.T) {
	home := t.TempDir()
	// --global needs no repository.
	code, out, errb := runIn(t, home, "-C", t.TempDir(), "setup", "gemini", "--global", "--write", "--command", "/opt/bin/starfix")
	if code != ExitOK || !strings.Contains(out, "added") {
		t.Fatalf("exit %d %s %s", code, out, errb)
	}
	b, err := os.ReadFile(filepath.Join(home, ".gemini", "settings.json")) //nolint:gosec // the test's temp home
	if err != nil || !strings.Contains(string(b), `"command": "/opt/bin/starfix"`) {
		t.Fatalf("%s %v", b, err)
	}
}

func TestSetupRefusals(t *testing.T) {
	_, sub := repoWithConfig(t)
	tests := []struct {
		args []string
		code int
		want string
	}{
		{[]string{"setup"}, ExitUsage, "setup needs one agent, or --all: claude-code, claude-desktop, codex, cursor, gemini, jetbrains, junie, vscode"},
		{[]string{"setup", "aider"}, ExitUsage, `unknown agent "aider"`},
		{[]string{"setup", "codex", "--all"}, ExitUsage, "--all takes no agent"},
		{[]string{"setup", "vscode", "--global", "--write"}, ExitFailure, "fix: drop --global"},
		{[]string{"setup", "jetbrains", "--global"}, ExitFailure, "fix: drop --global"},
		{[]string{"setup", "codex", "--write", "--remove"}, ExitUsage, "at most one"},
		{[]string{"-C", t.TempDir(), "setup", "codex", "--write"}, ExitFailure, ".starfix.yaml not found"},
	}
	for _, tc := range tests {
		args := tc.args
		if args[0] == "setup" {
			args = append([]string{"-C", sub}, args...)
		}
		code, _, errb := runIn(t, t.TempDir(), args...)
		if code != tc.code || !strings.Contains(errb, tc.want) || !strings.Contains(errb, "fix: ") {
			t.Errorf("%v: exit %d\n%s", tc.args, code, errb)
		}
	}
	bad := filepath.Join(filepath.Dir(sub), ".mcp.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runIn(t, t.TempDir(), "-C", sub, "setup", "claude-code", "--write"); code != ExitFailure ||
		!strings.Contains(errb, "cannot edit .mcp.json: not JSON") {
		t.Errorf("bad json: exit %d %s", code, errb)
	}
	// One file that cannot be edited leaves all of them alone.
	if err := os.Remove(bad); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(filepath.Dir(sub), ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"hooks": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runIn(t, t.TempDir(), "-C", sub, "setup", "claude-code", "--write"); code != ExitFailure ||
		!strings.Contains(errb, "cannot edit .claude/settings.json: hooks: not a JSON object") {
		t.Errorf("bad settings: exit %d %s", code, errb)
	}
	if files := snapshot(t, filepath.Dir(sub)); len(files) != 2 {
		t.Errorf("a refused setup wrote files: %v", files)
	}
}

func TestSetupGlobalClaudeCode(t *testing.T) {
	home := t.TempDir()
	code, out, errb := runIn(t, home, "-C", t.TempDir(), "setup", "claude-code", "--global", "--write")
	if code != ExitOK || strings.Count(out, ": added\n") != 3 || strings.Contains(out, "approve") {
		t.Fatalf("exit %d %s %s", code, out, errb)
	}
	for _, f := range []string{".claude.json", ".claude/CLAUDE.md", ".claude/settings.json"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// Junie runs hooks only from the home config, so only --global writes one.
func TestSetupGlobalJunie(t *testing.T) {
	home := t.TempDir()
	code, out, errb := runIn(t, home, "-C", t.TempDir(), "setup", "junie", "--global", "--write")
	if code != ExitOK || strings.Count(out, ": added\n") != 3 {
		t.Fatalf("exit %d %s %s", code, out, errb)
	}
	b, err := os.ReadFile(filepath.Join(home, ".junie", "config.json")) //nolint:gosec // the test's temp home
	if err != nil || !strings.Contains(string(b), `"command": "sfx prime --hook=junie"`) {
		t.Fatalf("%s %v", b, err)
	}
}

// lineOf is the line of out that starts with prefix.
func lineOf(out, prefix string) string {
	for l := range strings.Lines(out) {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// setup --all sets up every agent in one run. A file two agents share,
// or one agent keeps two parts in, is edited once with both parts, and a
// second run changes nothing.
func TestSetupAll(t *testing.T) {
	root, sub := repoWithConfig(t)
	home := t.TempDir()

	code, out, errb := runIn(t, home, "-C", sub, "setup", "--all")
	if code != ExitOK || len(snapshot(t, root)) != 1 {
		t.Fatalf("print: exit %d, wrote files\n%s%s", code, out, errb)
	}
	for _, name := range []string{"Claude Code", "Codex", "Cursor", "Gemini CLI", "JetBrains AI Assistant", "Junie", "VS Code"} {
		if !strings.Contains(out, "# "+name+": ") {
			t.Errorf("print lacks %s:\n%s", name, out)
		}
	}
	if code, _, errb := runIn(t, home, "-C", sub, "setup", "--all", "--check"); code != ExitFailure ||
		!strings.Contains(errb, "fix: run `sfx setup --all --write`") {
		t.Fatalf("check before write: exit %d %s", code, errb)
	}

	code, out, errb = runIn(t, home, "-C", sub, "setup", "--all", "--write")
	if code != ExitOK {
		t.Fatalf("write: exit %d\n%s%s", code, out, errb)
	}
	// One summary per agent, in name order.
	var names []string
	for l := range strings.Lines(out) {
		if name, _, ok := strings.Cut(l, ": "); ok && !strings.HasPrefix(l, " ") {
			names = append(names, name)
		}
	}
	if got := strings.Join(names, " "); got != "claude-code codex cursor gemini jetbrains junie vscode" {
		t.Fatalf("summaries for %s:\n%s", got, out)
	}
	if l := lineOf(out, "codex: "); !strings.Contains(l, "AGENTS.md added") {
		t.Errorf("codex: %s", l)
	}
	if l := lineOf(out, "junie: "); !strings.Contains(l, "AGENTS.md unchanged") {
		t.Errorf("junie: %s", l)
	}
	first := snapshot(t, root)
	agents := first[filepath.Join(root, "AGENTS.md")]
	if strings.Count(agents, "<!-- starfix:begin -->") != 1 {
		t.Errorf("AGENTS.md:\n%s", agents)
	}
	gemini := first[filepath.Join(root, ".gemini", "settings.json")]
	if !strings.Contains(gemini, `"mcpServers"`) || !strings.Contains(gemini, `"SessionStart"`) {
		t.Errorf(".gemini/settings.json lost a part:\n%s", gemini)
	}
	if len(snapshot(t, home)) != 0 {
		t.Fatal("wrote to the home directory without --global")
	}

	code, out, _ = runIn(t, home, "-C", sub, "setup", "--all", "--write")
	if code != ExitOK || strings.Contains(out, "added") || strings.Contains(out, "updated") {
		t.Fatalf("second write: exit %d\n%s", code, out)
	}
	sameFiles(t, first, snapshot(t, root))
	if code, out, errb := runIn(t, home, "-C", sub, "setup", "--all", "--check"); code != ExitOK || strings.Count(out, "\n") != 7 {
		t.Fatalf("check: exit %d\n%s%s", code, out, errb)
	}
	if code, out, errb := runIn(t, home, "--json", "-C", sub, "setup", "--all", "--check"); code != ExitOK || strings.Count(out, "\n") != 1 ||
		!strings.Contains(out, `"agent":"junie"`) {
		t.Fatalf("check --json: exit %d\n%s%s", code, out, errb)
	}

	if code, out, _ := runIn(t, home, "-C", sub, "setup", "--all", "--remove"); code != ExitOK || strings.Contains(out, "added") {
		t.Fatalf("remove: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("AGENTS.md after remove: %v", err)
	}
	removed := snapshot(t, root)
	if code, out, _ := runIn(t, home, "-C", sub, "setup", "--all", "--remove"); code != ExitOK || strings.Contains(out, "removed") {
		t.Fatalf("second remove: exit %d\n%s", code, out)
	}
	sameFiles(t, removed, snapshot(t, root))
}

// setup --all --global skips the agents with no home config, saying why.
func TestSetupAllGlobal(t *testing.T) {
	home := t.TempDir()
	code, out, errb := runIn(t, home, "-C", t.TempDir(), "setup", "--all", "--global", "--write")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, errb)
	}
	for _, name := range []string{"vscode", "jetbrains"} {
		if l := lineOf(out, name+": "); !strings.Contains(l, "skipped") || !strings.Contains(l, "fix: drop --global") {
			t.Errorf("%s: %q", name, l)
		}
	}
	for _, f := range []string{".claude/settings.json", ".codex/hooks.json", ".cursor/hooks.json", ".gemini/settings.json", ".junie/config.json"} {
		if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(f))); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

// The printed snippets say where each agent's session id comes from. With
// no session variable from the harness, sfx mcp picks its own: the server
// assigns one only to a client that sends none.
func TestSetupSessionID(t *testing.T) {
	_, sub := repoWithConfig(t)
	const picks = " gives sfx mcp none; sfx mcp picks its own per process (m-…) unless STARFIX_SESSION is set\n"
	for _, tc := range []struct{ agent, want string }{
		{"claude-code", "# session id: read from CLAUDE_CODE_SESSION_ID\n"},
		{"codex", "# session id: Codex" + picks},
		{"gemini", "# session id: Gemini CLI" + picks},
		{"cursor", "# session id: Cursor" + picks},
		{"vscode", "# session id: VS Code" + picks},
		{"junie", "# session id: Junie" + picks},
		{"jetbrains", "# session id: JetBrains AI Assistant" + picks},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			code, out, errb := runIn(t, t.TempDir(), "-C", sub, "setup", tc.agent)
			if code != ExitOK || !strings.Contains(out, tc.want) {
				t.Errorf("sfx setup %s: exit %d, output lacks %q:\n%s%s", tc.agent, code, tc.want, out, errb)
			}
		})
	}
}
