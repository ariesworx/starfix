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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// fakeConn answers calls with reply. A reply returning errLost breaks the
// connection, as a dropped SSH session does.
type fakeConn struct {
	reply func(op string, args any) (any, error)
	mu    sync.Mutex
	calls []call
	lost  error
	who   string
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
	return cs, d
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
	ro := []string{"blocked", "comments", "history", "list", "prime", "ready", "show"}
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
	want := []string{"blocked", "close", "comment", "comments", "create", "dep", "history", "label", "list", "prime",
		"ready", "reopen", "show", "update"}
	if !slices.Equal(names, want) {
		t.Errorf("tools = %v\nwant    %v", names, want)
	}
	// Design §5 aims for about 2k tokens for the whole verb set. These 14
	// tools are about 5 KiB, some 1.3k real tokens; the budget below is in
	// Tokens' deliberately high estimate. Counted is what a model reads:
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
	if got, budget := Tokens(b), 1750; got > budget {
		t.Errorf("tool schemas cost ~%d tokens, budget %d", got, budget)
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
	} {
		res := callTool(t, cs, tc.tool, tc.args)
		if res.IsError || text(t, res) != tc.want {
			t.Errorf("%s: %s (error %v), want %s", tc.tool, text(t, res), res.IsError, tc.want)
		}
	}
	// update without rev reads it first; label sends one call per label.
	want := []string{"create", "show", "update", "close", "dep.add", "label.rm", "label.rm", "comment"}
	if got := f.ops(); !slices.Equal(got, want) {
		t.Errorf("ops %v, want %v", got, want)
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
		{"not found", proto.Errf(proto.CodeNotFound, "find the id with `starfix list`", "issue sf-x not found"),
			"not_found: issue sf-x not found\nfix: find the id with list or ready"},
		{"conflict", proto.Errf(proto.CodeConflict, "re-read with `starfix show sf-x`", "sf-x changed since rev 1 (now rev 2 by bob)"),
			"conflict: sf-x changed since rev 1 (now rev 2 by bob)\nfix: call show for the current rev, then retry with that rev if your change still applies"},
		{"closed", proto.Errf(proto.CodeInvalid, "reopen it with `starfix reopen sf-x`", "issue sf-x is closed"),
			"invalid: issue sf-x is closed\nfix: call reopen first"},
		{"cycle", proto.Errf(proto.CodeCycle, "remove an edge with `starfix dep rm`", "sf-a depends on sf-b, so this would make a cycle"),
			"cycle: sf-a depends on sf-b, so this would make a cycle\nfix: remove an edge with dep (action rm), or choose another parent"},
		{"invalid", proto.Errf(proto.CodeInvalid, "correct it and retry; `starfix update -h` lists the options", "title is empty"),
			"invalid: title is empty\nfix: correct the arguments and retry"},
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
		{"typical", "alice", 3, 60, []string{"list", "ready"}},
		{"worst case", "alice", 5, 500, []string{"list", "ready"}},
		{"old server, no principal", "", 5, 500, []string{"ready"}},
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
