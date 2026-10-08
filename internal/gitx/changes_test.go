package gitx

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestBranchFor(t *testing.T) {
	tests := []struct {
		branch, id string
		want       bool
	}{
		{"feature/sf-a1b2-add-paths", "sf-a1b2", true},
		{"fix/sf-a1b2", "sf-a1b2", true},
		{"sf-a1b2-no-type", "sf-a1b2", true},
		{"feature/my-proj-a1b2-slug", "my-proj-a1b2", true},
		{"feature/sf-a1b2.1-child", "sf-a1b2.1", true},
		{"feature/sf-a1b2.1-child", "sf-a1b2", false},
		{"feature/sf-a1b2c3-other", "sf-a1b2", false},
		{"feature/sf-a1-slug", "sf-a1b2", false},
		{"main", "sf-a1b2", false},
		{"feature/sf-a1b2-x", "", false},
	}
	for _, tc := range tests {
		if got := BranchFor(tc.branch, tc.id); got != tc.want {
			t.Errorf("BranchFor(%q, %q) = %v, want %v", tc.branch, tc.id, got, tc.want)
		}
	}
}

// repoGit runs git in dir as the test user, failing the test on error.
func repoGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(t.Context(), dir, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// write creates or overwrites the file at rel, under dir, with body.
func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// commit writes each file and commits them all with msg.
func commit(t *testing.T, dir, msg string, files ...string) {
	t.Helper()
	for _, f := range files {
		write(t, dir, f, msg)
	}
	repoGit(t, dir, "add", "--all")
	repoGit(t, dir, "commit", "-q", "-m", msg)
}

// A branch named for an issue gives it every commit since the default
// branch, newest first, then its uncommitted files ahead of them; a
// trailer gives a commit to the issue it names, on any branch.
func TestReadChanges(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, "on main, before the branch", "main.go")
	repoGit(t, dir, "switch", "-q", "-c", "feature/sf-a1b2-paths")
	commit(t, dir, "first", "docs/design notes/a b.md", "internal/x.go")
	commit(t, dir, "second\n\nStarfix: sf-c3d4\n", "café/naïve.go", "internal/x.go")
	commit(t, dir, "third", "internal/y.go")
	write(t, dir, ".gitignore", "*.log\n")
	repoGit(t, dir, "add", ".gitignore")
	write(t, dir, "staged.go", "s")
	repoGit(t, dir, "add", "staged.go")
	write(t, dir, "internal/y.go", "changed, unstaged")
	write(t, dir, "new dir/untracked.go", "u")
	write(t, dir, "debug.log", "ignored")

	ch, err := ReadChanges(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Branch != "feature/sf-a1b2-paths" {
		t.Errorf("Branch = %q, want feature/sf-a1b2-paths", ch.Branch)
	}
	tests := []struct {
		id   string
		want []string
	}{
		{"sf-a1b2", []string{".gitignore", "internal/y.go", "new dir/untracked.go", "staged.go",
			"café/naïve.go", "internal/x.go", "docs/design notes/a b.md"}},
		{"sf-c3d4", []string{"café/naïve.go", "internal/x.go"}},
		{"sf-e5f6", nil},
	}
	for _, tc := range tests {
		if got := ch.Paths(tc.id); !slices.Equal(got, tc.want) {
			t.Errorf("Paths(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// Off an issue's branch, its trailer commits still count, and uncommitted
// files belong to nobody.
func TestReadChangesTrailerOnly(t *testing.T) {
	dir := newRepo(t)
	repoGit(t, dir, "switch", "-q", "-c", "feature/refactor")
	commit(t, dir, "part of it\n\nStarfix: sf-a1b2", "a.go")
	commit(t, dir, "unrelated", "b.go")
	write(t, dir, "c.go", "uncommitted")

	ch, err := ReadChanges(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ch.Paths("sf-a1b2"), []string{"a.go"}; !slices.Equal(got, want) {
		t.Errorf("Paths(sf-a1b2) = %q, want %q", got, want)
	}
}

// The default branch is origin's HEAD when there is one, so commits on it
// never count, whatever it is called.
func TestReadChangesOriginHead(t *testing.T) {
	upstream := newRepo(t)
	repoGit(t, upstream, "branch", "-q", "-m", "trunk")
	commit(t, upstream, "on trunk", "trunk.go")
	dir := filepath.Join(t.TempDir(), "clone")
	repoGit(t, upstream, "clone", "-q", upstream, dir)
	repoGit(t, dir, "switch", "-q", "-c", "fix/sf-a1b2")
	commit(t, dir, "fix", "fix.go")

	ch, err := ReadChanges(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ch.Paths("sf-a1b2"), []string{"fix.go"}; !slices.Equal(got, want) {
		t.Errorf("Paths(sf-a1b2) = %q, want %q", got, want)
	}
}

// Commits that reached the branch by a merge, or that the default branch
// had when the branch left it, are not the branch's work: the log follows
// first parents only, and the default branch is origin's before a local
// copy that may be stale.
func TestReadChangesMerged(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"merged another branch", func(t *testing.T) string {
			dir := newRepo(t)
			repoGit(t, dir, "switch", "-q", "-c", "other")
			commit(t, dir, "other work", "other.go")
			repoGit(t, dir, "switch", "-q", "main")
			repoGit(t, dir, "switch", "-q", "-c", "feature/sf-a1b2")
			commit(t, dir, "mine", "mine.go")
			repoGit(t, dir, "merge", "-q", "--no-edit", "other")
			return dir
		}},
		{"left a newer origin/main than the local main", func(t *testing.T) string {
			upstream := newRepo(t)
			dir := filepath.Join(t.TempDir(), "clone")
			repoGit(t, upstream, "clone", "-q", upstream, dir)
			commit(t, upstream, "main moved on", "main.go")
			repoGit(t, dir, "fetch", "-q")
			repoGit(t, dir, "remote", "set-head", "origin", "-d")
			repoGit(t, dir, "switch", "-q", "-c", "feature/sf-a1b2", "origin/main")
			commit(t, dir, "mine", "mine.go")
			return dir
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ch, err := ReadChanges(t.Context(), tc.setup(t))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := ch.Paths("sf-a1b2"), []string{"mine.go"}; !slices.Equal(got, want) {
				t.Errorf("Paths(sf-a1b2) = %q, want %q", got, want)
			}
		})
	}
}

// IssuePaths gives each issue the paths of its work that the server
// accepts, and nothing when git tells nothing.
func TestIssuePaths(t *testing.T) {
	long := strings.Repeat("d/", proto.MaxPathLen/2) + "x.go"
	dir := newRepo(t)
	repoGit(t, dir, "switch", "-q", "-c", "feature/sf-a1b2-thing")
	for _, f := range []string{"a b.go", "dir/c.go", long} {
		write(t, dir, f, "x")
	}
	tests := []struct {
		name string
		dir  string
		ids  []string
		want map[string][]string
	}{
		{"the branch's issue", dir, []string{"sf-a1b2"}, map[string][]string{"sf-a1b2": {"a b.go", "dir/c.go"}}},
		{"another issue", dir, []string{"sf-zzzz"}, nil},
		{"not a repository", t.TempDir(), []string{"sf-a1b2"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IssuePaths(t.Context(), tc.dir, tc.ids)
			if len(got) != len(tc.want) {
				t.Fatalf("IssuePaths(%v) = %q, want %q", tc.ids, got, tc.want)
			}
			for id, ps := range tc.want {
				if !slices.Equal(got[id], ps) {
					t.Errorf("IssuePaths(%v)[%s] = %q, want %q (a path the server refuses is left out)", tc.ids, id, got[id], ps)
				}
			}
		})
	}
}

// RepoRelative turns paths given in a directory into the repository's
// form, relative to its root.
func TestRepoRelative(t *testing.T) {
	dir := newRepo(t)
	sub := filepath.Join(dir, "internal", "store")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		dir  string
		in   []string
		want []string
	}{
		{"at the root", dir, []string{"a.go", "./docs/"}, []string{"a.go", "docs/"}},
		{"in a subdirectory", sub, []string{"paths.go", "./", "../cli/"},
			[]string{"internal/store/paths.go", "internal/store/", "internal/cli/"}},
		{"absolute, which the server refuses, saying why", sub, []string{"/etc/passwd"}, []string{"/etc/passwd"}},
		{"outside a repository, as given", t.TempDir(), []string{"./x.go"}, []string{"x.go"}},
		{"none, to clear", sub, nil, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RepoRelative(t.Context(), tc.dir, tc.in); !slices.Equal(got, tc.want) || got == nil {
				t.Errorf("RepoRelative(%q) = %q, want %q (never nil)", tc.in, got, tc.want)
			}
		})
	}
}

// Without a default branch to measure from, only uncommitted files count;
// with no commit at all, the same.
func TestReadChangesNoBase(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"no default branch", func(t *testing.T) string {
			dir := newRepo(t)
			repoGit(t, dir, "branch", "-q", "-m", "dev")
			repoGit(t, dir, "switch", "-q", "-c", "feature/sf-a1b2")
			commit(t, dir, "committed", "old.go")
			return dir
		}},
		{"unborn branch", func(t *testing.T) string {
			dir := newRepo(t)
			repoGit(t, dir, "switch", "-q", "--orphan", "feature/sf-a1b2")
			return dir
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t)
			write(t, dir, "new.go", "n")
			ch, err := ReadChanges(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := ch.Paths("sf-a1b2"), []string{"new.go"}; !slices.Equal(got, want) {
				t.Errorf("Paths(sf-a1b2) = %q, want %q", got, want)
			}
		})
	}
}

// A detached HEAD, a directory outside any repository and a machine
// without git all give no changes; the last two say why.
func TestReadChangesNothing(t *testing.T) {
	dir := newRepo(t)
	repoGit(t, dir, "switch", "-q", "-c", "feature/sf-a1b2")
	commit(t, dir, "work", "a.go")
	repoGit(t, dir, "switch", "-q", "--detach")
	write(t, dir, "b.go", "uncommitted")
	ch, err := ReadChanges(t.Context(), dir)
	if err != nil || ch.Branch != "" || len(ch.Paths("sf-a1b2")) != 0 {
		t.Errorf("detached HEAD: %+v, %v; want no changes and no error", ch, err)
	}

	if ch, err := ReadChanges(t.Context(), t.TempDir()); err == nil || ch.Branch != "" {
		t.Errorf("outside a repository: %+v, %v; want no changes and an error", ch, err)
	}

	t.Setenv("PATH", t.TempDir())
	if ch, err := ReadChanges(t.Context(), dir); err == nil || ch.Branch != "" {
		t.Errorf("without git: %+v, %v; want no changes and an error", ch, err)
	}
}

// FuzzParseGit checks that the git output parsers take any input without
// panicking, and never return an empty path.
func FuzzParseGit(f *testing.F) {
	f.Add("\x00\x00h\x00msg\n\x00\na.go\x00b c.go\x00\x00\x00h2\x00\x00\x00")
	f.Add(" M a.go\x00?? new dir/b.go\x00A  c.go\x00")
	f.Add("\x00\x00")
	f.Fuzz(func(t *testing.T, out string) {
		for _, c := range parseLog(out) {
			for _, p := range c.Paths {
				if p == "" {
					t.Fatalf("parseLog(%q) gave an empty path", out)
				}
			}
		}
		for _, p := range parseStatus(out) {
			if p == "" {
				t.Fatalf("parseStatus(%q) gave an empty path", out)
			}
		}
	})
}
