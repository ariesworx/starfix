package e2e

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// clock is a test clock for the store, moved by hand.
type clock struct {
	mu sync.Mutex // guards t
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// lines is a buffer safe to read while the CLI writes to it.
type lines struct {
	mu sync.Mutex // guards b
	b  bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// waitFor polls f until it holds, or fails the test.
func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// content runs a tool and returns every text block of its result.
func (a *agent) content(name string, args map[string]any) []string {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := a.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil || res.IsError {
		a.t.Fatalf("%s: %v %+v", name, err, res)
	}
	var out []string
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			a.t.Fatalf("%s: content %T", name, c)
		}
		out = append(out, tc.Text)
	}
	return out
}

// Alice's agent loses its claim to bob after the lease runs out, and hears
// of it on its next tool result without asking; bob hands the issue back
// with a structured handoff, and alice's agent finds it in the inbox and
// on start.
func TestInboxLostClaimAndHandoff(t *testing.T) {
	clk := &clock{t: time.Now().UTC()}
	w := newWorld(t, daemonOpts{now: clk.now})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-alice"
	ag := alice.mcp("v0.2.0")

	id := decode[proto.WriteResult](t, alice.ok("create", "Fix", "the", "parser", "--json")).ID
	ag.ok("start", map[string]any{"id": id})
	if got := ag.content("show", map[string]any{"id": id}); len(got) != 1 {
		t.Fatalf("an inbox line with nothing new: %q", got)
	}

	clk.add(16 * time.Minute) // past the agent's 15m lease, with no renewal
	bob.ok("start", id)

	got := ag.content("show", map[string]any{"id": id})
	if len(got) != 2 || got[1] != "inbox: 1 new (call inbox)" {
		t.Fatalf("next tool result after the takeover: %q", got)
	}
	if got := ag.content("ready", nil); len(got) != 1 {
		t.Fatalf("the line repeats: %q", got)
	}

	bob.ok("handoff", id, "parser half done", "--state", "partial", "--next", "write the tests",
		"--branch", "fix/"+id+"-parser", "--to", "alice", "--release")

	box := decode[mcpserver.Inbox](t, ag.content("inbox", nil)[0])
	if box.Unread != 2 || len(box.Items) != 2 {
		t.Fatalf("inbox: %+v", box)
	}
	handoff, lost := box.Items[0], box.Items[1] // newest first
	if lost.Kind != "claim.lost" || lost.Issue != id || lost.From != "bob" ||
		handoff.Kind != "handoff" || handoff.Issue != id || handoff.Body != "write the tests" {
		t.Fatalf("items: %+v", box.Items)
	}
	box = decode[mcpserver.Inbox](t, ag.content("inbox", map[string]any{"ack": []int64{lost.ID, handoff.ID}})[0])
	if box.Acked != 2 || box.Unread != 0 || len(box.Items) != 0 {
		t.Fatalf("after ack: %+v", box)
	}

	show := decode[proto.ShowResult](t, alice.ok("show", id, "--json"))
	want := proto.HandoffFields{State: "partial", Next: "write the tests", Branch: "fix/" + id + "-parser", To: "alice"}
	if h := show.Handoff; h == nil || h.HandoffFields != want || h.Body != "parser half done" || h.Author != "bob" {
		t.Fatalf("show handoff: %+v", show.Handoff)
	}
	text := alice.ok("show", id)
	for _, s := range []string{"handoff from bob", "(partial, to alice)", "next: write the tests", "on branch: fix/" + id} {
		if !strings.Contains(text, s) {
			t.Errorf("show lacks %q:\n%s", s, text)
		}
	}

	st := decode[mcpserver.Started](t, ag.ok("start", map[string]any{"id": id}))
	if h := st.Handoff; h == nil || h.State != "partial" || h.Next != "write the tests" || h.To != "alice" || h.By != "bob" {
		t.Fatalf("start handoff: %+v", st.Handoff)
	}
}

// A mention reaches a known principal's inbox, and `sfx watch --json`
// prints it as it happens, one object per line, until interrupted.
func TestInboxMentionAndWatch(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	bob.ok("ready") // bob is a known principal from here on
	id := decode[proto.WriteResult](t, alice.ok("create", "Tidy", "the", "logs", "--json")).ID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errb lines
	code := make(chan int, 1)
	go func() { code <- bob.runTo(ctx, &out, &errb, "v0.2.0", "watch", "--json") }()
	waitFor(t, "sfx watch to start", func() bool { return strings.Contains(errb.String(), "watching your inbox") })

	alice.ok("comment", id, "@bob can you look at this?")
	waitFor(t, "the pushed mention", func() bool { return strings.Contains(out.String(), "\n") })
	cancel()
	if c := <-code; c != 0 {
		t.Fatalf("watch exit %d\n%s", c, errb.String())
	}
	first, _, _ := strings.Cut(out.String(), "\n")
	ev := decode[struct {
		Op   string          `json:"op"`
		Item proto.InboxItem `json:"item"`
	}](t, first)
	if ev.Op != "inbox" || ev.Item.Kind != "mention" || ev.Item.Issue != id || ev.Item.From != "alice" ||
		ev.Item.Body != "@bob can you look at this?" {
		t.Fatalf("watch line: %s", first)
	}

	box := decode[proto.InboxResult](t, bob.ok("inbox", "--json"))
	if box.Unread != 1 || len(box.Items) != 1 || box.Items[0].ID != ev.Item.ID {
		t.Fatalf("inbox: %+v", box)
	}
	if got := bob.ok("inbox", "--ack", "#"+strconv.FormatInt(ev.Item.ID, 10)); got != "acked 1\n" {
		t.Fatalf("ack: %q", got)
	}
	if got := bob.ok("inbox"); got != "no unread items\n" {
		t.Fatalf("after ack: %q", got)
	}
	if got := alice.ok("inbox"); got != "no unread items\n" {
		t.Fatalf("alice mentioned no one: %q", got)
	}
}
