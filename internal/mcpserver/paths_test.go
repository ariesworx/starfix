package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
)

// gitCommit writes each file in dir and commits them all with msg.
func gitCommit(t *testing.T, dir, msg string, files ...string) {
	t.Helper()
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"add", "--all"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "-m", msg}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test fixture
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
	}
}

// gitRepo makes a repository on branch with one commit on main before it,
// and the files given left uncommitted, isolated from the user's git
// configuration.
func gitRepo(t *testing.T, branch string, files ...string) string {
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
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-q", "--allow-empty", "-m", "init"},
		{"switch", "-q", "-c", branch},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test fixture
			t.Fatalf("git %s: %v\n%s", args[0], err, out)
		}
	}
	for _, f := range files {
		p := filepath.Join(dir, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// clock is a settable time for the renewal throttle.
type clock struct {
	mu sync.Mutex
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

// renewPaths returns the paths each renew call carried, in order.
func renewPaths(f *fakeConn) []map[string][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string][]string
	for _, c := range f.calls {
		if c.op == proto.OpRenew {
			out = append(out, c.args.(proto.RenewArgs).Paths)
		}
	}
	return out
}

// workServer is an MCP server on the repository dir whose fake conn
// starts and holds every issue asked for, refusing a renew, finish or
// handoff that carries paths when refusePaths is set.
func workServer(t *testing.T, dir string, refusePaths bool) (*mcp.ClientSession, *Server, *fakeConn, *clock) {
	t.Helper()
	var mu sync.Mutex
	var held []proto.Claim
	refused := proto.Errf(proto.CodeInvalid, "correct it and retry", "path \"x\" is bad")
	f := &fakeConn{who: "alice", reply: func(op string, args any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		switch op {
		case proto.OpStart:
			id := args.(proto.StartArgs).ID
			held = append(held, proto.Claim{ID: id, Epoch: 1})
			return proto.StartResult{Issue: proto.Issue{ID: id, Type: "task"}, Claim: &proto.Claim{ID: id, Epoch: 1}}, nil
		case proto.OpRenew:
			if refusePaths && args.(proto.RenewArgs).Paths != nil {
				return nil, refused
			}
			return proto.ClaimsResult{Claims: held}, nil
		case proto.OpFinish:
			if refusePaths && args.(proto.FinishArgs).Paths != nil {
				return nil, refused
			}
			return proto.FinishResult{ID: args.(proto.FinishArgs).ID, Rev: 3}, nil
		case proto.OpHandoff:
			if refusePaths && args.(proto.HandoffArgs).Paths != nil {
				return nil, refused
			}
		}
		return proto.WriteResult{ID: "sf-a1b2", Rev: 2}, nil
	}}
	d := &dialer{conns: []*fakeConn{f}}
	clk := &clock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	s := New(Options{Version: "v0.2.0", Dial: d.dial, Dir: dir})
	s.now = clk.now
	t.Cleanup(func() { _ = s.Close() })
	st, ct := mcp.NewInMemoryTransports()
	if _, err := s.MCP().Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, s, f, clk
}

// Renew sends the paths of the issues the session holds, read from git,
// at most once every DefaultPathsEvery per issue.
func TestRenewSendsPaths(t *testing.T) {
	dir := gitRepo(t, "feature/sf-a1b2-thing", "a.go")
	cs, s, f, clk := workServer(t, dir, false)
	callTool(t, cs, "start", map[string]any{"id": "sf-a1b2"})

	s.Renew(t.Context())
	clk.add(time.Minute)
	s.Renew(t.Context())
	clk.add(DefaultPathsEvery)
	s.Renew(t.Context())

	want := []map[string][]string{{"sf-a1b2": {"a.go"}}, nil, {"sf-a1b2": {"a.go"}}}
	got := renewPaths(f)
	if len(got) != len(want) {
		t.Fatalf("renewals carried %v, want %v", got, want)
	}
	for i := range want {
		if !slices.Equal(got[i]["sf-a1b2"], want[i]["sf-a1b2"]) || (got[i] == nil) != (want[i] == nil) {
			t.Errorf("renewal %d carried %v, want %v", i+1, got[i], want[i])
		}
	}
}

// A server that refuses the paths does not cost the session its claims:
// the renewal goes again without them.
func TestRenewRefusedPaths(t *testing.T) {
	dir := gitRepo(t, "feature/sf-a1b2-thing", "a.go")
	cs, s, f, _ := workServer(t, dir, true)
	callTool(t, cs, "start", map[string]any{"id": "sf-a1b2"})
	s.Renew(t.Context())
	got := renewPaths(f)
	if len(got) != 2 || got[0] == nil || got[1] != nil {
		t.Fatalf("renewals carried %v, want paths, then none", got)
	}
	if s.claims.epoch("sf-a1b2") != 1 {
		t.Errorf("the session lost its claim on sf-a1b2 over refused paths")
	}
}

// sentPaths returns the paths each call of op carried, in order.
func sentPaths(f *fakeConn, op string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		switch a := c.args.(type) {
		case proto.HandoffArgs:
			if op == proto.OpHandoff {
				out = append(out, a.Paths)
			}
		case proto.FinishArgs:
			if op == proto.OpFinish {
				out = append(out, a.Paths)
			}
		}
	}
	return out
}

// finish and handoff carry the issue's paths, which the agent never
// gives: the tools' schemas have no such field. A server that refuses
// the paths gets the request again without them.
func TestFinishAndHandoffSendPaths(t *testing.T) {
	want := []string{"a.go", "b/c.go"}
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("refused %v", refuse), func(t *testing.T) {
			dir := gitRepo(t, "feature/sf-a1b2-thing", "a.go", "b/c.go")
			cs, _, f, _ := workServer(t, dir, refuse)
			for tool, args := range map[string]map[string]any{
				"handoff": {"id": "sf-a1b2", "note": "halfway"},
				"finish":  {"id": "sf-a1b2"},
			} {
				if res := callTool(t, cs, tool, args); res.IsError {
					t.Errorf("%s: %s", tool, text(t, res))
				}
			}
			for _, op := range []string{proto.OpHandoff, proto.OpFinish} {
				got := sentPaths(f, op)
				if len(got) == 0 || !slices.Equal(got[0], want) {
					t.Errorf("%s carried paths %q, want %q first", op, got, want)
				}
				if refuse && (len(got) != 2 || got[1] != nil) {
					t.Errorf("%s after its paths were refused carried %q, want a second call without paths", op, got)
				}
			}
		})
	}
	cs, _, _, _ := workServer(t, "", false)
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		b, _ := json.Marshal(tl.InputSchema)
		if (tl.Name == "finish" || tl.Name == "handoff") && strings.Contains(string(b), "paths") {
			t.Errorf("%s's schema names paths: %s", tl.Name, b)
		}
	}
}

