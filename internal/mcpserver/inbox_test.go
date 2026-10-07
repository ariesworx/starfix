package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
)

// lines returns a result's text blocks after the first.
func lines(res *mcp.CallToolResult) []string {
	var out []string
	for _, c := range res.Content[1:] {
		if tc, ok := c.(*mcp.TextContent); ok {
			out = append(out, tc.Text)
		}
	}
	return out
}

func mention(id int64) proto.Push {
	return proto.Push{Op: proto.EvInbox, Item: &proto.InboxItem{ID: id, Kind: "mention"}}
}

// Pushed items are kept, and the next tool result says how many in one
// extra line, once; errors carry it too. The result itself is unchanged.
func TestInboxLine(t *testing.T) {
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		if op == proto.OpClose {
			return nil, proto.Errf(proto.CodeNotFound, "find the id with `sfx list`", "issue sf-x not found")
		}
		return proto.ListResult{Issues: []proto.Summary{}}, nil
	}}
	cs, _, s := connectServer(t, f)

	if res := callTool(t, cs, "ready", nil); len(lines(res)) != 0 {
		t.Fatalf("ready with nothing pushed carries %q", lines(res))
	}
	s.Push(mention(1))
	s.Push(mention(2))
	res := callTool(t, cs, "ready", nil)
	if got := lines(res); !slices.Equal(got, []string{"inbox: 2 new (call inbox)"}) {
		t.Fatalf("lines after two pushes = %q", got)
	}
	if text(t, res) != `{"issues":[]}` {
		t.Errorf("the result itself changed: %s", text(t, res))
	}
	if got := lines(callTool(t, cs, "ready", nil)); len(got) != 0 {
		t.Errorf("the line repeated: %q", got)
	}
	s.Push(mention(3))
	res = callTool(t, cs, "close", map[string]any{"id": "sf-x"})
	if !res.IsError || !slices.Equal(lines(res), []string{"inbox: 1 new (call inbox)"}) {
		t.Errorf("error result lines = %q (error %v)", lines(res), res.IsError)
	}
	// The inbox tool and prime show the inbox, so they reset the count
	// without the line.
	s.Push(mention(4))
	if got := lines(callTool(t, cs, "inbox", nil)); len(got) != 0 {
		t.Errorf("inbox carries %q", got)
	}
	if got := lines(callTool(t, cs, "ready", nil)); len(got) != 0 {
		t.Errorf("after inbox, ready carries %q", got)
	}
}

// sfx mcp watches on each new connection, and again after a resync.
func TestWatchOnEachConnection(t *testing.T) {
	ok := func(string, any) (any, error) { return proto.ListResult{}, nil }
	first := &fakeConn{reply: func(string, any) (any, error) { return nil, errLost }}
	second := &fakeConn{reply: ok}
	cs, _, s := connectServer(t, first, second)
	callTool(t, cs, "ready", nil) // first drops; the retry dials second
	if first.watches != 1 || second.watches != 1 {
		t.Fatalf("watches = %d, %d; want one per connection", first.watches, second.watches)
	}
	callTool(t, cs, "ready", nil)
	if second.watches != 1 {
		t.Fatalf("watched again without a resync: %d", second.watches)
	}
	s.Push(proto.Push{Op: proto.EvResync})
	res := callTool(t, cs, "ready", nil)
	if second.watches != 2 {
		t.Errorf("watches after a resync = %d, want 2", second.watches)
	}
	if got := lines(res); !slices.Equal(got, []string{"inbox: new items (call inbox)"}) {
		t.Errorf("lines after a resync = %q", got)
	}
}

