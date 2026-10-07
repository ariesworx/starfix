package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// agent is an MCP client session against `sfx mcp` run as this user,
// over in-memory transport, with the real SSH client underneath.
type agent struct {
	t       *testing.T
	cs      *mcp.ClientSession
	srv     *mcpserver.Server
	dials   int
	version string
}

func (u *user) mcp(version string) *agent {
	t := u.w.t
	t.Helper()
	a := &agent{t: t, version: version}
	opts := client.Options{Version: version, Session: client.SessionFromEnv(func(k string) string { return u.env[k] }),
		Machine: "laptop-test", Getenv: func(k string) string { return u.env[k] },
		OnPush: func(p proto.Push) { a.srv.Push(p) }}
	var renew time.Duration
	if u.w.fixedClock {
		renew = -1 // renewals would not move the test's clock
	}
	a.srv = mcpserver.New(mcpserver.Options{Version: version, RenewEvery: renew, Dial: func(ctx context.Context) (mcpserver.Conn, error) {
		a.dials++
		return mcpserver.DialRepo(ctx, u.repo, opts)
	}})
	t.Cleanup(func() { _ = a.srv.Close() })
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := a.srv.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	a.cs = cs
	return a
}

// call runs a tool and returns its text and whether it was an error.
func (a *agent) call(name string, args map[string]any) (string, bool) {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		a.t.Fatalf("%s: %v", name, err)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		a.t.Fatalf("%s: content %T", name, res.Content[0])
	}
	return tc.Text, res.IsError
}

func (a *agent) ok(name string, args map[string]any) string {
	a.t.Helper()
	out, isErr := a.call(name, args)
	if isErr {
		a.t.Fatalf("%s %v: %s", name, args, out)
	}
	return out
}

