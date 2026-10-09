package e2e

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/proto"
)

// The memory commands, through the CLI to a real daemon: remember with
// tags, an issue and a pin; recall by text, key and tag; a stale replace
// refused with its fix; pin, unpin and forget; and --json throughout.
func TestMemoryCLI(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := decode[proto.WriteResult](t, alice.ok("create", "Ship", "it", "--label", "release", "--json")).ID

	if got := alice.ok("remember", "deploy-window", "Deploys", "go", "out", "on", "weekday", "mornings.", "--tag", "ops,release",
		"--issue", id, "--pin"); got != "deploy-window (project) rev 1\n" {
		t.Fatalf("remember: %q", got)
	}
	alice.ok("remember", "editor", "-", "--scope", "user")
	out := bob.ok("recall", "weekday")
	if !strings.HasPrefix(out, "deploy-window (project, pinned) rev 1, alice, ") ||
		!strings.Contains(out, "  tags: ops, release; issue: "+id+"\n  Deploys go out on weekday mornings.\n") {
		t.Fatalf("bob's recall:\n%s", out)
	}
	if got := alice.ok("recall", "--key", "editor", "--scope", "user"); !strings.Contains(got, "editor (user) rev 1") ||
		!strings.Contains(got, "  from stdin\n") {
		t.Fatalf("alice's recall of her user memory:\n%s", got)
	}
	res := decode[proto.RecallResult](t, bob.ok("recall", "--tag", "ops", "--json"))
	if len(res.Memories) != 1 || res.Memories[0].Key != "deploy-window" || res.Memories[0].Rev != 1 {
		t.Fatalf("recall --tag --json: %+v", res)
	}

	r := bob.run("v0.2.0", "remember", "deploy-window", "Any", "time.")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "memory deploy-window exists in project scope at rev 1, written by alice") ||
		!strings.Contains(r.stderr, "fix: recall it with `sfx recall --key deploy-window --scope project`, merge, and retry with --rev 1") {
		t.Fatalf("remember over an existing key: exit %d\n%s", r.code, r.stderr)
	}
	wr := decode[proto.WriteResult](t, bob.ok("remember", "deploy-window", "Any", "weekday.", "--rev", "1", "--json"))
	if wr.Rev != 2 {
		t.Fatalf("replace: %+v", wr)
	}
	if got := bob.ok("recall", "--key", "deploy-window"); !strings.Contains(got, "  tags: ops, release; issue: "+id+"\n  Any weekday.\n") {
		t.Fatalf("a replace without --tag or --issue dropped them:\n%s", got)
	}
	if r := alice.run("v0.2.0", "forget", "deploy-window", "--rev", "1"); r.code != cli.ExitFailure ||
		!strings.Contains(r.stderr, "changed since rev 1 (now rev 2 by bob)") {
		t.Fatalf("stale forget: exit %d\n%s", r.code, r.stderr)
	}

	if got := bob.ok("unpin", "deploy-window"); got != "deploy-window (project) rev 3\n" {
		t.Fatalf("unpin: %q", got)
	}
	if got := bob.ok("memories"); strings.Contains(got, "pinned") || strings.Contains(got, "editor") {
		t.Fatalf("bob's memories after unpin:\n%s", got)
	}
	alice.ok("pin", "deploy-window")
	if got := alice.ok("forget", "deploy-window", "--rev", "4"); got != "forgot deploy-window (project)\n" {
		t.Fatalf("forget: %q", got)
	}
	if got := bob.ok("memories"); got != "no memories\n" {
		t.Fatalf("bob's memories after forget: %q", got)
	}
	if r := bob.run("v0.2.0", "pin", "deploy-window"); r.code != cli.ExitFailure ||
		!strings.Contains(r.stderr, "fix: find the key with `sfx memories --scope project`") {
		t.Fatalf("pin a forgotten memory: exit %d\n%s", r.code, r.stderr)
	}

	// Empty --tag and --issue clear what a replace would otherwise keep.
	alice.ok("remember", "scratch", "x", "--tag", "ops", "--issue", id)
	alice.ok("remember", "scratch", "y", "--tag", "", "--issue", "", "--rev", "1")
	if got := alice.ok("recall", "--key", "scratch"); strings.Contains(got, "tags:") || strings.Contains(got, "issue:") ||
		!strings.Contains(got, "  y\n") {
		t.Fatalf("a replace with empty --tag and --issue kept them:\n%s", got)
	}
}

// A memory that looks like it holds a credential is refused in every
// scope, and the refusal never repeats it.
func TestMemorySecretsRefused(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	token := "ghp_" + strings.Repeat("Ab1", 12) // an obviously fake GitHub token
	for _, scope := range []string{"project", "user", "team"} {
		r := alice.run("v0.2.0", "remember", "ci-token", "the", "token", "is", token, "--scope", scope)
		if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "the memory's body looks like it holds a secret (GitHub token)") ||
			!strings.Contains(r.stderr, "fix: remove the secret") || strings.Contains(r.stdout+r.stderr, token) {
			t.Errorf("%s scope: exit %d\n%s%s", scope, r.code, r.stdout, r.stderr)
		}
	}
	if got := alice.ok("memories"); got != "no memories\n" {
		t.Errorf("memories after refusals: %q", got)
	}
}

