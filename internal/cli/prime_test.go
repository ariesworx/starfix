package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// primeHook runs `sfx prime --hook` with stdin as the hook's input.
func primeHook(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), append(args, "prime", "--hook"), Env{Stdin: strings.NewReader(stdin),
		Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }, Version: "v0.3.0"})
	return code, out.String(), errb.String()
}

func TestPrimeHookNeverFails(t *testing.T) {
	// No SSH key: Getenv has no SSH_AUTH_SOCK and the config no key.
	unreachable := t.TempDir()
	if err := os.WriteFile(filepath.Join(unreachable, ".starfix.yaml"), []byte(testConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, ".starfix.yaml"), []byte("colour: blue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hookJSON := func(cwd string) string {
		b, err := json.Marshal(map[string]string{"session_id": "cc-1", "cwd": cwd, "hook_event_name": "SessionStart", "source": "startup"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tests := []struct {
		name, stdin string
		args        []string
		// note is in the context; "" means no output at all.
		note string
	}{
		{name: "not a starfix repository", args: []string{"-C", t.TempDir()}},
		{name: "cwd from the hook input, not a repository", stdin: hookJSON(t.TempDir())},
		{name: "cannot connect", args: []string{"-C", unreachable}, stdin: hookJSON(""), note: "starfix: prime failed: "},
		{name: "cwd from the hook input", stdin: hookJSON(unreachable), note: "starfix: prime failed: "},
		{name: "input not JSON", args: []string{"-C", unreachable}, stdin: "hello", note: "starfix: prime failed: "},
		{name: "bad config", args: []string{"-C", broken}, note: "starfix: prime failed: "},
		{name: "--json changes nothing", args: []string{"--json", "-C", broken}, note: "fix: correct .starfix.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := primeHook(t, tc.stdin, tc.args...)
			if code != ExitOK || errb != "" {
				t.Fatalf("exit %d, stderr %q", code, errb)
			}
			if tc.note == "" {
				if out != "" {
					t.Fatalf("output outside a repository: %q", out)
				}
				return
			}
			var doc struct {
				Out struct {
					Event   string `json:"hookEventName"`
					Context string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &doc); err != nil || strings.Count(out, "\n") != 1 {
				t.Fatalf("not one JSON document: %q %v", out, err)
			}
			if doc.Out.Event != "SessionStart" || !strings.Contains(doc.Out.Context, tc.note) ||
				!strings.Contains(doc.Out.Context, "fix: ") || strings.Count(doc.Out.Context, "\n") != 1 {
				t.Fatalf("context %q, want one line with %q and a fix", doc.Out.Context, tc.note)
			}
		})
	}
}

func TestPrimeRefusesArguments(t *testing.T) {
	if code, _, errb := runCLI(t, "prime", "--hook", "extra"); code != ExitUsage || !strings.Contains(errb, "usage: sfx prime [--hook]") {
		t.Fatalf("exit %d %s", code, errb)
	}
}