func TestMCPAgentSession(t *testing.T) {
	w := newWorld(t, daemonOpts{latest: "v0.3.0"})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-e2e"
	ag := alice.mcp("v0.2.0")

	tools, err := ag.cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) != 20 {
		t.Fatalf("tools: %v %d", err, len(tools.Tools))
	}
	if ag.dials != 0 {
		t.Fatal("dialed before the first tool call")
	}

	p := decode[mcpserver.Prime](t, ag.ok("prime", nil))
	if p.You != "alice" || p.Session != "cc-e2e" || len(p.Working) != 0 || len(p.Ready) != 0 ||
		len(p.Notices) != 1 || !strings.Contains(p.Notices[0], "v0.3.0 is out") {
		t.Fatalf("prime: %+v", p)
	}

	a := decode[proto.WriteResult](t, ag.ok("create", map[string]any{"title": "Ship it", "priority": 1, "labels": []any{"area:api"}}))
	b := decode[proto.WriteResult](t, ag.ok("create", map[string]any{"title": "Write the docs", "type": "chore", "body": "how"}))
	if a.Rev != 1 || !strings.HasPrefix(a.ID, "sf-") || b.ID == a.ID {
		t.Fatalf("create: %+v %+v", a, b)
	}
	if out := ag.ok("dep", map[string]any{"action": "add", "id": a.ID, "depends_on": b.ID}); out != `{"id":"`+a.ID+`"}` {
		t.Fatalf("dep: %s", out)
	}
	ready := decode[mcpserver.Issues](t, ag.ok("ready", nil))
	if len(ready.Issues) != 1 || ready.Issues[0].ID != b.ID {
		t.Fatalf("ready: %+v", ready)
	}
	blocked := decode[mcpserver.Blocked](t, ag.ok("blocked", nil))
	if len(blocked.Issues) != 1 || blocked.Issues[0].ID != a.ID || blocked.Issues[0].BlockedBy[0] != b.ID {
		t.Fatalf("blocked: %+v", blocked)
	}

	// update cannot take b (only a claim holds an issue; its schema leaves
	// in_progress out); start does.
	out, isErr := ag.call("update", map[string]any{"id": b.ID, "status": "in_progress", "assignee": "alice"})
	if !isErr || !strings.Contains(out, "in_progress does not equal any of") {
		t.Fatalf("update status in_progress: %v %q", isErr, out)
	}
	if st := decode[mcpserver.Started](t, ag.ok("start", map[string]any{"id": b.ID})); st.ID != b.ID || st.Rev != 2 {
		t.Fatalf("start: %+v", st)
	}
	p = decode[mcpserver.Prime](t, ag.ok("prime", nil))
	if len(p.Working) != 1 || p.Working[0].ID != b.ID || p.Working[0].Status != "in_progress" {
		t.Fatalf("prime working: %+v", p.Working)
	}

	if out := alice.ok("prime"); !strings.Contains(out, "in progress:\n  "+b.ID+" P2 \"Write the docs\"\n") {
		t.Fatalf("sfx prime:\n%s", out)
	}

	// As a SessionStart hook: the hook input's session id is used, and
	// the text arrives as additionalContext.
	var hookOut, errb bytes.Buffer
	if code := cli.Run(context.Background(), []string{"prime", "--hook"}, cli.Env{
		Stdin:  strings.NewReader(`{"session_id":"cc-hook","cwd":` + strconv.Quote(alice.repo) + `,"hook_event_name":"SessionStart"}`),
		Stdout: &hookOut, Stderr: &errb, Getenv: func(k string) string { return alice.env[k] },
		Hostname: func() (string, error) { return "laptop-test", nil }, Version: "v0.2.0",
	}); code != 0 || errb.Len() != 0 {
		t.Fatalf("prime --hook: exit %d %s", code, errb.String())
	}
	hook := decode[struct {
		Out struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}](t, hookOut.String())
	if !strings.Contains(hook.Out.Context, "session cc-hook\n") || !strings.Contains(hook.Out.Context, b.ID+" P2 \"Write the docs\"") {
		t.Fatalf("prime --hook:\n%s", hookOut.String())
	}

	show := decode[mcpserver.Issue](t, ag.ok("show", map[string]any{"id": a.ID}))
	if show.Rev != 1 || show.Priority != 1 || len(show.DependsOn) != 1 || show.DependsOn[0] != b.ID+" blocks" ||
		len(show.Labels) != 1 {
		t.Fatalf("show: %+v", show)
	}
	ag.ok("label", map[string]any{"action": "add", "id": a.ID, "labels": []any{"docs", "small"}})
	ag.ok("label", map[string]any{"action": "rm", "id": a.ID, "labels": []any{"small"}})
	listed := decode[mcpserver.Issues](t, ag.ok("list", map[string]any{"labels": []any{"docs"}}))
	if len(listed.Issues) != 1 || listed.Issues[0].ID != a.ID {
		t.Fatalf("list: %+v", listed)
	}

	// Bob edits a; alice's stale rev is refused, with the next step.
	bob.ok("update", a.ID, "--rev", "1", "-p", "0")
	out, isErr = ag.call("update", map[string]any{"id": a.ID, "rev": 1, "title": "mine"})
	if !isErr || out != "conflict: "+a.ID+" changed since rev 1 (now rev 2 by bob)\nfix: call show for the current rev, then retry with that rev if your change still applies" {
		t.Fatalf("conflict: %v %q", isErr, out)
	}
	out, isErr = ag.call("show", map[string]any{"id": "sf-zzzzzzzz"})
	if !isErr || out != "not_found: issue sf-zzzzzzzz not found\nfix: find the id with list or ready" {
		t.Fatalf("not found: %v %q", isErr, out)
	}

	// The connection drops; the next read reconnects and succeeds.
	w.dropAll()
	if show := decode[mcpserver.Issue](t, ag.ok("show", map[string]any{"id": b.ID})); show.Body != "how" {
		t.Fatalf("show after reconnect: %+v", show)
	}
	if c := decode[mcpserver.Ref](t, ag.ok("comment", map[string]any{"id": b.ID, "body": "first"})); c.ID == "" {
		t.Fatal("comment has no id")
	}
	ag.ok("comment", map[string]any{"id": b.ID, "body": "second"})
	cs := decode[mcpserver.Comments](t, ag.ok("comments", map[string]any{"id": b.ID, "limit": 1}))
	if len(cs.Comments) != 1 || cs.Comments[0].Body != "second" || cs.Comments[0].Author != "alice" || cs.Omitted < 1 {
		t.Fatalf("comments: %+v", cs)
	}

	if out := ag.ok("close", map[string]any{"id": b.ID, "reason": "done"}); !strings.HasPrefix(out, `{"id":"`+b.ID+`","rev":`) {
		t.Fatalf("close: %s", out)
	}
	out, isErr = ag.call("close", map[string]any{"id": b.ID})
	if !isErr || !strings.Contains(out, "is already closed\nfix: nothing to do") {
		t.Fatalf("close twice: %q", out)
	}
	ready = decode[mcpserver.Issues](t, ag.ok("ready", nil))
	if len(ready.Issues) != 1 || ready.Issues[0].ID != a.ID {
		t.Fatalf("ready after close: %+v", ready)
	}
	ag.ok("reopen", map[string]any{"id": b.ID})

	h := decode[mcpserver.History](t, ag.ok("history", map[string]any{"id": b.ID, "limit": 50}))
	var ops []string
	for _, e := range h.Events {
		ops = append(ops, e.Op)
		if e.By != "alice" {
			t.Fatalf("event by %s", e.By)
		}
	}
	if got := strings.Join(ops, " "); got != "issue.create claim.take issue.update comment.add comment.add issue.close issue.reopen" {
		t.Fatalf("history: %s", got)
	}
	// The session id reached the server on every connection.
	full := decode[proto.HistoryResult](t, alice.ok("history", b.ID, "--json"))
	for _, e := range full.Events {
		if e.Session != "cc-e2e" || e.Machine != "laptop-test" {
			t.Fatalf("event actor: %+v", e)
		}
	}
	if ag.dials != 2 {
		t.Fatalf("dials = %d, want 2 (one reconnect)", ag.dials)
	}
}

