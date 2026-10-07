package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// fakeConn answers calls with reply. A reply returning errLost breaks the
// connection, as a dropped SSH session does. watch, which the server sends
// on every new connection, is answered here and counted apart from calls.
type fakeConn struct {
	reply   func(op string, args any) (any, error)
	mu      sync.Mutex
	calls   []call
	watches int
	lost    error
	who     string
}

type call struct {
	op   string
	args any
}

var errLost = proto.Errf(proto.CodeUnavailable, "retry; if it persists, check the server", "the server closed the connection")

func (f *fakeConn) Call(_ context.Context, op string, args, result any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lost != nil {
		return f.lost
	}
	if op == proto.OpWatch {
		f.watches++
		return nil
	}
	f.calls = append(f.calls, call{op, args})
	res, err := f.reply(op, args)
	if errors.Is(err, errLost) {
		f.lost = err
	}
	if err != nil {
		return err
	}
	if result == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, result)
}

func (f *fakeConn) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lost
}
func (f *fakeConn) Close() error      { return nil }
func (f *fakeConn) Principal() string { return f.who }
func (f *fakeConn) Session() string   { return "s-test" }
func (f *fakeConn) Project() string   { return "example" }
func (f *fakeConn) Notices(string) []string {
	return []string{"the server runs starfixd v0.1.0; v0.2.0 is out: ask the admin to run `starfixd upgrade`"}
}

func (f *fakeConn) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.op)
	}
	return out
}

// dialer hands out conns in order and counts dials.
type dialer struct {
	mu    sync.Mutex
	conns []*fakeConn
	n     int
}

func (d *dialer) dial(context.Context) (Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n >= len(d.conns) {
		return nil, proto.Errf(proto.CodeAuth, "send your public key to the starfix admin", "127.0.0.1:22 refused your SSH key")
	}
	d.n++
	return d.conns[d.n-1], nil
}

func connect(t *testing.T, conns ...*fakeConn) (*mcp.ClientSession, *dialer) {
	t.Helper()
	cs, d, _ := connectServer(t, conns...)
	return cs, d
}

func connectServer(t *testing.T, conns ...*fakeConn) (*mcp.ClientSession, *dialer, *Server) {
	t.Helper()
	d := &dialer{conns: conns}
	s := New(Options{Version: "v0.2.0", Dial: d.dial})
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, d, s
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("no content")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return tc.Text
}

func summaries(n, titleLen int) []proto.Summary {
	out := make([]proto.Summary, n)
	for i := range out {
		out[i] = proto.Summary{ID: fmt.Sprintf("sf-%08d", i), Title: strings.Repeat("t", titleLen), Status: "open", Priority: 2}
	}
	return out
}

func okReply(string, any) (any, error) { return proto.Empty{}, nil }

func TestTools(t *testing.T) {
	cs, _ := connect(t)
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ro := []string{"blocked", "comments", "digest", "history", "list", "prime", "ready", "show", "who"}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if tool.Annotations == nil {
			t.Errorf("%s has no annotations", tool.Name)
			continue
		}
		if want := slices.Contains(ro, tool.Name); tool.Annotations.ReadOnlyHint != want {
			t.Errorf("%s: readOnlyHint = %v", tool.Name, tool.Annotations.ReadOnlyHint)
		}
		if !tool.Annotations.ReadOnlyHint && (tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint) {
			t.Errorf("%s may be destructive", tool.Name)
		}
		if tool.OutputSchema != nil {
			t.Errorf("%s publishes an output schema", tool.Name)
		}
	}
	slices.Sort(names)
	want := []string{"blocked", "close", "comment", "comments", "create", "dep", "digest", "finish", "handoff", "history", "inbox",
		"label", "list", "prime", "ready", "reopen", "show", "start", "update", "who"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v\nwant    %v", names, want)
	}
	// Design §5 aims for about 2k tokens for the whole verb set. These 20
	// tools are about 7 KiB, some 1.8k real tokens; the budget below is
	// in Tokens' deliberately high estimate, raised from 2,200 to 2,400
	// for the inbox and the structured handoff; finish's ticked and waived
	// fit by trimming descriptions. Counted is what a model reads:
	// names, descriptions and input schemas; annotations steer the
	// harness's approval prompts.
	type seen struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		InputSchema any    `json:"inputSchema"`
	}
	var visible []seen
	for _, tool := range res.Tools {
		visible = append(visible, seen{tool.Name, tool.Description, tool.InputSchema})
		if strings.Contains(fmt.Sprint(tool.InputSchema), "null") {
			t.Errorf("%s: nullable field in %v", tool.Name, tool.InputSchema)
		}
	}
	b, err := json.Marshal(visible)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tool schemas cost ~%d tokens (%d bytes)", Tokens(b), len(b))
	if got, budget := Tokens(b), 2400; got > budget {
		t.Errorf("tool schemas cost ~%d tokens (%d bytes), budget %d", got, len(b), budget)
	}
}

