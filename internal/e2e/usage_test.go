package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// usageCall sends a usage request on a raw session and returns its result.
func (r *rawSession) usageCall(id uint64, recs []proto.UsageRecord) proto.UsageResult {
	r.t.Helper()
	args, err := json.Marshal(proto.UsageArgs{Records: recs})
	if err != nil {
		r.t.Fatal(err)
	}
	if err := r.enc.Encode(&proto.Frame{T: proto.FrameReq, ID: id, Op: proto.OpUsage, Args: args}); err != nil {
		r.t.Fatal(err)
	}
	f := r.next()
	if f.T != proto.FrameRes || f.ID != id || f.Err != nil {
		r.t.Fatalf("usage: %+v (err %+v)", f, f.Err)
	}
	var out proto.UsageResult
	if err := json.Unmarshal(f.OK, &out); err != nil {
		r.t.Fatal(err)
	}
	return out
}

// An agent session works an issue under a client's epic; its harness
// reports tokens for the session, and show, digest and MCP attribute them.
func TestTokenUsage(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	alice.env["STARFIX_SESSION"] = "s-agent"
	epic := strings.TrimSpace(alice.ok("create", "Client work", "-t", "epic", "--account", "acme"))
	task := strings.TrimSpace(alice.ok("create", "Do the work", "--parent", epic))
	other := strings.TrimSpace(alice.ok("create", "Internal chore"))
	alice.ok("start", task)

	// What PR 2's hook will send: records for the session sfx claimed in.
	at := time.Now().UTC()
	in, out, cw, cw1h, cr := int64(1200), int64(300), int64(5000), int64(1000), int64(90000)
	recs := []proto.UsageRecord{
		{Harness: "claude-code", RequestID: "msg_01:req_01", Model: "claude-opus-4-1", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &in, Output: &out, CacheWrite: &cw, CacheWrite1h: &cw1h, CacheRead: &cr}},
		{Harness: "claude-code", RequestID: "msg_02:req_02", Model: "claude-opus-4-1", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &in, Output: &out}},
	}
	raw := alice.rawDial("s-agent")
	if got := raw.usageCall(1, recs); got != (proto.UsageResult{Added: 2}) {
		t.Fatalf("usage = %+v, want 2 added", got)
	}
	if got := raw.usageCall(2, recs); got != (proto.UsageResult{Duplicates: 2}) {
		t.Fatalf("usage resent = %+v, want 2 duplicates", got)
	}

	shown := alice.ok("show", task)
	for _, want := range []string{
		"account: acme (from " + epic + ")\n",
		"held 0m\n",
		"tokens claude-opus-4-1: 2.4k in, 600 out, 5k cache write (1k 1h), 90k cache read\n",
	} {
		if !strings.Contains(shown, want) {
			t.Errorf("show lacks %q:\n%s", want, shown)
		}
	}
	js := decode[proto.ShowResult](t, alice.ok("show", task, "--json"))
	if u := js.Usage; u == nil || u.Account != "acme" || u.AccountFrom != epic || len(u.Models) != 1 ||
		*u.Models[0].Input != 2*in || *u.Models[0].CacheRead != cr {
		t.Errorf("show --json usage = %+v", js.Usage)
	}

	if s := alice.ok("show", other); !strings.Contains(s, "account: internal (default)\n") || strings.Contains(s, "held") {
		t.Errorf("show of an issue never held:\n%s", s)
	}
	alice.ok("update", other, "--account", "ops")
	if s := alice.ok("show", other); !strings.Contains(s, "account: ops\n") {
		t.Errorf("show after update --account ops:\n%s", s)
	}
	r := alice.run("v0.2.0", "update", other, "--account", "Acme Corp")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "must be a code name") ||
		!strings.Contains(r.stderr, "fix: correct it and retry; `sfx update -h` lists the options") {
		t.Errorf("update --account \"Acme Corp\": exit %d\n%s", r.code, r.stderr)
	}

	digest := alice.ok("digest")
	if !strings.Contains(digest, "\nheld 0m\ntokens claude-opus-4-1: 2.4k in, 600 out, 5k cache write (1k 1h), 90k cache read\n") {
		t.Errorf("digest lacks the usage lines:\n%s", digest)
	}
	if d := decode[proto.DigestResult](t, alice.ok("digest", "--json")); d.Usage == nil || len(d.Usage.Models) != 1 {
		t.Errorf("digest --json usage = %+v", d.Usage)
	}

	ag := alice.mcp("v0.2.0")
	is := decode[mcpserver.Issue](t, ag.ok("show", map[string]any{"id": task}))
	if is.Account != "acme" || is.Usage != "held 0m; claude-opus-4-1 2.4k in, 600 out, 5k cache write, 90k cache read" {
		t.Errorf("mcp show: account %q usage %q", is.Account, is.Usage)
	}
	d := decode[mcpserver.Digest](t, ag.ok("digest", map[string]any{}))
	if !strings.Contains(d.Usage, "claude-opus-4-1 2.4k in") {
		t.Errorf("mcp digest usage %q", d.Usage)
	}
}

