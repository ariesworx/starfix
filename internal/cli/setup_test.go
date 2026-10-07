package cli

import (
	"bytes"
	"context"
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
	for _, agent := range []struct{ name, file, want string }{
		{"claude-code", ".mcp.json", `"type": "stdio"`},
		{"codex", ".codex/config.toml", "[mcp_servers.starfix]"},
		{"gemini", ".gemini/settings.json", `"mcpServers"`},
	} {
		t.Run(agent.name, func(t *testing.T) {
			root, sub := repoWithConfig(t)
			home := t.TempDir()

			// Without --write nothing is written.
			code, out, errb := runIn(t, home, "-C", sub, "setup", agent.name)
			if code != ExitOK || !strings.Contains(out, "--write") || !strings.Contains(out, agent.want) {
				t.Fatalf("print: exit %d\n%s%s", code, out, errb)
			}
			if code, _, errb := runIn(t, home, "-C", sub, "setup", agent.name, "--check"); code != ExitFailure ||
				!strings.Contains(errb, "fix: run `sf setup "+agent.name+" --write`") {
				t.Fatalf("check before write: exit %d %s", code, errb)
			}
			if len(snapshot(t, root)) != 1 {
				t.Fatal("printing wrote a file")
			}

			code, out, errb = runIn(t, home, "-C", sub, "setup", agent.name, "--write")
			if code != ExitOK || !strings.Contains(out, "added") {
				t.Fatalf("write: exit %d\n%s%s", code, out, errb)
			}
			first := snapshot(t, root)
			b := first[filepath.Join(root, filepath.FromSlash(agent.file))]
			if !strings.Contains(b, agent.want) {
				t.Fatalf("%s:\n%s", agent.file, b)
			}
			code, out, _ = runIn(t, home, "-C", sub, "setup", agent.name, "--write")
			if code != ExitOK || !strings.Contains(out, "unchanged") {
				t.Fatalf("second write: exit %d %s", code, out)
			}
			second := snapshot(t, root)
			if len(first) != len(second) {
				t.Fatalf("files changed: %v vs %v", first, second)
			}
			for p, c := range first {
				if second[p] != c {
					t.Fatalf("%s changed on the second run:\n%s\n---\n%s", p, c, second[p])
				}
			}
			if code, _, errb := runIn(t, home, "-C", sub, "setup", agent.name, "--check"); code != ExitOK {
				t.Fatalf("check: %d %s", code, errb)
			}
			if len(snapshot(t, home)) != 0 {
				t.Fatal("wrote to the home directory without --global")
			}
			if code, out, _ := runIn(t, home, "-C", sub, "setup", agent.name, "--remove"); code != ExitOK || !strings.Contains(out, "removed") {
				t.Fatalf("remove: %d %s", code, out)
			}
		})
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
		{[]string{"setup"}, ExitUsage, "setup needs one agent: claude-code, codex, gemini"},
		{[]string{"setup", "cursor"}, ExitUsage, `unknown agent "cursor"`},
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
}