// The schema enums are the store's own values.
func TestEnumsMatchStore(t *testing.T) {
	for _, v := range issueTypes {
		if !store.IssueType(v.(string)).Valid() {
			t.Errorf("type %v", v)
		}
	}
	for _, v := range allStatuses {
		if !store.Status(v.(string)).Valid() {
			t.Errorf("status %v", v)
		}
	}
	for _, v := range depTypes {
		if !store.DepType(v.(string)).Valid() {
			t.Errorf("dep type %v", v)
		}
	}
}

// Bad input is refused before anything reaches the server.
func TestSchemaRejects(t *testing.T) {
	f := &fakeConn{reply: okReply}
	cs, d := connect(t, f)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"create", map[string]any{"title": "x", "priority": 7}},
		{"create", map[string]any{"title": "x", "type": "story"}},
		{"create", map[string]any{}},
		{"update", map[string]any{"id": "sf-1", "status": "closed"}},
		{"ready", map[string]any{"limit": 0}},
		{"list", map[string]any{"status": []any{"done"}}},
		{"dep", map[string]any{"action": "add", "id": "sf-1", "depends_on": "sf-2", "type": "maybe"}},
		{"label", map[string]any{"action": "toggle", "id": "sf-1", "labels": []any{"x"}}},
		{"show", map[string]any{}},
		{"show", map[string]any{"id": "sf-1", "extra": true}},
		{"finish", map[string]any{"id": "sf-1", "discovered": []any{map[string]any{"title": "x", "type": "story"}}}},
		{"finish", map[string]any{"id": "sf-1", "discovered": []any{map[string]any{"title": "x", "priority": 9}}}},
		{"finish", map[string]any{"id": "sf-1", "discovered": []any{map[string]any{"type": "bug"}}}},
		{"handoff", map[string]any{"id": "sf-1"}},
		{"handoff", map[string]any{"id": "sf-1", "note": "n", "state": "finished"}},
		{"finish", map[string]any{"id": "sf-1", "state": "maybe"}},
		{"inbox", map[string]any{"ack": []any{"one"}}},
		{"digest", map[string]any{"since": "7d", "extra": true}},
	} {
		if res := callTool(t, cs, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s %v was accepted", tc.tool, tc.args)
		}
	}
	if d.n != 0 {
		t.Errorf("dialed %d times for rejected calls", d.n)
	}
}

