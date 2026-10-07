package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, Env{Stdout: &out, Stderr: &errb,
		Getenv: func(string) string { return "" }, Version: "v0.3.0"})
	return code, out.String(), errb.String()
}

// These need no server: they fail or finish before connecting.
func TestRunWithoutServer(t *testing.T) {
	empty := t.TempDir()
	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{name: "no command prints help", args: nil, code: ExitUsage, stdout: "usage: starfix"},
		{name: "help", args: []string{"help"}, code: ExitOK, stdout: "ready"},
		{name: "help for a command", args: []string{"help", "dep"}, code: ExitOK, stdout: "usage: starfix dep add|rm FROM TO"},
		{name: "version", args: []string{"version"}, code: ExitOK, stdout: "starfix v0.3.0 (protocol 1)"},
		{name: "version json", args: []string{"--json", "version"}, code: ExitOK, stdout: `{"protocol":"1","version":"v0.3.0"}`},
		{name: "unknown global flag", args: []string{"--colour", "ready"}, code: ExitUsage, stderr: "unknown flag --colour"},
		{name: "unknown command flag", args: []string{"ready", "--colour"}, code: ExitUsage, stderr: "fix: usage: starfix ready [-n N]"},
		{name: "bad priority", args: []string{"-C", empty, "create", "x", "-p", "9"}, code: ExitUsage, stderr: "priority must be 0-4"},
		{name: "update with nothing", args: []string{"-C", empty, "update", "sf-aaaa"}, code: ExitUsage, stderr: "nothing to update"},
		{name: "dep needs an action", args: []string{"dep", "link", "a", "b"}, code: ExitUsage, stderr: `unknown dep action "link"`},
		{name: "no config", args: []string{"-C", empty, "ready"}, code: ExitFailure,
			stderr: "starfix: .starfix.yaml not found here or in any parent directory\nfix: run starfix inside a repository"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, tc.args...)
			if code != tc.code || !strings.Contains(out, tc.stdout) || !strings.Contains(errb, tc.stderr) {
				t.Fatalf("exit %d (want %d)\nstdout %q (want %q)\nstderr %q (want %q)", code, tc.code, out, tc.stdout, errb, tc.stderr)
			}
			if code != ExitOK && !strings.Contains(errb, "fix: ") && tc.stdout == "" {
				t.Fatalf("failure without a fix line: %q", errb)
			}
		})
	}
}

func TestJSONErrorIsOneDocument(t *testing.T) {
	code, out, errb := runCLI(t, "-C", t.TempDir(), "show", "sf-aaaa", "--json")
	if code != ExitFailure || errb != "" {
		t.Fatalf("exit %d stderr %q", code, errb)
	}
	var doc map[string]proto.Error
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("%q: %v", out, err)
	}
	if e := doc["error"]; e.Code != proto.CodeInvalid || e.Fix == "" {
		t.Fatalf("error: %+v", e)
	}
}

func TestParseInterspersed(t *testing.T) {
	r := &runner{}
	fs := r.newFlags("x")
	n := fs.Int("n", 0, "")
	pos, err := parse(fs, []string{"a", "-n", "3", "b", "--json", "--", "-c", "--json"}, "x")
	if err != nil {
		t.Fatal(err)
	}
	if *n != 3 || !r.json || strings.Join(pos, " ") != "a b -c --json" {
		t.Fatalf("n=%d json=%v pos=%q", *n, r.json, pos)
	}
}

func TestPriority(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "4": 4, "P1": 1, "p2": 2} {
		if got, err := priority(in); err != nil || got != want {
			t.Errorf("priority(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "5", "P", "12", "high"} {
		if _, err := priority(in); err == nil {
			t.Errorf("priority(%q) accepted", in)
		}
	}
}
