package e2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// gitIn runs git in dir, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test fixture
		t.Fatalf("git %s: %v\n%s", args[0], err, out)
	}
}

// touch writes the files at the repo-relative paths given, under dir.
func touch(t *testing.T, dir string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		f := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(f), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte(p), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// filePaths returns the paths show lists for id, as the user sees them
// with --json.
func filePaths(t *testing.T, u *user, id string) []string {
	t.Helper()
	sh := decode[proto.ShowResult](t, u.ok("show", id, "--json"))
	if sh.Files == nil {
		return nil
	}
	var out []string
	for _, p := range sh.Files.Paths {
		out = append(out, p.Path)
	}
	return out
}

// Files to issues end to end: an agent starts an issue, changes files and
// renews; another principal's ready ranks down the issue that overlaps
// them and says why, and show names the holder. The agent's own session
// is not held back by its own claim. finish records the paths, and a
// person's `sfx away`, handoff and finish send theirs.
func TestFilesToIssues(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	gitInit(t, alice.repo)
	// The world's key and config are untracked in the repository; a real
	// one ignores the key.
	if err := os.WriteFile(filepath.Join(alice.repo, ".git", "info", "exclude"), []byte("id_ed25519\n.starfix.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alice.env["CLAUDE_CODE_SESSION_ID"] = "cc-files"
	held := strings.TrimSpace(alice.ok("create", "Record paths", "-t", "feature"))
	cand := strings.TrimSpace(bob.ok("create", "Refactor the store", "-p", "0", "--paths", "internal/store/,docs/x.md"))
	other := strings.TrimSpace(bob.ok("create", "Unrelated", "-p", "3"))

	ag := alice.mcp("v0.2.0")
	st := decode[mcpserver.Started](t, ag.ok("start", map[string]any{"id": held}))
	gitIn(t, alice.repo, "switch", "-q", "-c", st.Branch)
	touch(t, alice.repo, "internal/store/paths.go", "docs/cli.md")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ag.srv.Renew(ctx)

	ready := decode[proto.ListResult](t, bob.ok("ready", "--json"))
	var got []string
	for _, is := range ready.Issues {
		got = append(got, is.ID+strings.Join(is.Overlaps, ","))
	}
	if want := []string{other, cand + held}; !slices.Equal(got, want) {
		t.Fatalf("bob's ready = %v, want %v: the issue overlapping alice's work last, naming it", got, want)
	}
	if out := bob.ok("ready"); !strings.Contains(out, "overlaps "+held) {
		t.Errorf("bob's ready does not say why %s is last:\n%s", cand, out)
	}
	out := bob.ok("show", cand)
	for _, want := range []string{"likely files:\n  docs/x.md (declared)\n  internal/store/ (declared)\n",
		"overlaps " + held + ", held by alice (cc-files)"} {
		if !strings.Contains(out, want) {
			t.Errorf("bob's show %s lacks %q:\n%s", cand, want, out)
		}
	}
	// The agent's own claim does not hold it back.
	mine := decode[mcpserver.Issues](t, ag.ok("ready", nil))
	if len(mine.Issues) == 0 || mine.Issues[0].ID != cand || len(mine.Issues[0].Overlaps) != 0 {
		t.Errorf("alice's agent's ready = %+v, want %s first, with no overlaps", mine.Issues, cand)
	}

	touch(t, alice.repo, "README.md")
	ag.ok("finish", map[string]any{"id": held})
	if got, want := filePaths(t, bob, held), []string{"README.md", "docs/cli.md", "internal/store/paths.go"}; !slices.Equal(got, want) {
		t.Errorf("paths after finish = %q, want %q", got, want)
	}
	if out := bob.ok("ready"); strings.Contains(out, "overlaps") {
		t.Errorf("ready still names overlaps once the work is finished:\n%s", out)
	}

	// A person's own terminal: start two issues, change files for each,
	// go away. away sends each claim's paths, one issue per request.
	delete(alice.env, "CLAUDE_CODE_SESSION_ID")
	side := strings.TrimSpace(alice.ok("create", "Side work"))
	alice.ok("start", side)
	alice.ok("start", other, "--branch")
	touch(t, alice.repo, "side.go")
	gitIn(t, alice.repo, "add", "side.go")
	gitIn(t, alice.repo, "-c", "user.name=Alice", "-c", "user.email=alice@example.com",
		"commit", "-q", "-m", "side work\n\nStarfix: "+side)
	touch(t, alice.repo, "cmd/tool.go")
	alice.ok("away", "1h")
	if got := filePaths(t, bob, other); !slices.Contains(got, "cmd/tool.go") {
		t.Errorf("paths of %s after sfx away = %q, want cmd/tool.go among them", other, got)
	}
	if got := filePaths(t, bob, side); !slices.Equal(got, []string{"side.go"}) {
		t.Errorf("paths of %s after sfx away = %q, want side.go, from its trailer", side, got)
	}
	touch(t, alice.repo, "cmd/handoff.go")
	alice.ok("handoff", other, "halfway")
	if got := filePaths(t, bob, other); !slices.Contains(got, "cmd/handoff.go") {
		t.Errorf("paths after sfx handoff = %q, want cmd/handoff.go among them", got)
	}
	// A branch naming an id the server refuses costs the renewal nothing.
	gitIn(t, alice.repo, "switch", "-q", "-c", "feature/sf-"+strings.Repeat("a", 70))
	if out := alice.ok("away", "1h"); !strings.Contains(out, other+" held until") {
		t.Errorf("sfx away on a branch with a hostile id = %q, want %s renewed", out, other)
	}
	gitIn(t, alice.repo, "switch", "-q", "-")
	touch(t, alice.repo, "cmd/finish.go")
	alice.ok("finish", other)
	if got := filePaths(t, bob, other); !slices.Contains(got, "cmd/finish.go") {
		t.Errorf("paths after sfx finish = %q, want cmd/finish.go among them", got)
	}

	// --paths is relative to where sfx runs, like any path on a command
	// line.
	if err := os.MkdirAll(filepath.Join(alice.repo, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	alice.ok("-C", filepath.Join(alice.repo, "docs"), "update", cand, "--paths", "cli.md,./")
	if got, want := filePaths(t, bob, cand), []string{"docs/", "docs/cli.md"}; !slices.Equal(got, want) {
		t.Errorf("paths declared from docs/ = %q, want %q", got, want)
	}

	bob.ok("update", cand, "--paths", "")
	if got := filePaths(t, bob, cand); got != nil {
		t.Errorf("paths after clearing = %q, want none", got)
	}
}