// A renewal carries paths for at most proto.MaxPathIssues issues, which
// the server accepts; the rest go with the next.
func TestRenewPathIssuesCap(t *testing.T) {
	dir := gitRepo(t, "feature/refactor")
	cs, s, f, clk := workServer(t, dir, false)
	n := proto.MaxPathIssues + 1
	for i := range n {
		id := fmt.Sprintf("sf-a%03d", i)
		gitCommit(t, dir, "work\n\nStarfix: "+id, id+".go")
		callTool(t, cs, "start", map[string]any{"id": id})
	}
	s.Renew(t.Context())
	clk.add(time.Minute)
	s.Renew(t.Context())
	if got, want := issueCounts(renewPaths(f)), []int{proto.MaxPathIssues, n - proto.MaxPathIssues}; !slices.Equal(got, want) {
		t.Errorf("renewals carried paths for %v issues, want %v", got, want)
	}
}

// issueCounts is how many issues each renewal carried paths for.
func issueCounts(rs []map[string][]string) []int {
	out := make([]int, len(rs))
	for i, r := range rs {
		out[i] = len(r)
	}
	return out
}

// show lists the likely files and the work others hold that overlaps
// them; ready names the overlaps.
func TestShowAndReadyFiles(t *testing.T) {
	f := &fakeConn{who: "bob", reply: func(op string, _ any) (any, error) {
		switch op {
		case proto.OpShow:
			return proto.ShowResult{Issue: proto.Issue{ID: "sf-c3d4", Title: "t", Status: "open", Type: "task"},
				Files: &proto.Files{Paths: []proto.FilePath{{Path: "internal/", Source: proto.PathDeclared},
					{Path: "a.go", Source: proto.PathCommit}}, More: 3,
					Overlaps: []proto.Overlap{{ID: "sf-a1b2", By: "alice", Session: "s1"}}}}, nil
		case proto.OpReady:
			return proto.ListResult{Issues: []proto.Summary{{ID: "sf-c3d4", Title: "t", Status: "open",
				Overlaps: []string{"sf-a1b2"}}}}, nil
		}
		return nil, nil
	}}
	cs, _ := connect(t, f)
	var is Issue
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "show", map[string]any{"id": "sf-c3d4"}))), &is); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(is.Files, []string{"internal/", "a.go"}) || is.FilesMore != 3 ||
		!slices.Equal(is.Overlaps, []string{"sf-a1b2 by alice/s1"}) {
		t.Errorf("show files %q (+%d), overlaps %q; want the two paths, 3 more, and sf-a1b2 by alice/s1", is.Files, is.FilesMore, is.Overlaps)
	}
	if got := text(t, callTool(t, cs, "ready", nil)); !strings.Contains(got, `"overlaps":["sf-a1b2"]`) {
		t.Errorf("ready = %s, want sf-c3d4's overlaps", got)
	}
}