func TestWritesAreCompact(t *testing.T) {
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		switch op {
		case proto.OpShow:
			return proto.ShowResult{Issue: proto.Issue{ID: "sf-1", Rev: 4}}, nil
		case proto.OpComment:
			return proto.CommentResult{ID: "c-1"}, nil
		case proto.OpFinish:
			return proto.FinishResult{ID: "sf-1", Rev: 5, Created: []string{"sf-2"}}, nil
		}
		return proto.WriteResult{ID: "sf-1", Rev: 5}, nil
	}}
	cs, _ := connect(t, f)
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"create", map[string]any{"title": "x", "priority": 0}, `{"id":"sf-1","rev":5}`},
		{"update", map[string]any{"id": "sf-1", "title": "y"}, `{"id":"sf-1","rev":5}`},
		{"close", map[string]any{"id": "sf-1", "reason": "done"}, `{"id":"sf-1","rev":5}`},
		{"dep", map[string]any{"action": "add", "id": "sf-1", "depends_on": "sf-2"}, `{"id":"sf-1"}`},
		{"label", map[string]any{"action": "rm", "id": "sf-1", "labels": []any{"a", "b"}}, `{"id":"sf-1"}`},
		{"comment", map[string]any{"id": "sf-1", "body": "hi"}, `{"id":"c-1"}`},
		{"handoff", map[string]any{"id": "sf-1", "note": "next: tests", "release": true, "state": "partial", "next": "tests",
			"branch": "fix/sf-1-x", "to": "bob"}, `{"id":"sf-1","rev":5}`},
		{"finish", map[string]any{"id": "sf-1", "discovered": []any{map[string]any{"title": "more", "type": "bug", "priority": 1}},
			"handoff": "shipped", "state": "done", "to": "bob"}, `{"id":"sf-1","rev":5,"created":["sf-2"]}`},
	} {
		res := callTool(t, cs, tc.tool, tc.args)
		if res.IsError || text(t, res) != tc.want {
			t.Errorf("%s: %s (error %v), want %s", tc.tool, text(t, res), res.IsError, tc.want)
		}
	}
	// update without rev reads it first; label sends one call per label.
	want := []string{"create", "show", "update", "close", "dep.add", "label.rm", "label.rm", "comment", "handoff", "finish"}
	if got := f.ops(); !slices.Equal(got, want) {
		t.Errorf("ops %v, want %v", got, want)
	}
	if got, ok := f.calls[8].args.(proto.HandoffArgs); !ok || got.Note != "next: tests" || !got.Release ||
		got.HandoffFields != (proto.HandoffFields{State: "partial", Next: "tests", Branch: "fix/sf-1-x", To: "bob"}) {
		t.Errorf("handoff args %#v", f.calls[8].args)
	}
	got, ok := f.calls[9].args.(proto.FinishArgs)
	if !ok || len(got.Discovered) != 1 || got.Discovered[0].Type != "bug" || *got.Discovered[0].Priority != 1 ||
		got.Handoff != "shipped" || got.HandoffFields != (proto.HandoffFields{State: "done", To: "bob"}) {
		t.Errorf("finish args %#v", f.calls[9].args)
	}
	if got := f.calls[2].args.(proto.UpdateArgs); got.Rev != 4 || *got.Title != "y" || got.Status != nil {
		t.Errorf("update args %+v", got)
	}
	if got := f.calls[0].args.(proto.CreateArgs); !strings.HasPrefix(got.Idem, "mcp-") || *got.Priority != 0 {
		t.Errorf("create args %+v", got)
	}
}