// usageHook runs `sfx usage --hook` as this user with the hook input
// given, keeping its offsets in cache.
func (u *user) usageHook(cache string, input map[string]string) result {
	u.w.t.Helper()
	b, err := json.Marshal(input)
	if err != nil {
		u.w.t.Fatal(err)
	}
	var out, errb bytes.Buffer
	ctx, cancel := context.WithTimeout(u.w.t.Context(), 30*time.Second)
	defer cancel()
	code := cli.Run(ctx, []string{"usage", "--hook"}, cli.Env{
		Stdin: bytes.NewReader(b), Stdout: &out, Stderr: &errb,
		Getenv:       func(k string) string { return u.env[k] },
		Hostname:     func() (string, error) { return "laptop-test", nil },
		UserCacheDir: func() (string, error) { return cache, nil },
		Version:      "v0.2.0",
	})
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

// writeClaudeLines appends lines to a Claude Code transcript.
func writeClaudeLines(t *testing.T, path string, lines ...map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // the test's temp file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
}

// claudeResponse is one API response as Claude Code writes it, at at: a
// line per content block, each with the response's usage, its output
// count growing to the last line's. req is the requestId, which some
// lines lack ("").
func claudeResponse(session, id, req string, at time.Time, outputs []int, usage map[string]any) []map[string]any {
	var lines []map[string]any
	for i, o := range outputs {
		u := maps.Clone(usage)
		u["output_tokens"] = o
		l := map[string]any{"type": "assistant", "sessionId": session, "timestamp": at.UTC().Format("2006-01-02T15:04:05.000Z"),
			"uuid": fmt.Sprintf("%s-%d", id, i), "message": map[string]any{"id": id, "model": "claude-opus-4-5-20251101", "role": "assistant",
				"content": []any{map[string]any{"type": "text", "text": "Fixture reply."}}, "usage": u}}
		if req != "" {
			l["requestId"], l["version"] = req, "2.1.300"
		}
		lines = append(lines, l)
	}
	return lines
}

// toolResult is the user line a tool's result makes, which can fall
// among the lines of the response that called it.
func toolResult(session string) map[string]any {
	return map[string]any{"type": "user", "sessionId": session,
		"message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": "ok"}}}}
}

