package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// A tool call whose response is lost to a dropped connection, after the
// server applied it, is retried on a new connection with the same
// idempotency key: the agent gets the first result and the work exists
// once.
func TestMCPRetryAfterDropWritesOnce(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	ag := alice.mcp("v0.2.0")
	ag.ok("ready", nil) // connected

	w.dropReplyTo(proto.OpCreate)
	created := decode[mcpserver.Created](t, ag.ok("create", map[string]any{"title": "Only once"}))
	w.dropReplyTo(proto.OpComment)
	ag.ok("comment", map[string]any{"id": created.ID, "body": "noted once"})
	if ag.dials != 3 {
		t.Fatalf("dials = %d, want 3: each lost response reconnects", ag.dials)
	}

	list := decode[proto.ListResult](t, alice.ok("list", "--json"))
	if len(list.Issues) != 1 || list.Issues[0].ID != created.ID || created.Rev != 1 {
		t.Fatalf("issues = %+v, created %+v; want the one issue", list.Issues, created)
	}
	cs := decode[proto.CommentsResult](t, alice.ok("comments", created.ID, "--json"))
	if len(cs.Comments) != 1 || cs.Comments[0].Body != "noted once" {
		t.Fatalf("comments = %+v, want one", cs.Comments)
	}
}

// create names similar closed issues on standard error, so the id stays
// alone on standard output; show lists them and the checklist; accept
// ticks and waives; close refuses until every item is settled, unless
// forced.
func TestCLIAcceptanceAndSimilar(t *testing.T) {
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, dana := w.newUser("alice", ""), w.newUser("dana", "")
	old := strings.TrimSpace(alice.ok("create", "Login fails with expired token"))
	alice.ok("close", old)

	r := alice.run("v0.2.0", "create", "Expired token breaks login", "--acceptance", "Done when:\n- redirects home\n- has a test")
	if r.code != 0 || strings.Count(r.stdout, "\n") != 1 || !strings.Contains(r.stderr, "similar closed issues:\n  "+old+"  Login fails with expired token\n") {
		t.Fatalf("create: exit %d\nstdout %q\nstderr %q", r.code, r.stdout, r.stderr)
	}
	id := strings.TrimSpace(r.stdout)
	out := alice.ok("show", id)
	for _, want := range []string{"acceptance:\n  1 [ ] redirects home\n  2 [ ] has a test\n", "similar closed issues:\n  " + old} {
		if !strings.Contains(out, want) {
			t.Fatalf("show lacks %q:\n%s", want, out)
		}
	}

	r = alice.run("v0.2.0", "close", id)
	if r.code == 0 || !strings.Contains(r.stderr, "fix: tick what is met with `sfx accept "+id+" 1 2`") || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("close with open items: exit %d\n%s", r.code, r.stderr)
	}
	if out := alice.ok("accept", id, "1"); !strings.Contains(out, "1 [x] redirects home (alice)") || strings.Contains(out, "every item") {
		t.Fatalf("accept 1:\n%s", out)
	}
	if out := alice.ok("accept", id, "2", "--waive", "covered by the e2e suite"); !strings.Contains(out, "2 [~] has a test (waived by alice: covered by the e2e suite)") ||
		!strings.Contains(out, id+": every item is ticked or waived") {
		t.Fatalf("accept 2 --waive:\n%s", out)
	}
	alice.ok("close", id)

	forced := strings.TrimSpace(alice.ok("create", "Obsolete", "--acceptance", "- never done"))
	r = alice.run("v0.2.0", "close", forced, "--force")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: close --force is for starfix admins\nfix: ") {
		t.Fatalf("close --force by alice: exit %d\n%s", r.code, r.stderr)
	}
	dana.ok("close", forced, "--force")
	h := decode[proto.HistoryResult](t, alice.ok("history", forced, "--json"))
	if last := h.Events[len(h.Events)-1]; last.Op != "issue.close" || !strings.Contains(string(last.After), `"acceptance_overridden":[1]`) {
		t.Fatalf("forced close event: %+v", last)
	}
}
