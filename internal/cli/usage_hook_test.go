package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/capture"
)

// hookSession is the Claude Code session in these tests.
const hookSession = "0b1c2d3e-4f5a-4b6c-8d7e-9f0a1b2c3d4e"

// transcriptLine is an assistant line of hookSession with usage, or in
// a format sfx does not know when unknown is set.
func transcriptLine(t *testing.T, id string, unknown bool) string {
	t.Helper()
	msg := map[string]any{"id": id, "model": "claude-x", "content": []any{map[string]any{"type": "text", "text": "Fixture reply."}},
		"usage": map[string]any{"input_tokens": 3, "output_tokens": 4}}
	if unknown {
		delete(msg, "usage")
		msg["tokens"] = map[string]any{"in": 3}
	}
	b, err := json.Marshal(map[string]any{"type": "assistant", "sessionId": hookSession, "requestId": "req_" + id,
		"timestamp": "2026-10-08T09:00:00.000Z", "version": "9.0.0", "message": msg})
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

// writeTranscript writes lines as hookSession's transcript in a new
// directory and returns its path.
func writeTranscript(t *testing.T, lines string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), hookSession+".jsonl")
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// usageHook runs `sfx usage` with args and stdin as the hook's input,
// keeping its state under cache.
func usageHook(t *testing.T, cache, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(t.Context(), append(args, "usage", "--hook"), Env{Stdin: strings.NewReader(stdin),
		Stdout: &out, Stderr: &errb, Getenv: func(string) string { return "" }, Version: "v0.3.0",
		UserCacheDir: func() (string, error) { return cache, nil }})
	return code, out.String(), errb.String()
}

// The hook always exits 0 and prints nothing on stdout. Outside a starfix
// repository, or with nothing new to send, it says nothing at all; any
// failure is one line on stderr with a fix, and never transcript text.
func TestUsageHookNeverFails(t *testing.T) {
	repo, _ := repoWithConfig(t) // no SSH key: connecting fails
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, ".starfix.yaml"), []byte("colour: blue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	records := writeTranscript(t, transcriptLine(t, "msg_01A", false))
	empty := writeTranscript(t, "")
	unknown := writeTranscript(t, transcriptLine(t, "msg_01U", true))
	input := func(cwd, transcript string) string {
		b, err := json.Marshal(map[string]string{"session_id": hookSession, "transcript_path": transcript, "cwd": cwd,
			"hook_event_name": "Stop"})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	tests := []struct {
		name, stdin string
		args        []string
		// note is in the one stderr line; "" means no output at all.
		note string
	}{
		{name: "not a starfix repository", stdin: input(t.TempDir(), records)},
		{name: "-C not a repository", args: []string{"-C", t.TempDir()}, stdin: input("", records)},
		{name: "nothing new to send", stdin: input(repo, empty)},
		{name: "cannot connect", stdin: input(repo, records), note: "starfix: token usage not sent: "},
		{name: "bad config", stdin: input(broken, records), note: "starfix: token usage not sent: "},
		{name: "unknown format", stdin: input(repo, unknown),
			note: "starfix: token usage not sent: transcript format not recognized (Claude Code 9.0.0)"},
		{name: "no hook input", args: []string{"-C", repo}, note: `starfix: token usage not sent: "no session_id and transcript_path in the hook input"`},
		{name: "input not JSON", args: []string{"-C", repo}, stdin: "hello", note: "no session_id and transcript_path"},
		{name: "--json changes nothing", args: []string{"--json"}, stdin: input(repo, records), note: "starfix: token usage not sent: "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := usageHook(t, t.TempDir(), tc.stdin, tc.args...)
			if code != ExitOK || out != "" {
				t.Fatalf("usage --hook: exit %d, stdout %q; want 0 and nothing", code, out)
			}
			if tc.note == "" {
				if errb != "" {
					t.Fatalf("usage --hook printed %q, want nothing", errb)
				}
				return
			}
			if !strings.Contains(errb, tc.note) || !strings.Contains(errb, "; fix: ") || strings.Count(errb, "\n") != 1 {
				t.Fatalf("usage --hook stderr = %q, want one line with %q and a fix", errb, tc.note)
			}
			if strings.Contains(errb, "Fixture") {
				t.Fatalf("usage --hook stderr shows transcript text: %q", errb)
			}
		})
	}
}

func TestUsageRefusesArguments(t *testing.T) {
	tests := []struct{ args []string }{
		{[]string{"usage"}},
		{[]string{"usage", "--hook", "extra"}},
		{[]string{"usage", "--hook=codex"}}, // not captured yet
		{[]string{"usage", "--hook=aider"}},
	}
	for _, tc := range tests {
		if code, _, errb := runCLI(t, tc.args...); code != ExitUsage || !strings.Contains(errb, "usage: sfx usage --hook[=AGENT]") {
			t.Errorf("sfx %v: exit %d, stderr %q; want %d and the usage line", tc.args, code, errb, ExitUsage)
		}
	}
	if code, _, errb := runCLI(t, "-C", t.TempDir(), "usage", "--hook="+capture.Harness); code != ExitOK || errb != "" {
		t.Errorf("sfx usage --hook=%s outside a repository: exit %d, stderr %q; want 0 and nothing", capture.Harness, code, errb)
	}
}
