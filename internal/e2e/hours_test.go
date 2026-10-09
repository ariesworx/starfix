package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// Hours and plans, through the CLI and MCP to a real daemon: a person
// logs, lists and undoes their hours, another is refused the undo; an
// admin sets a plan and a non-admin is refused; show, digest and cost
// carry the hours, and cost the plan's amortized cost beside the list
// price.
func TestHoursAndPlansCLI(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	w := newWorld(t, daemonOpts{admins: []string{"dana"}, now: clk.now})
	alice, bob, dana := w.newUser("alice", ""), w.newUser("bob", ""), w.newUser("dana", "")
	alice.env["STARFIX_SESSION"] = "s-agent"
	task := strings.TrimSpace(alice.ok("create", "Do the work"))
	alice.ok("start", task)

	e := decode[proto.HoursLogResult](t, alice.ok("log", "1h30m", task, "--note", "pairing", "--json"))
	if e.ID == "" || e.On != "2026-10-08" {
		t.Fatalf("log --json = %+v, want an entry for today", e)
	}
	if got := bob.ok("log", "15m", task); !strings.HasPrefix(got, "logged 0.25h on "+task+" for ") {
		t.Errorf("log 15m = %q", got)
	}
	if got := alice.ok("log", "--issue", task); !strings.Contains(got, "alice  "+task+"  1.5h   "+e.ID+"  pairing\n") {
		t.Errorf("log --issue:\n%s", got)
	}
	if r := bob.run("v0.2.0", "log", "--undo", e.ID); r.code != cli.ExitFailure ||
		!strings.Contains(r.stderr, "sfx: undoing another person's hours is for starfix admins\n") || !strings.Contains(r.stderr, "fix: ask a starfix admin") {
		t.Errorf("bob undoing alice's entry: exit %d\n%s", r.code, r.stderr)
	}
	if r := alice.run("v0.2.0", "log", "25h", task); r.code != cli.ExitUsage || !strings.Contains(r.stderr, "not a time from 1m to 24h") {
		t.Errorf("log 25h: exit %d\n%s", r.code, r.stderr)
	}
	if s := alice.ok("show", task); !strings.Contains(s, "\nlogged 1.75h: alice 1.5h, bob 0.25h\n") {
		t.Errorf("show lacks the hours:\n%s", s)
	}
	if d := alice.ok("digest"); !strings.Contains(d, "\nlogged 1.75h\n") {
		t.Errorf("digest lacks the hours:\n%s", d)
	}

	const month = "2026-10"
	set := []string{"admin", "plans", "set", "team", "--from", month, "--fee", "30", "--seats", "1", "--principal", "alice"}
	if got := dana.ok(set...); got != "team from "+month+": added\n" {
		t.Fatalf("admin plans set: %q", got)
	}
	if r := alice.run("v0.2.0", set...); r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: plans set is for starfix admins\n") {
		t.Errorf("admin plans set by alice: exit %d\n%s", r.code, r.stderr)
	}
	if got := bob.ok("admin", "plans"); !strings.Contains(got, "team  "+month+"  30   1      30       alice       dana\n") {
		t.Errorf("admin plans:\n%s", got)
	}
	if ps := decode[proto.PlansResult](t, bob.ok("admin", "--json", "plans")); len(ps.Plans) != 1 || ps.Plans[0].Name != "team" {
		t.Errorf("admin --json plans = %+v, want the team plan", ps)
	}

	in := int64(1000)
	alice.rawDial("s-agent").usageCall(1, []proto.UsageRecord{
		{Harness: "claude-code", RequestID: "msg_01", Model: "example-large", At: clk.now(), Granularity: "request", Tokens: proto.Tokens{Input: &in}},
	})
	clk.add(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Sub(clk.now())) // October has accrued its whole fee
	report := alice.ok("cost", "--since", month+"-01", "--by", "issue")
	for _, want := range []string{
		", list price and amortized\n",
		"  " + task + "  Do the work  unpriced  $30.00 amortized  1k tokens  1.75h logged\n",
		"total unpriced, $30.00 amortized, 1k tokens, 1.75h logged\n",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("cost lacks %q:\n%s", want, report)
		}
	}
	ag := alice.mcp("v0.2.0")
	if is := decode[mcpserver.Issue](t, ag.ok("show", map[string]any{"id": task})); !strings.Contains(is.Usage, "; logged 1.75h") {
		t.Errorf("mcp show usage %q, want the hours", is.Usage)
	}
	c := decode[mcpserver.Cost](t, ag.ok("cost", map[string]any{"since": month + "-01", "by": "issue"}))
	if len(c.Groups) != 1 || c.Groups[0] != task+" Do the work: unpriced, amortized $30.00, 1k tokens, 1.75h logged" {
		t.Errorf("mcp cost = %+v", c)
	}

	if got := alice.ok("log", "--undo", e.ID); got != "undone: entry "+e.ID+"\n" {
		t.Errorf("log --undo = %q", got)
	}
	if got := alice.ok("log", "--by", "alice"); got != "no hours logged\n" {
		t.Errorf("log --by alice after the undo = %q", got)
	}
}