// inbox acks the ids given, then lists what is unread, compact and
// capped.
func TestInboxTool(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var items []proto.InboxItem
	for i := range 60 {
		items = append(items, proto.InboxItem{ID: int64(100 - i), Session: "s-test", Kind: "mention", Issue: "sf-a1b2",
			Body: strings.Repeat("b", 200), From: "bob", At: at})
	}
	f := &fakeConn{reply: func(op string, args any) (any, error) {
		switch op {
		case proto.OpAck:
			return proto.AckResult{Acked: len(args.(proto.AckArgs).IDs)}, nil
		case proto.OpInbox:
			n := args.(proto.InboxArgs).Limit
			return proto.InboxResult{Items: items[:min(n, len(items))], Unread: 61}, nil
		}
		return nil, fmt.Errorf("unexpected %s", op)
	}}
	cs, _ := connect(t, f)
	res := callTool(t, cs, "inbox", map[string]any{"ack": []any{7, 8}})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	body := text(t, res)
	var got Inbox
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if Tokens([]byte(body)) > MaxResultTokens || got.Unread != 61 || got.Acked != 2 || !got.More || len(got.Items) == 0 {
		t.Fatalf("inbox = %d tokens, %+v", Tokens([]byte(body)), got)
	}
	if it := got.Items[0]; it != (InboxItem{ID: 100, Kind: "mention", Issue: "sf-a1b2", From: "bob", At: "2026-10-07T12:00Z",
		Body: strings.Repeat("b", 200)}) {
		t.Errorf("first item = %+v", it)
	}
	if ops := f.ops(); !slices.Equal(ops, []string{"ack", "inbox"}) {
		t.Errorf("ops = %v, want ack then inbox", ops)
	}
	if a := f.calls[0].args.(proto.AckArgs); !slices.Equal(a.IDs, []int64{7, 8}) || a.All {
		t.Errorf("ack args = %+v", a)
	}
}

// prime shows the unread count and the newest items, and lists the
// issues whose claims this session lost, from its unread claim.lost items.
func TestPrimeInbox(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		inbox   func() (any, error)
		unread  int
		lost    []string
		text    []string
		missing []string
	}{
		{
			name: "lost claims and mentions",
			inbox: func() (any, error) {
				return proto.InboxResult{Unread: 5, Items: []proto.InboxItem{
					{ID: 9, Kind: "claim.lost", Issue: "sf-2", Body: "lease expired", From: "starfixd", At: at},
					{ID: 8, Kind: "mention", Issue: "sf-3", Body: "@alice look", From: "bob", At: at},
					{ID: 7, Kind: "claim.lost", Issue: "sf-4", Body: "taken by bob/s-b", From: "bob", At: at},
					{ID: 6, Kind: "assigned", Issue: "sf-5", Body: "Fix it", From: "bob", At: at},
				}}, nil
			},
			unread: 5, lost: []string{"sf-2", "sf-4"},
			text: []string{"lost claim (stop work on it): sf-2 sf-4", "inbox: 5 unread (call inbox)",
				"  #9 claim.lost sf-2 from starfixd: \"lease expired\"", "  #8 mention sf-3 from bob: \"@alice look\""},
		},
		{
			name:    "empty",
			inbox:   func() (any, error) { return proto.InboxResult{Items: []proto.InboxItem{}}, nil },
			missing: []string{"inbox:", "lost claim"},
		},
		{
			name: "a server without the inbox",
			inbox: func() (any, error) {
				return nil, proto.Errf(proto.CodeInvalid, "upgrade starfix to match the server", `unknown operation "inbox"`)
			},
			missing: []string{"inbox:", "lost claim"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeConn{who: "alice", reply: func(op string, _ any) (any, error) {
				if op == proto.OpInbox {
					return tc.inbox()
				}
				return proto.ListResult{Issues: []proto.Summary{}}, nil
			}}
			p, err := BuildPrime(context.Background(), f, "v0.2.0")
			if err != nil {
				t.Fatal(err)
			}
			if p.Unread != tc.unread || !slices.Equal(p.Lost, tc.lost) || len(p.Inbox) > primeInbox {
				t.Fatalf("prime unread %d, lost %v, %d items; want %d, %v, at most %d", p.Unread, p.Lost, len(p.Inbox),
					tc.unread, tc.lost, primeInbox)
			}
			txt := p.Text()
			for _, want := range tc.text {
				if !strings.Contains(txt, want) {
					t.Errorf("prime text lacks %q:\n%s", want, txt)
				}
			}
			for _, not := range tc.missing {
				if strings.Contains(txt, not) {
					t.Errorf("prime text has %q:\n%s", not, txt)
				}
			}
		})
	}
}