// A user-scope memory is its author's alone. Its key and body reach no
// other principal: not through recall, memories, start, prime, an issue's
// history, the digest, the pushed event stream or the MCP tools. The same
// key in another user's scope is another record.
func TestUserScopeMemoryIsPrivate(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	const secretKey, secretBody = "alice-only-key", "alice-only-body: she prefers tabs"
	id := decode[proto.WriteResult](t, alice.ok("create", "Shared", "work", "--label", "api", "--json")).ID

	watcher := bob.rawDial("s-watch")
	if err := watcher.enc.Encode(&proto.Frame{T: proto.FrameReq, ID: 1, Op: proto.OpWatch,
		Args: []byte(`{"events":true}`)}); err != nil {
		t.Fatal(err)
	}
	if f := watcher.next(); f.T != proto.FrameRes || f.Err != nil {
		t.Fatalf("watch: %+v", f)
	}

	alice.ok("remember", secretKey, secretBody, "--scope", "user", "--tag", "api", "--issue", id, "--pin")
	alice.ok("remember", secretKey, secretBody+", still", "--scope", "user", "--rev", "1")
	alice.ok("unpin", secretKey, "--scope", "user")
	alice.ok("remember", "editor", "vim", "--scope", "user")
	bob.ok("remember", "editor", "bob's own", "--scope", "user") // a different record: no conflict
	alice.ok("update", id, "--title", "Shared work, renamed")    // an issue event, pushed after hers

	leaks := func(where, s string) {
		t.Helper()
		if strings.Contains(s, "alice-only") {
			t.Errorf("%s shows alice's user memory:\n%s", where, s)
		}
	}
	// The pushed stream: frames up to the issue event carry none of it.
	for {
		f := watcher.next()
		leaks("a pushed frame", string(f.E)+string(f.OK))
		p, err := proto.DecodePush(f)
		if err != nil {
			t.Fatal(err)
		}
		if p.Event != nil && strings.HasPrefix(p.Event.Issue, "memory:") {
			t.Errorf("a memory event was pushed: %+v", p.Event)
		}
		if p.Event != nil && p.Event.Op == "issue.update" {
			break
		}
	}

	bob.ok("start", id)
	for _, args := range [][]string{
		{"recall"}, {"recall", "alice-only"}, {"recall", "--key", secretKey, "--scope", "user"}, {"recall", "--tag", "api", "--json"},
		{"memories"}, {"memories", "--scope", "user", "--json"}, {"show", id}, {"history", id, "--json"},
		{"digest", "--json"}, {"prime"}, {"prime", "--json"},
	} {
		leaks("bob's sfx "+strings.Join(args, " "), bob.ok(args...))
	}
	if got := bob.ok("recall", "--scope", "user"); !strings.Contains(got, "bob's own") {
		t.Errorf("bob's user scope lost his own memory:\n%s", got)
	}
	bob.ok("handoff", id, "back to you", "--release")
	leaks("bob's sfx start", bob.ok("start", id, "--take"))

	agent := bob.mcp("v0.2.0")
	for _, call := range []struct {
		tool string
		args map[string]any
	}{
		{"prime", nil}, {"recall", nil}, {"recall", map[string]any{"text": "alice-only"}}, {"recall", map[string]any{"scope": "user"}},
		{"history", map[string]any{"id": id}}, {"digest", nil},
	} {
		leaks("bob's agent's "+call.tool, agent.ok(call.tool, call.args))
	}

	// Alice still sees it, in her recall and her prime.
	if got := alice.ok("recall", "--scope", "user"); !strings.Contains(got, secretBody+", still") {
		t.Errorf("alice's own recall:\n%s", got)
	}
	if got := alice.ok("prime"); !strings.Contains(got, secretKey) {
		t.Errorf("alice's prime lacks her memory:\n%s", got)
	}
}

// An agent keeps, finds, replaces and forgets a memory over MCP, and a
// replace from a stale rev is refused with the next step in tools.
func TestMCPMemory(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	ag := alice.mcp("v0.2.0")
	if got := decode[proto.WriteResult](t, ag.ok("remember", map[string]any{"key": "build", "body": "run task check first",
		"tags": []any{"ci"}})); got.ID == "" || got.Rev != 1 {
		t.Fatalf("remember: %+v", got)
	}
	if got := ag.ok("recall", map[string]any{"tag": "ci"}); !strings.Contains(got, `"key":"build","scope":"project","body":"run task check first"`) {
		t.Fatalf("recall: %s", got)
	}
	bob.ok("remember", "build", "run task ci", "--rev", "1")
	out, isErr := ag.call("remember", map[string]any{"key": "build", "body": "run task check", "rev": 1})
	if !isErr || !strings.Contains(out, "changed since rev 1 (now rev 2 by bob)") ||
		!strings.Contains(out, "fix: call recall with the key and scope for its body and rev, merge your change into it, then remember again with that rev") {
		t.Fatalf("stale remember: %s", out)
	}
	ag.ok("forget", map[string]any{"key": "build", "rev": 2})
	if got := ag.ok("recall", nil); got != `{"memories":[]}` {
		t.Fatalf("recall after forget: %s", got)
	}
}
