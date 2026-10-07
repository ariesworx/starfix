package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
)

// The write rate limit, end to end: past its burst a principal's writes
// are refused with busy and a wait, on stderr and in --json; reads and
// other principals go on.
func TestWriteRateLimit(t *testing.T) {
	w := newWorld(t, daemonOpts{limits: server.Limits{WriteBurst: 2, WriteRate: 0.001}})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.ok("create", "one")
	alice.ok("create", "two")
	r := alice.run("v0.2.0", "create", "three")
	if r.code != 1 || !strings.Contains(r.stderr, "over the write limit") || !strings.Contains(r.stderr, "fix: wait ") {
		t.Fatalf("third write: exit %d\n%s", r.code, r.stderr)
	}
	r = alice.run("v0.2.0", "--json", "create", "four")
	if e := decode[map[string]proto.Error](t, r.stdout)["error"]; e.Code != proto.CodeBusy {
		t.Errorf("--json refusal = %+v, want code busy", e)
	}
	if out := alice.ok("list"); !strings.Contains(out, "one") {
		t.Errorf("list while over the write limit:\n%s", out)
	}
	bob.ok("create", "bob's")
}

// The connection cap, end to end: a principal holding its limit of
// connections is refused another at the handshake, with busy; another
// principal is not.
func TestConnectionCap(t *testing.T) {
	w := newWorld(t, daemonOpts{limits: server.Limits{ConnsPerPrincipal: 1}})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	alice.rawDial("s-held") // holds alice's one connection
	r := alice.run("v0.2.0", "list")
	if r.code != 1 || !strings.Contains(r.stderr, "alice is at its connection limit (1)") || !strings.Contains(r.stderr, "fix: ") {
		t.Fatalf("alice's second connection: exit %d\n%s", r.code, r.stderr)
	}
	r = alice.run("v0.2.0", "--json", "list")
	if e := decode[map[string]proto.Error](t, r.stdout)["error"]; e.Code != proto.CodeBusy {
		t.Errorf("--json refusal = %+v, want code busy", e)
	}
	bob.ok("list")
}

// Paged comments, end to end: -n shows the newest and says how many came
// before.
func TestCommentsNewest(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	id := decode[proto.CreateResult](t, alice.ok("--json", "create", "talk")).ID
	for _, c := range []string{"first", "second", "third"} {
		alice.ok("comment", id, c)
	}
	out := alice.ok("comments", id, "-n", "2")
	if !strings.Contains(out, "(1 earlier not shown") || strings.Contains(out, "first") || !strings.Contains(out, "third") {
		t.Errorf("comments -n 2:\n%s", out)
	}
	if out := alice.ok("history", id, "-n", "1"); !strings.Contains(out, "(3 earlier not shown") {
		t.Errorf("history -n 1:\n%s", out)
	}
}