// A Claude Code hook run reads the session's transcripts, its subagent's
// included, and sends their tokens through the SSH server to the daemon;
// show on the issue the session held then shows them, counted once. A
// response is sent once another has begun, with its last line's output,
// though a tool result splits it: at Stop the last response of each file
// waits; SubagentStop sends the stopped subagent's, and SessionEnd the
// rest. The records go to the session the connection names: the hook's
// session_id, unless STARFIX_SESSION is set, as for prime --hook and sfx
// mcp.
func TestUsageHookCapture(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	tests := []struct {
		name, principal string
		env             map[string]string
	}{
		{name: "hook session id", principal: "alice", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"}},
		{name: "STARFIX_SESSION wins", principal: "bob", env: map[string]string{"STARFIX_SESSION": "s-agent"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const sess = "7c8d9e0f-1a2b-4c3d-8e4f-5a6b7c8d9e0f"
			u := w.newUser(tc.principal, "")
			u.env = tc.env
			task := strings.TrimSpace(u.ok("create", "Do the work"))
			u.ok("start", task)

			dir := t.TempDir()
			main := filepath.Join(dir, sess+".jsonl")
			sub := filepath.Join(dir, sess, "subagents", "agent-a1.jsonl")
			now := time.Now()
			a := claudeResponse(sess, "msg_01Main", "req_01Main", now, []int{5, 100, 280},
				map[string]any{"input_tokens": 1200, "cache_creation_input_tokens": 5000, "cache_read_input_tokens": 90000,
					"cache_creation": map[string]any{"ephemeral_1h_input_tokens": 1000, "ephemeral_5m_input_tokens": 4000}})
			writeClaudeLines(t, main, a[0], a[1], toolResult(sess), a[2], toolResult(sess))
			writeClaudeLines(t, main, claudeResponse(sess, "msg_01Last", "req_01Last", now, []int{50, 300},
				map[string]any{"input_tokens": 1200, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0})...)
			writeClaudeLines(t, sub, claudeResponse(sess, "msg_01Sub", "", now, []int{2, 20},
				map[string]any{"input_tokens": 1200, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
					"cache_creation": map[string]any{"ephemeral_1h_input_tokens": 0, "ephemeral_5m_input_tokens": 0}})...)

			cache := t.TempDir()
			const model = "tokens claude-opus-4-5-20251101: "
			for i, run := range []struct{ event, agent, want string }{
				{"Stop", "", model + "1.2k in, 280 out, 5k cache write (1k 1h), 90k cache read\n"},
				{"SubagentStop", sub, model + "2.4k in, 300 out, 5k cache write (1k 1h), 90k cache read\n"},
				{"SessionEnd", "", model + "3.6k in, 600 out, 5k cache write (1k 1h), 90k cache read\n"},
				{"SessionEnd", "", model + "3.6k in, 600 out, 5k cache write (1k 1h), 90k cache read\n"},
			} {
				in := map[string]string{"session_id": sess, "transcript_path": main, "cwd": filepath.Join(u.repo, "src"), "hook_event_name": run.event}
				if run.agent != "" {
					in["agent_transcript_path"] = run.agent
				}
				r := u.usageHook(cache, in)
				if r.code != 0 || r.stdout != "" || r.stderr != "" {
					t.Fatalf("usage --hook run %d (%s): exit %d, stdout %q, stderr %q; want 0 and nothing", i+1, run.event, r.code, r.stdout, r.stderr)
				}
				if shown := u.ok("show", task); !strings.Contains(shown, run.want) {
					t.Fatalf("show after usage --hook run %d (%s) lacks %q:\n%s", i+1, run.event, run.want, shown)
				}
			}
		})
	}
}

// The server refuses a record whose time is more than an hour past its
// clock, and with it the whole batch. The hook drops that record, sends
// the rest, and says so in one line; the next run sends nothing again.
func TestUsageHookDropsRefusedRecord(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	u := w.newUser("alice", "")
	const sess = "8d9e0f1a-2b3c-4d4e-9f5a-6b7c8d9e0f1a"
	u.env = map[string]string{"CLAUDE_CODE_SESSION_ID": sess}
	task := strings.TrimSpace(u.ok("create", "Do the work"))
	u.ok("start", task)

	main := filepath.Join(t.TempDir(), sess+".jsonl")
	now := time.Now()
	usage := map[string]any{"input_tokens": 1000}
	for _, r := range []struct {
		id string
		at time.Time
	}{{"msg_01One", now}, {"msg_01Ahead", now.Add(2 * time.Hour)}, {"msg_01Two", now}, {"msg_01Open", now}} {
		writeClaudeLines(t, main, claudeResponse(sess, r.id, "", r.at, []int{100}, usage)...)
	}
	in := map[string]string{"session_id": sess, "transcript_path": main, "cwd": filepath.Join(u.repo, "src"), "hook_event_name": "Stop"}
	cache := t.TempDir()
	r := u.usageHook(cache, in)
	if r.code != 0 || r.stdout != "" || !strings.HasPrefix(r.stderr, "starfix: 1 token usage record refused and dropped: ") ||
		!strings.Contains(r.stderr, "; fix: ") || strings.Count(r.stderr, "\n") != 1 {
		t.Fatalf("usage --hook with a record ahead of the server: exit %d, stdout %q, stderr %q; want one note on the dropped record", r.code, r.stdout, r.stderr)
	}
	want := "tokens claude-opus-4-5-20251101: 2k in, 200 out\n"
	if shown := u.ok("show", task); !strings.Contains(shown, want) {
		t.Fatalf("show lacks %q, the records sent around the refused one:\n%s", want, shown)
	}
	if r := u.usageHook(cache, in); r.code != 0 || r.stderr != "" {
		t.Fatalf("second usage --hook: exit %d, stderr %q; want nothing", r.code, r.stderr)
	}
}
