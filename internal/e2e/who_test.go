package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// Two sessions of one person, under two harnesses, show up in `sfx who`
// for another, with the claim the first holds.
func TestWhoListsSessions(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")

	alice.env = map[string]string{"STARFIX_SESSION": "s-one", "CLAUDECODE": "1"}
	id := strings.TrimSpace(alice.ok("create", "Wire the registry"))
	alice.ok("start", id)
	alice.env = map[string]string{"STARFIX_SESSION": "s-two", "GEMINI_CLI": "1"}
	alice.ok("ready")

	got := decode[proto.WhoResult](t, bob.ok("who", "--json"))
	var rows []string
	for _, a := range got.Agents {
		rows = append(rows, a.Principal+"/"+a.Session+" "+a.Machine+" "+a.Harness+" "+strings.Join(a.Claims, ","))
	}
	slices.Sort(rows)
	want := []string{"alice/s-one laptop-test claude-code " + id, "alice/s-two laptop-test gemini ", "bob/cli laptop-test  "}
	if !slices.Equal(rows, want) {
		t.Fatalf("who --json agents:\n%q\nwant\n%q", rows, want)
	}

	out := bob.ok("who")
	for _, line := range []string{
		"alice/s-one on laptop-test (claude-code) seen just now, holds " + id,
		"alice/s-two on laptop-test (gemini) seen just now",
		"bob/cli on laptop-test seen just now",
	} {
		if !strings.Contains(out, line+"\n") {
			t.Errorf("sfx who lacks %q:\n%s", line, out)
		}
	}
}