func TestErrorsNameTheNextStep(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"not found", proto.Errf(proto.CodeNotFound, "find the id with `sfx list`", "issue sf-x not found"),
			"not_found: issue sf-x not found\nfix: find the id with list or ready"},
		{"conflict", proto.Errf(proto.CodeConflict, "re-read with `sfx show sf-x`", "sf-x changed since rev 1 (now rev 2 by bob)"),
			"conflict: sf-x changed since rev 1 (now rev 2 by bob)\nfix: call show for the current rev, then retry with that rev if your change still applies"},
		{"closed", proto.Errf(proto.CodeInvalid, "reopen it with `sfx reopen sf-x`", "issue sf-x is closed"),
			"invalid: issue sf-x is closed\nfix: call reopen first"},
		{"cycle", proto.Errf(proto.CodeCycle, "remove an edge with `sfx dep rm`", "sf-a depends on sf-b, so this would make a cycle"),
			"cycle: sf-a depends on sf-b, so this would make a cycle\nfix: remove an edge with dep (action rm), or choose another parent"},
		{"invalid", proto.Errf(proto.CodeInvalid, "correct it and retry; `sfx update -h` lists the options", "title is empty"),
			"invalid: title is empty\nfix: correct the arguments and retry"},
		{"held at start", proto.Errf(proto.CodeConflict, "take that one with `sfx start sf-y`", "sf-x is in progress by bob; next ready: sf-y"),
			"conflict: sf-x is in progress by bob; next ready: sf-y\nfix: call start with the next ready id named above"},
		{"held at finish", proto.Errf(proto.CodeConflict, "leave it to bob, or close it with `sfx close sf-x`", "sf-x is in progress by bob"),
			"conflict: sf-x is in progress by bob\nfix: someone else holds this issue: pick other work with start, or tell the user if it must move"},
		{"nothing ready", proto.Errf(proto.CodeNotFound, "see what holds work back with `sfx blocked`, or create an issue", "nothing is ready to start"),
			"not_found: nothing is ready to start\nfix: nothing is ready: call blocked to see why, or create an issue"},
		{"dial", &dialError{err: proto.Errf(proto.CodeAuth, "send your public key to the starfix admin", "refused your SSH key")},
			"auth: refused your SSH key\nfix: tell the user starfix cannot connect: send your public key to the starfix admin"},
		{"lost write", &lostError{err: errLost},
			"unavailable: the server closed the connection\nfix: the connection dropped and the next call reconnects; this write may have applied, so check with show before repeating it"},
		{"lost read", &lostError{err: errLost, retried: true},
			"unavailable: the server closed the connection\nfix: starfix reconnected and the retry failed too; try again later, and if it persists tell the user: retry; if it persists, check the server"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := toolError(tc.err)
			if got := text(t, res); !res.IsError || got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
			var doc errorDoc
			b, _ := json.Marshal(res.StructuredContent)
			if err := json.Unmarshal(b, &doc); err != nil || doc.Error.Code == "" || doc.Error.Fix == "" {
				t.Fatalf("structured: %s (%v)", b, err)
			}
		})
	}
}

// A dropped connection is redialed. Reads and creates are retried on the
// new connection; other writes are not, and say so.
func TestReconnect(t *testing.T) {
	breaksOnce := func() *fakeConn {
		return &fakeConn{reply: func(string, any) (any, error) { return nil, errLost }}
	}
	var idems []string
	healthy := &fakeConn{reply: func(_ string, args any) (any, error) {
		if a, ok := args.(proto.CreateArgs); ok {
			idems = append(idems, a.Idem)
		}
		return proto.WriteResult{ID: "sf-1", Rev: 1}, nil
	}}
	first, second := breaksOnce(), breaksOnce()
	cs, d := connect(t, first, healthy, second, healthy)

	if res := callTool(t, cs, "ready", nil); res.IsError {
		t.Fatalf("ready after a drop: %s", text(t, res))
	}
	if d.n != 2 {
		t.Fatalf("dials = %d, want 2", d.n)
	}
	// Break the healthy conn's successor: the next dial gets second.
	healthy.mu.Lock()
	healthy.lost = errLost
	healthy.mu.Unlock()
	res := callTool(t, cs, "close", map[string]any{"id": "sf-1"})
	if !res.IsError || !strings.Contains(text(t, res), "may have applied") {
		t.Fatalf("close on a dropped conn: %s", text(t, res))
	}
	healthy.mu.Lock()
	healthy.lost = nil
	healthy.mu.Unlock()
	if res := callTool(t, cs, "close", map[string]any{"id": "sf-1"}); res.IsError {
		t.Fatalf("close after redial: %s", text(t, res))
	}
	if d.n != 4 {
		t.Fatalf("dials = %d, want 4", d.n)
	}
	if res := callTool(t, cs, "create", map[string]any{"title": "x"}); res.IsError || len(idems) != 1 {
		t.Fatalf("create: %s, idems %v", text(t, res), idems)
	}
	// No conn left: the dial refusal is a tool error for the person.
	healthy.mu.Lock()
	healthy.lost = errLost
	healthy.mu.Unlock()
	res = callTool(t, cs, "show", map[string]any{"id": "sf-1"})
	if !res.IsError || !strings.Contains(text(t, res), "fix: tell the user starfix cannot connect: send your public key") {
		t.Fatalf("show with no server: %s", text(t, res))
	}
}

