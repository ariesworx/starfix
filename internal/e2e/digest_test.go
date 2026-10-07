package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// A day's work by two people, summarized over SSH on the CLI and over MCP.
func TestDigest(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	a := strings.TrimSpace(alice.ok("create", "Fix the login redirect", "-t", "bug", "-p", "1", "--label", "web"))
	b := strings.TrimSpace(bob.ok("create", "Ship the release", "-p", "0"))
	alice.ok("start", a)
	f := decode[proto.FinishResult](t, alice.ok("finish", a, "--handoff", "cookie path is gone",
		"--discovered", "Flaky login test", "--json"))
	bob.ok("dep", "add", b, f.Created[0])
	bob.ok("start", f.Created[0])

	out := bob.ok("digest")
	for _, want := range []string{
		" (1d): ",
		"closed 1\n  " + a + "  P1  Fix the login redirect  alice  0m ago\n",
		"started 2\n",
		"in progress 1\n  " + f.Created[0] + "  P2  Flaky login test  bob  for 0m\n",
		"blocked 1\n  " + b + "  P0  Ship the release  by " + f.Created[0] + "\n",
		"handed off 1\n  " + a + "  P1  Fix the login redirect  alice  0m ago  cookie path is gone\n",
		"created 3\n  " + b + "  P0",
		"discovered 1\n  " + f.Created[0] + "  P2  Flaky login test  alice  0m ago  from " + a + "\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("digest lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "stalled") {
		t.Errorf("nothing is stalled yet:\n%s", out)
	}

	byAlice := decode[proto.DigestResult](t, bob.ok("digest", "--since", "7d", "--by", "alice", "--json"))
	if tt := byAlice.Totals; tt.Closed != 1 || tt.Started != 1 || tt.Created != 2 || tt.InProgress != 0 || tt.Blocked != 0 {
		t.Errorf("by alice: %+v", tt)
	}
	web := decode[proto.DigestResult](t, bob.ok("digest", "--label", "web", "--json"))
	if tt := web.Totals; tt.Closed != 1 || tt.Created != 1 || tt.Discovered != 1 || tt.Blocked != 0 {
		t.Errorf("label web: %+v", tt)
	}

	// Reading twice gives the same digest: nothing was written.
	again := decode[proto.DigestResult](t, bob.ok("digest", "--label", "web", "--json"))
	if again.Totals != web.Totals {
		t.Errorf("second read: %+v, first %+v", again.Totals, web.Totals)
	}

	r := bob.run("v0.2.0", "digest", "--since", "yesterday")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, `sfx: invalid input: since "yesterday" must be`) ||
		!strings.Contains(r.stderr, "fix: correct it and retry; `sfx digest -h` lists the options") {
		t.Fatalf("bad since: exit %d\n%s", r.code, r.stderr)
	}

	ag := alice.mcp("v0.2.0")
	d := decode[mcpserver.Digest](t, ag.ok("digest", map[string]any{"since": "24h"}))
	if d.Totals.Closed != 1 || len(d.Closed) != 1 || d.Closed[0].ID != a || d.Closed[0].At == "" ||
		len(d.InProgress) != 1 || d.InProgress[0].By != "bob" || d.InProgress[0].For != "0m" || d.Truncated {
		t.Fatalf("mcp digest: %+v", d)
	}
	out, isErr := ag.call("digest", map[string]any{"since": "soon"})
	if !isErr || !strings.HasPrefix(out, "invalid: ") || !strings.HasSuffix(out, "\nfix: correct the arguments and retry") {
		t.Fatalf("mcp bad since: %q", out)
	}
}