// Without a session variable the server assigns one, and the agent's
// failures to connect come back as tool errors for the person.
func TestMCPServerAssignedSessionAndRefusal(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	ag := w.newUser("carol", "").mcp("v0.2.0")
	p := decode[mcpserver.Prime](t, ag.ok("prime", nil))
	if !strings.HasPrefix(p.Session, "s-") || p.You != "carol" || len(p.Notices) != 0 {
		t.Fatalf("prime: %+v", p)
	}

	stranger := w.newUser("", "").mcp("v0.2.0")
	out, isErr := stranger.call("ready", nil)
	if !isErr || !strings.HasPrefix(out, "auth: ") || !strings.Contains(out, "fix: tell the user starfix cannot connect, quoting the server: \"send your public key") {
		t.Fatalf("unknown key: %q", out)
	}
	var doc struct {
		Error struct{ Code, Fix string }
	}
	res, err := stranger.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res.StructuredContent)
	if json.Unmarshal(b, &doc) != nil || doc.Error.Code != "auth" {
		t.Fatalf("structured error: %s", b)
	}
}

// `sfx mcp` itself, over pipes as a harness runs it.
func TestMCPCommand(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan int, 1)
	go func() {
		var errb bytes.Buffer
		done <- cli.Run(context.Background(), []string{"-C", alice.repo, "mcp"}, cli.Env{
			Stdin: inR, Stdout: outW, Stderr: &errb,
			Getenv:   func(k string) string { return alice.env[k] },
			Hostname: func() (string, error) { return "laptop-test", nil },
			Version:  "v0.2.0",
		})
		_ = outW.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "harness"}, nil).Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := cs.InitializeResult().Instructions; got != mcpserver.Instructions {
		t.Fatalf("instructions: %q", got)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "create", Arguments: map[string]any{"title": "over pipes"}})
	if err != nil || res.IsError {
		t.Fatalf("create: %v %+v", err, res)
	}
	_ = cs.Close()
	_ = inW.Close()
	select {
	case code := <-done:
		if code != cli.ExitOK {
			t.Fatalf("sfx mcp exited %d", code)
		}
	case <-ctx.Done():
		t.Fatal("sfx mcp did not exit when its input closed")
	}
	if out := alice.ok("list"); !strings.Contains(out, "over pipes") {
		t.Fatalf("list:\n%s", out)
	}
}