// A create retried after a drop carries the same idempotency key.
func TestCreateRetryKeepsKey(t *testing.T) {
	var idems []string
	record := func(fail bool) *fakeConn {
		return &fakeConn{reply: func(_ string, args any) (any, error) {
			idems = append(idems, args.(proto.CreateArgs).Idem)
			if fail {
				return nil, errLost
			}
			return proto.WriteResult{ID: "sf-1", Rev: 1}, nil
		}}
	}
	cs, _ := connect(t, record(true), record(false))
	if res := callTool(t, cs, "create", map[string]any{"title": "x"}); res.IsError {
		t.Fatal(text(t, res))
	}
	if len(idems) != 2 || idems[0] != idems[1] || idems[0] == "" {
		t.Fatalf("idempotency keys %q", idems)
	}
}

func TestResultsAreCapped(t *testing.T) {
	var limits []int
	big := strings.Repeat("word ", 20000)
	f := &fakeConn{reply: func(op string, args any) (any, error) {
		switch op {
		case proto.OpList:
			a := args.(proto.ListArgs)
			limits = append(limits, a.Limit)
			return proto.ListResult{Issues: summaries(a.Limit, 500), Next: "c"}, nil
		case proto.OpReady:
			return proto.ListResult{Issues: summaries(args.(proto.LimitArgs).Limit, 500)}, nil
		case proto.OpShow:
			return proto.ShowResult{Issue: proto.Issue{ID: "sf-1", Title: "t", Body: big, Notes: big}}, nil
		case proto.OpComments:
			var cs []proto.Comment
			for range 50 {
				cs = append(cs, proto.Comment{Author: "alice", Body: big})
			}
			return proto.CommentsResult{Comments: cs}, nil
		}
		return nil, errors.New("unexpected " + op)
	}}
	cs, _ := connect(t, f)
	for _, tc := range []struct {
		tool string
		args map[string]any
		mark string
	}{
		{"list", map[string]any{"limit": 100}, `"next":"c"`},
		{"ready", map[string]any{"limit": 100}, `"more":true`},
		{"show", map[string]any{"id": "sf-1", "full": true}, `"truncated":true`},
		{"comments", map[string]any{"id": "sf-1", "limit": 50}, `"omitted":`},
	} {
		res := callTool(t, cs, tc.tool, tc.args)
		got := text(t, res)
		if res.IsError || Tokens([]byte(got)) > MaxResultTokens || !strings.Contains(got, tc.mark) {
			t.Errorf("%s: %d tokens (cap %d), error %v, mark %s: %.200s", tc.tool, Tokens([]byte(got)), MaxResultTokens, res.IsError, tc.mark, got)
		}
	}
	// list asked again for as many as fit, so its cursor stays exact.
	if len(limits) != 2 || limits[0] != 100 || limits[1] >= 100 || limits[1] < 1 {
		t.Errorf("list limits %v", limits)
	}
}

