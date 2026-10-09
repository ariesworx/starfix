package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/proto"
)

// Prices and cost, through the CLI to a real daemon: an admin sets a
// price and anyone lists it, a non-admin is refused with the fix, and
// show, digest and cost price the tokens an agent's session reported.
func TestCostCLI(t *testing.T) {
	w := newWorld(t, daemonOpts{admins: []string{"dana"}})
	alice, dana := w.newUser("alice", ""), w.newUser("dana", "")
	alice.env["STARFIX_SESSION"] = "s-agent"
	epic := strings.TrimSpace(alice.ok("create", "Client work", "-t", "epic", "--account", "acme"))
	task := strings.TrimSpace(alice.ok("create", "Do the work", "--parent", epic))
	alice.ok("start", task)

	rates := []string{"--input", "4", "--output", "20", "--cache-write", "5", "--cache-write-1h", "8", "--cache-read", "0.4"}
	set := append([]string{"admin", "prices", "set", "example-large", "--from", "2026-01-01"}, rates...)
	if got := dana.ok(set...); got != "example-large from 2026-01-01: added\n" {
		t.Fatalf("admin prices set: %q", got)
	}
	if got := decode[proto.PriceSetResult](t, dana.ok(append(set, "--json")...)); got.Change != "unchanged" {
		t.Fatalf("admin prices set again: %+v, want unchanged", got)
	}
	r := alice.run("v0.2.0", set...)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: prices set is for starfix admins\n") ||
		!strings.Contains(r.stderr, "fix: ask a starfix admin") {
		t.Fatalf("admin prices set by alice: exit %d\n%s", r.code, r.stderr)
	}
	if got := alice.ok("admin", "prices"); !strings.Contains(got, "example-large  2026-01-01  4      20      5            8               0.4         dana\n") {
		t.Fatalf("admin prices:\n%s", got)
	}

	at := time.Now().UTC()
	in, out, cw, cw1h, cr := int64(1_000_000), int64(100_000), int64(200_000), int64(50_000), int64(3_000_000)
	unknown := int64(42)
	raw := alice.rawDial("s-agent")
	raw.usageCall(1, []proto.UsageRecord{
		// 4 + 2 + 0.15×5 + 0.05×8 + 3×0.4 = 8.35 USD
		{Harness: "claude-code", RequestID: "msg_01", Model: "example-large", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &in, Output: &out, CacheWrite: &cw, CacheWrite1h: &cw1h, CacheRead: &cr}},
		{Harness: "claude-code", RequestID: "msg_02", Model: "example-unpriced", At: at, Granularity: "request",
			Tokens: proto.Tokens{Input: &unknown}},
	})

	if s := alice.ok("show", task); !strings.Contains(s, "\ncost $8.35 list price; some tokens unpriced\n") {
		t.Errorf("show lacks the cost line:\n%s", s)
	}
	if d := alice.ok("digest"); !strings.Contains(d, "\ncost $8.35 list price; some tokens unpriced\n") {
		t.Errorf("digest lacks the cost line:\n%s", d)
	}
	report := alice.ok("cost", "--since", "1h")
	for _, want := range []string{
		"cost by account, ",
		"  acme  $8.35  4.3M tokens  unpriced\n",
		"total $8.35, 4.3M tokens\n",
		"unpriced: no price for example-unpriced; an admin sets one with `sfx admin prices set`\n",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("cost lacks %q:\n%s", want, report)
		}
	}
	js := decode[proto.CostResult](t, alice.ok("cost", "--since", "1h", "--by", "epic", "--json"))
	if len(js.Groups) != 1 || js.Groups[0].Key != epic || js.Groups[0].Title != "Client work" || js.Groups[0].CostUSD != "8.35" ||
		js.Total.CostUSD != "8.35" {
		t.Errorf("cost --by epic --json = %+v", js)
	}
	if r := alice.run("v0.2.0", "cost", "--since", "2026-02-01", "--until", "2026-01-01"); r.code != cli.ExitFailure ||
		!strings.Contains(r.stderr, "is before since") || !strings.Contains(r.stderr, "fix: correct it and retry; `sfx cost -h` lists the options") {
		t.Errorf("cost with until before since: exit %d\n%s", r.code, r.stderr)
	}
}
