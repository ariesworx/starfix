package cli

import (
	"bytes"
	"context"
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
	code := Run(context.Background(), args, Env{Stdout: &out, Stderr: &errb,
		Getenv: func(string) string { return "" }, Version: "v0.3.0",
		UserHomeDir: func() (string, error) { return home, nil }})
	return code, out.String(), errb.String()
}

// snapshot reads every file under dir.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
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
	// files maps each file setup writes to a string it must hold.
	for _, agent := range []struct {
		name  string
		files map[string]string
	}{
		{"claude-code", map[string]string{".mcp.json": `"type": "stdio"`, "CLAUDE.md": "<!-- starfix:begin -->",
			".claude/settings.json": `"command": "sfx prime --hook"`}},
		{"codex", map[string]string{".codex/config.toml": "[mcp_servers.starfix]", "AGENTS.md": "<!-- starfix:end -->"}},
		{"gemini", map[string]string{".gemini/settings.json": `"mcpServers"`, "GEMINI.md": "`prime`"}},
		{"cursor", map[string]string{".cursor/mcp.json": `"mcpServers"`, ".cursor/rules/starfix.mdc": "alwaysApply: true"}},
		{"vscode", map[string]string{".vscode/mcp.json": `"servers"`, ".github/copilot-instructions.md": "`finish`"}},
	} {
		t.Run(agent.name, func(t *testing.T) {
			root, sub := repoWithConfig(t)
			home := t.TempDir()

			// Without --write nothing is written; the output shows every file.
			code, out, errb := runIn(t, home, "-C", sub, "setup", agent.name)
			if code != ExitOK || !strings.Contains(out, "--write") {
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
			if code != ExitOK || strings.Count(out, ": added\n") != len(agent.files) {
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
			if code != ExitOK || strings.Count(out, ": unchanged\n") != len(agent.files) {
				t.Fatalf("second write: exit %d %s", code, out)
			}
			sameFiles(t, first, snapshot(t, root))
			if code, out, errb := runIn(t, home, "-C", sub, "setup", agent.name, "--check"); code != ExitOK ||
				strings.Count(out, ": registered\n") != len(agent.files) {
				t.Fatalf("check: %d %s %s", code, out, errb)
			}
			if len(snapshot(t, home)) != 0 {
				t.Fatal("wrote to the home directory without --global")
			}
			if code, out, _ := runIn(t, home, "-C", sub, "setup", agent.name, "--remove"); code != ExitOK ||
				strings.Count(out, ": removed\n") != len(agent.files) {
				t.Fatalf("remove: %d %s", code, out)
			}
			// A file that held only the pointer is gone; the configs stay.
			for f := range agent.files {
				_, err := os.Stat(filepath.Join(root, filepath.FromSlash(f)))
				if gone := errors.Is(err, fs.ErrNotExist); gone != (strings.HasSuffix(f, ".md") || strings.HasSuffix(f, ".mdc")) {
					t.Errorf("%s after remove: %v", f, err)
				}
			}
			removed := snapshot(t, root)
			if code, out, _ := runIn(t, home, "-C", sub, "setup", agent.name, "--remove"); code != ExitOK ||
				strings.Count(out, ": unchanged\n") != len(agent.files) {
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
		{[]string{"setup"}, ExitUsage, "setup needs one agent: claude-code, codex, cursor, gemini, vscode"},
		{[]string{"setup", "aider"}, ExitUsage, `unknown agent "aider"`},
		{[]string{"setup", "vscode", "--global", "--write"}, ExitFailure, "fix: drop --global"},
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