func TestPrime(t *testing.T) {
	tests := []struct {
		name     string
		who      string
		n, title int
		wantOps  []string
	}{
		{"typical", "alice", 3, 60, []string{"list", "ready", "inbox"}},
		{"worst case", "alice", 5, 500, []string{"list", "ready", "inbox"}},
		{"old server, no principal", "", 5, 500, []string{"ready", "inbox"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeConn{who: tc.who, reply: func(op string, args any) (any, error) {
				if op == proto.OpList {
					if a := args.(proto.ListArgs); a.Assignee != "alice" || !slices.Equal(a.Status, []string{"in_progress"}) {
						return nil, fmt.Errorf("list args %+v", a)
					}
				}
				return proto.ListResult{Issues: summaries(tc.n, tc.title), Next: "more"}, nil
			}}
			p, err := BuildPrime(context.Background(), f, "v0.2.0")
			if err != nil {
				t.Fatal(err)
			}
			if got := f.ops(); !slices.Equal(got, tc.wantOps) {
				t.Fatalf("ops %v", got)
			}
			b, _ := json.Marshal(p)
			if Tokens(b) > MaxPrimeTokens || Tokens([]byte(p.Text())) > MaxPrimeTokens {
				t.Fatalf("prime is %d tokens as JSON, %d as text; cap %d", Tokens(b), Tokens([]byte(p.Text())), MaxPrimeTokens)
			}
			if len(p.Ready) != tc.n || len(p.Notices) != 1 || p.Project != "example" {
				t.Fatalf("prime %+v", p)
			}
			txt := p.Text()
			if !strings.Contains(txt, "notice: the server runs starfixd v0.1.0") || !strings.Contains(txt, "ready:\n  sf-00000000 P2 ") {
				t.Fatalf("text:\n%s", txt)
			}
		})
	}
}

// Even a prime with nothing to cut but notices stays in budget.
func TestPrimeFitDropsToBudget(t *testing.T) {
	p := &Prime{Project: "example", Session: "s", Working: summaries(50, 100), Ready: summaries(50, 100),
		Notices: []string{strings.Repeat("n", 5000)}}
	p.fit()
	if b, _ := json.Marshal(p); Tokens(b) > MaxPrimeTokens || !p.More {
		t.Fatalf("%d tokens, more %v", Tokens(b), p.More)
	}
}

