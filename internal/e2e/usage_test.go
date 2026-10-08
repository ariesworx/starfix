package e2e

import (
	"encoding/json"
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
