package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// gitInit makes dir a repository with one commit, isolated from the
// user's git configuration.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", empty)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test fixture
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
	}
}

func gitBranch(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "branch", "--show-current").Output() //nolint:gosec // test fixture
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// One issue from start to finish on the CLI, passed between two people
// with a handoff, with its branch and worktree.
func TestStartHandoffFinish(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	gitInit(t, alice.repo)
	gitInit(t, bob.repo)
	id := strings.TrimSpace(alice.ok("create", "Fix the login redirect", "-t", "bug", "-p", "1", "--acceptance", "lands on /home"))
	alice.ok("create", "Later", "-p", "3")

	out := alice.ok("start", "--branch")
	branch := "fix/" + id + "-fix-the-login-redirect"
	for _, want := range []string{id + "  P1  in_progress  bug  rev 2", "assignee: alice", "acceptance:\nlands on /home",
		"branch: " + branch + " (checked out)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("start lacks %q:\n%s", want, out)
		}
	}
	if got := gitBranch(t, alice.repo); got != branch {
		t.Fatalf("on branch %q, want %q", got, branch)
	}
	// Again: nothing changes on the server, and the existing branch is kept.
	again := decode[struct {
		Issue  proto.Issue `json:"issue"`
		Branch string      `json:"branch"`
	}](t, alice.ok("start", id, "--branch", "--json"))
	if again.Issue.Rev != 2 || again.Branch != branch || gitBranch(t, alice.repo) != branch {
		t.Fatalf("start again: %+v", again)
	}

	r := bob.run("v0.2.0", "start", id)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "sfx: "+id+" is in progress by alice; next ready: ") ||
		!strings.Contains(r.stderr, "fix: take that one with `sfx start sf-") {
		t.Fatalf("bob start: exit %d\n%s", r.code, r.stderr)
	}

	if got := alice.ok("handoff", id, "--release", "-"); got != id+" rev 3\n" {
		t.Fatalf("handoff: %q", got)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	out = bob.ok("start", id, "--worktree", wt)
	if !strings.Contains(out, "handoff from alice, ") || !strings.Contains(out, "\nfrom stdin\n") ||
		!strings.Contains(out, "branch: "+branch+" (worktree "+wt+")") {
		t.Fatalf("bob start:\n%s", out)
	}
	if got := gitBranch(t, wt); got != branch {
		t.Fatalf("worktree on %q", got)
	}
	if out := bob.ok("comments", id); !strings.Contains(out, "alice  ") || !strings.Contains(out, "(handoff)\nfrom stdin") {
		t.Fatalf("comments:\n%s", out)
	}

	r = alice.run("v0.2.0", "finish", id)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "fix: leave it to bob") {
		t.Fatalf("alice finish: exit %d\n%s", r.code, r.stderr)
	}
	f := decode[proto.FinishResult](t, bob.ok("finish", id, "--reason", "fixed", "--handoff", "cookie path is gone",
		"--discovered", "Flaky login test, sometimes", "--discovered", "Document the redirect", "--json"))
	if f.ID != id || len(f.Created) != 2 {
		t.Fatalf("finish: %+v", f)
	}
	show := decode[proto.ShowResult](t, bob.ok("show", f.Created[0], "--json"))
	if show.Issue.Title != "Flaky login test, sometimes" || len(show.Deps) != 1 || show.Deps[0].To != id ||
		show.Deps[0].Type != "discovered-from" {
		t.Fatalf("discovered: %+v", show)
	}
	if s := decode[proto.ShowResult](t, bob.ok("show", id, "--json")); s.Issue.Status != "closed" || s.Issue.CloseReason != "fixed" {
		t.Fatalf("finished issue: %+v", s.Issue)
	}
	r = bob.run("v0.2.0", "finish", id)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "is already closed\nfix: nothing to do") {
		t.Fatalf("finish twice: exit %d\n%s", r.code, r.stderr)
	}

	h := decode[proto.HistoryResult](t, bob.ok("history", id, "--json"))
	var ops []string
	for _, e := range h.Events {
		ops = append(ops, e.Op+"/"+e.Principal)
	}
	want := "issue.create/alice issue.update/alice comment.add/alice issue.update/alice issue.update/bob comment.add/bob issue.close/bob"
	if got := strings.Join(ops, " "); got != want {
		t.Fatalf("history:\n got %s\nwant %s", got, want)
	}
	// A dependency's event is recorded on the issue that depends.
	if out := bob.ok("history", f.Created[1]); !strings.Contains(out, "issue.create") || !strings.Contains(out, "dep.add") {
		t.Fatalf("discovered history:\n%s", out)
	}
}

// The two-call flow over MCP: start, then finish.
func TestMCPStartFinish(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice := w.newUser("alice", "")
	ag := alice.mcp("v0.2.0")
	out, isErr := ag.call("start", nil)
	if !isErr || out != "not_found: nothing is ready to start\nfix: nothing is ready: call blocked to see why, or create an issue" {
		t.Fatalf("start with nothing ready: %q", out)
	}
	id := strings.TrimSpace(alice.ok("create", "Add the start tool", "-t", "feature", "--acceptance", "two calls"))

	st := decode[mcpserver.Started](t, ag.ok("start", nil))
	if st.ID != id || st.Rev != 2 || st.Acceptance != "two calls" || st.Branch != "feature/"+id+"-add-the-start-tool" || st.Handoff != nil {
		t.Fatalf("start: %+v", st)
	}
	if again := decode[mcpserver.Started](t, ag.ok("start", map[string]any{"id": id})); again.Rev != 2 {
		t.Fatalf("start again: %+v", again)
	}
	if out := ag.ok("handoff", map[string]any{"id": id, "note": "schema done"}); out != `{"id":"`+id+`","rev":2}` {
		t.Fatalf("handoff: %s", out)
	}
	out = ag.ok("finish", map[string]any{"id": id, "reason": "done", "handoff": "see the README",
		"discovered": []any{map[string]any{"title": "Add digest", "priority": 1}}})
	f := decode[proto.FinishResult](t, out)
	if f.ID != id || f.Rev != 3 || len(f.Created) != 1 {
		t.Fatalf("finish: %s", out)
	}
	ready := decode[mcpserver.Issues](t, ag.ok("ready", nil))
	if len(ready.Issues) != 1 || ready.Issues[0].ID != f.Created[0] || ready.Issues[0].Priority != 1 {
		t.Fatalf("ready after finish: %+v", ready)
	}
	cs := decode[mcpserver.Comments](t, ag.ok("comments", map[string]any{"id": id}))
	if len(cs.Comments) != 2 || cs.Comments[1].Kind != "handoff" || cs.Comments[1].Body != "see the README" {
		t.Fatalf("comments: %+v", cs)
	}
}