// start returns the issue with its handoff and branch, cut to the budget.
func TestStart(t *testing.T) {
	at := time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)
	f := &fakeConn{reply: func(op string, args any) (any, error) {
		if a, ok := args.(proto.StartArgs); !ok || a.ID != "sf-a1b2" {
			return nil, fmt.Errorf("%s args %#v", op, args)
		}
		return proto.StartResult{
			Issue: proto.Issue{ID: "sf-a1b2", Rev: 3, Title: "Fix the login redirect", Type: "bug", Priority: 1,
				Status: "in_progress", Body: strings.Repeat("b", 20000), Acceptance: "redirects to /home"},
			Handoff: &proto.Handoff{Comment: proto.Comment{Author: "bob", Kind: "handoff", Body: "tried the cookie path", CreatedAt: at},
				HandoffFields: proto.HandoffFields{State: "partial", Next: "try the header", Branch: "fix/sf-a1b2-x", To: "alice"}},
		}, nil
	}}
	cs, _ := connect(t, f)
	res := callTool(t, cs, "start", map[string]any{"id": "sf-a1b2"})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	var got Started
	if err := json.Unmarshal([]byte(text(t, res)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Branch != "fix/sf-a1b2-fix-the-login-redirect" || got.Rev != 3 || got.Acceptance != "redirects to /home" ||
		got.Handoff == nil || *got.Handoff != (Handoff{By: "bob", At: "2026-10-07T09:30Z", Note: "tried the cookie path",
		State: "partial", Next: "try the header", Branch: "fix/sf-a1b2-x", To: "alice"}) {
		t.Fatalf("start: %+v", got)
	}
	if !got.Truncated || Tokens([]byte(text(t, res))) > MaxResultTokens {
		t.Fatalf("start is %d tokens, truncated %v", Tokens([]byte(text(t, res))), got.Truncated)
	}
}

// digest passes its filters through, turns times into stamps and spans,
// and holds the worst case under its budget.
func TestDigest(t *testing.T) {
	until := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	item := func(i int, title int) proto.DigestItem {
		return proto.DigestItem{ID: fmt.Sprintf("sf-%08d", i), Title: strings.Repeat("t", title), Priority: 2, By: "alice",
			At: until.Add(-26 * time.Hour), Note: strings.Repeat("n", 200), From: "sf-00000000", BlockedBy: []string{"sf-1", "sf-2"}}
	}
	section := func(n, title int) []proto.DigestItem {
		var out []proto.DigestItem
		for i := range n {
			out = append(out, item(i, title))
		}
		return out
	}
	tests := []struct {
		name      string
		n, title  int
		truncated bool
	}{
		{"small", 1, 20, false},
		{"worst case", 10, 500, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got proto.DigestArgs
			f := &fakeConn{reply: func(op string, args any) (any, error) {
				a, ok := args.(proto.DigestArgs)
				if !ok || op != proto.OpDigest {
					return nil, fmt.Errorf("%s args %#v", op, args)
				}
				got = a
				s := section(tc.n, tc.title)
				return proto.DigestResult{Since: until.Add(-7 * 24 * time.Hour), Until: until,
					Totals: proto.DigestTotals{Events: 99, Closed: tc.n, InProgress: tc.n},
					Closed: s, Started: s, InProgress: s, Stalled: s, Blocked: s, HandedOff: s, Created: s, Discovered: s}, nil
			}}
			cs, _ := connect(t, f)
			res := callTool(t, cs, "digest", map[string]any{"since": "7d", "by": "alice", "label": "ui"})
			if res.IsError {
				t.Fatal(text(t, res))
			}
			if got != (proto.DigestArgs{Since: "7d", By: "alice", Label: "ui"}) {
				t.Fatalf("args %+v", got)
			}
			raw := text(t, res)
			var d Digest
			if err := json.Unmarshal([]byte(raw), &d); err != nil {
				t.Fatal(err)
			}
			if Tokens([]byte(raw)) > MaxDigestTokens || d.Truncated != tc.truncated {
				t.Fatalf("%d tokens (cap %d), truncated %v", Tokens([]byte(raw)), MaxDigestTokens, d.Truncated)
			}
			if d.Since != "2026-09-30T12:00Z" || d.Until != "2026-10-07T12:00Z" || d.Totals.Events != 99 || len(d.Closed) == 0 {
				t.Fatalf("digest %+v", d)
			}
			if c := d.Closed[0]; c.At != "2026-10-06T10:00Z" || c.For != "" || c.By != "alice" || len(c.Title) > 100+len("…") {
				t.Errorf("closed %+v", c)
			}
			if len(d.InProgress) == 0 {
				t.Fatal("in progress dropped entirely")
			}
			if ip := d.InProgress[0]; ip.For != "1d2h" || ip.At != "" {
				t.Errorf("in progress %+v", ip)
			}
		})
	}
}

// start claims for Lease and remembers the epoch: finish and a releasing
// handoff pass it, and the renewer keeps it alive until the server says
// the claim is gone (the inbox reports that: TestPrimeInbox).
func TestClaimsFollowTheSession(t *testing.T) {
	var mu sync.Mutex
	held := []proto.Claim{{ID: "sf-1", Epoch: 3}, {ID: "sf-2", Epoch: 1}}
	f := &fakeConn{who: "alice", reply: func(op string, args any) (any, error) {
		switch op {
		case proto.OpStart:
			id := args.(proto.StartArgs).ID
			epoch := int64(3)
			if id == "sf-2" {
				epoch = 1
			}
			return proto.StartResult{Issue: proto.Issue{ID: id, Type: "task"}, Claim: &proto.Claim{ID: id, Epoch: epoch}}, nil
		case proto.OpRenew:
			mu.Lock()
			defer mu.Unlock()
			return proto.ClaimsResult{Claims: held}, nil
		case proto.OpList, proto.OpReady:
			return proto.ListResult{Issues: []proto.Summary{}}, nil
		case proto.OpFinish:
			return proto.FinishResult{ID: "sf-1", Rev: 4}, nil
		}
		return proto.WriteResult{ID: "sf-2", Rev: 2}, nil
	}}
	cs, _, s := connectServer(t, f)
	ctx := t.Context()

	s.Renew(ctx) // nothing held: no call
	callTool(t, cs, "start", map[string]any{"id": "sf-1"})
	callTool(t, cs, "start", map[string]any{"id": "sf-2"})
	s.Renew(ctx)

	// sf-2's claim lapses on the server.
	mu.Lock()
	held = held[:1]
	mu.Unlock()
	s.Renew(ctx)
	callTool(t, cs, "finish", map[string]any{"id": "sf-1"})
	callTool(t, cs, "handoff", map[string]any{"id": "sf-2", "note": "n", "release": true})
	s.Renew(ctx) // nothing held, but connected: renew keeps the session in who

	want := []string{"start", "start", "renew", "renew", "finish", "handoff", "renew"}
	if got := f.ops(); !slices.Equal(got, want) {
		t.Fatalf("ops %v, want %v", got, want)
	}
	if a := f.calls[0].args.(proto.StartArgs); a.Lease != Lease {
		t.Errorf("start lease %q", a.Lease)
	}
	if a := f.calls[4].args.(proto.FinishArgs); a.Epoch != 3 {
		t.Errorf("finish epoch %d, want 3", a.Epoch)
	}
	if a := f.calls[5].args.(proto.HandoffArgs); a.Epoch != 0 {
		t.Errorf("release of a lost claim sent epoch %d", a.Epoch)
	}
}

// who lists the agents seen recently, terse, and drops the least recently
// seen to fit the budget.
func TestWho(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		in     map[string]any
		agents int
		since  string
		shown  int
		more   bool
	}{
		{name: "default window", in: nil, agents: 2, since: "", shown: 2},
		{name: "a wider window", in: map[string]any{"since": "2h"}, agents: 2, since: "2h", shown: 2},
		{name: "cut to the budget", in: nil, agents: 200, shown: -1, more: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var agents []proto.Agent
			for i := range tc.agents {
				agents = append(agents, proto.Agent{Principal: "alice", Session: fmt.Sprintf("s-%03d-%s", i, strings.Repeat("x", 40)),
					Machine: "laptop-a", Harness: "claude-code", Started: now.Add(-time.Hour),
					LastSeen: now.Add(-time.Duration(i+2) * time.Minute), Claims: []string{"sf-a1"}})
			}
			f := &fakeConn{reply: func(string, any) (any, error) {
				return proto.WhoResult{Now: now, Agents: agents}, nil
			}}
			cs, _ := connect(t, f)
			res := callTool(t, cs, "who", tc.in)
			if res.IsError {
				t.Fatalf("who: %s", text(t, res))
			}
			body := text(t, res)
			if Tokens([]byte(body)) > MaxResultTokens {
				t.Fatalf("who result costs ~%d tokens, over %d", Tokens([]byte(body)), MaxResultTokens)
			}
			var w Who
			if err := json.Unmarshal([]byte(body), &w); err != nil {
				t.Fatal(err)
			}
			if a := f.calls[0].args.(proto.WhoArgs); a.Since != tc.since {
				t.Errorf("who sent since %q, want %q", a.Since, tc.since)
			}
			if tc.shown >= 0 && len(w.Agents) != tc.shown || w.More != tc.more {
				t.Fatalf("who = %d agents, more %v; want %d, %v", len(w.Agents), w.More, tc.shown, tc.more)
			}
			first := w.Agents[0]
			if first.Principal != "alice" || first.Machine != "laptop-a" || first.Harness != "claude-code" ||
				first.Seen != "2m" || !slices.Equal(first.Claims, []string{"sf-a1"}) {
				t.Errorf("who agent = %+v", first)
			}
		})
	}
}
