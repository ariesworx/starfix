package gitx

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBranchType(t *testing.T) {
	for typ, want := range map[string]string{
		"bug": "fix", "feature": "feature", "task": "maintenance", "chore": "maintenance",
		"docs": "docs", "epic": "feature", "": "feature", "story": "feature",
	} {
		if got := BranchType(typ); got != want {
			t.Errorf("BranchType(%q) = %q, want %q", typ, got, want)
		}
	}
}

func TestBranch(t *testing.T) {
	tests := []struct {
		typ, id, title, want string
	}{
		{"bug", "sf-a1b2c3d4", "Fix the login redirect", "fix/sf-a1b2c3d4-fix-the-login-redirect"},
		{"task", "sf-a1b2", "  Update   deps (Go 1.27)!  ", "maintenance/sf-a1b2-update-deps-go-1-27"},
		{"feature", "sf-a1b2.3", "Café au lait", "feature/sf-a1b2.3-caf-au-lait"},
		{"chore", "sf-a1b2", "!!!", "maintenance/sf-a1b2"},
		{"epic", "bd-x9", "Ship the whole thing to everyone everywhere at once", "feature/bd-x9-ship-the-whole-thing-to-everyone"},
		{"bug", "sf-a1b2", strings.Repeat("a", 60), "fix/sf-a1b2-" + strings.Repeat("a", 40)},
	}
	for _, tc := range tests {
		if got := Branch(tc.typ, tc.id, tc.title); got != tc.want {
			t.Errorf("Branch(%q, %q, %q) = %q, want %q", tc.typ, tc.id, tc.title, got, tc.want)
		}
		if s := Slug(tc.title); len(s) > maxSlug || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
			t.Errorf("Slug(%q) = %q", tc.title, s)
		}
	}
}

func TestIDFromBranch(t *testing.T) {
	tests := []struct {
		branch, prefix, want string
		ok                   bool
	}{
		{"fix/sf-a1b2c3d4-fix-the-login", "", "sf-a1b2c3d4", true},
		{"feature/sf-a1b2.3-caf-au-lait", "", "sf-a1b2.3", true},
		{"maintenance/sf-a1b2", "", "sf-a1b2", true},
		{"sf-a1b2-no-type", "", "sf-a1b2", true},
		{"feature/my-proj-a1b2-slug", "my-proj", "my-proj-a1b2", true},
		{"feature/other-a1b2-slug", "my-proj", "", false},
		{"main", "", "", false},
		{"feature/Upper-Case", "", "", false},
		{"feature/my-proj-", "my-proj", "", false},
	}
	for _, tc := range tests {
		got, ok := IDFromBranch(tc.branch, tc.prefix)
		if got != tc.want || ok != tc.ok {
			t.Errorf("IDFromBranch(%q, %q) = %q, %v; want %q, %v", tc.branch, tc.prefix, got, ok, tc.want, tc.ok)
		}
	}
	// A branch Branch made parses back to its ID.
	if got, ok := IDFromBranch(Branch("bug", "sf-a1b2c3d4", "Fix it"), ""); !ok || got != "sf-a1b2c3d4" {
		t.Errorf("round trip: %q", got)
	}
}

func TestIDFromMessage(t *testing.T) {
	tests := []struct {
		msg, want string
		ok        bool
	}{
		{"feature: add start\n\nWhy.\n\nStarfix: sf-a1b2c3d4\n", "sf-a1b2c3d4", true},
		{"subject\n\nStarfix: sf-old\nStarfix: sf-new\r\n", "sf-new", true},
		{"subject\n\nStarfix:sf-a1b2", "sf-a1b2", true},
		{"subject\n\nStarfix: not an id", "", false},
		{"subject\n\nstarfix: sf-a1b2", "", false},
		{"Starfix: sf-a1b2 trailing words", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := IDFromMessage(tc.msg)
		if got != tc.want || ok != tc.ok {
			t.Errorf("IDFromMessage(%q) = %q, %v; want %q, %v", tc.msg, got, ok, tc.want, tc.ok)
		}
	}
}

// newRepo makes a repository with one commit, isolated from the user's git
// configuration.
func newRepo(t *testing.T) string {
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
	} {
		if _, err := git(t.Context(), dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func current(t *testing.T, dir string) string {
	t.Helper()
	b, err := git(t.Context(), dir, "branch", "--show-current")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSwitch(t *testing.T) {
	dir := newRepo(t)
	const br = "fix/sf-a1b2-thing"
	for range 2 { // creates, then switches to the existing branch
		if err := Switch(t.Context(), dir, br); err != nil {
			t.Fatal(err)
		}
		if got := current(t, dir); got != br {
			t.Fatalf("on %q, want %q", got, br)
		}
		if _, err := git(t.Context(), dir, "switch", "-q", "main"); err != nil {
			t.Fatal(err)
		}
	}
	err := Switch(t.Context(), t.TempDir(), br)
	if err == nil || !strings.Contains(err.Error(), "git rev-parse") {
		t.Fatalf("outside a repository: %v", err)
	}
}

func TestAddWorktree(t *testing.T) {
	dir := newRepo(t)
	const br = "feature/sf-a1b2-thing"
	wt := filepath.Join(t.TempDir(), "wt")
	if err := AddWorktree(t.Context(), dir, wt, br); err != nil {
		t.Fatal(err)
	}
	if got := current(t, wt); got != br {
		t.Fatalf("worktree on %q, want %q", got, br)
	}
	// The branch now exists and is checked out there: git refuses a second
	// worktree on it, and says why.
	err := AddWorktree(t.Context(), dir, filepath.Join(t.TempDir(), "wt2"), br)
	if err == nil || !strings.HasPrefix(err.Error(), "git worktree: ") {
		t.Fatalf("second worktree: %v", err)
	}
	// An existing branch that is not checked out gets a worktree.
	if _, err := git(t.Context(), dir, "branch", "fix/sf-c3d4"); err != nil {
		t.Fatal(err)
	}
	wt3 := filepath.Join(t.TempDir(), "wt3")
	if err := AddWorktree(t.Context(), dir, wt3, "fix/sf-c3d4"); err != nil || current(t, wt3) != "fix/sf-c3d4" {
		t.Fatalf("existing branch: %v", err)
	}
}

// A failed git command's error carries git's own message, or the cause's
// when git printed none, and unwraps to the cause: the context's error, or
// git's exit status.
func TestGitErrors(t *testing.T) {
	dir := newRepo(t)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, cancel := context.WithTimeout(t.Context(), 0)
	defer cancel()
	tests := []struct {
		name string
		run  func() error
		is   error  // the cause errors.Is must find, or nil for an exit status
		exit int    // the exit status errors.As must find, when is is nil
		msg  string // the start of the message
	}{
		{name: "cancelled context", run: func() error { _, err := git(cancelled, dir, "status"); return err },
			is: context.Canceled, msg: "git status: context canceled"},
		{name: "expired context", run: func() error { _, err := git(expired, dir, "status"); return err },
			is: context.DeadlineExceeded, msg: "git status: context deadline exceeded"},
		{name: "git refuses", run: func() error { _, err := git(t.Context(), dir, "rev-parse", "--verify", "refs/heads/none"); return err },
			exit: 128, msg: "git rev-parse: fatal: "},
		{name: "switch outside a repository", run: func() error { return Switch(t.Context(), t.TempDir(), "fix/sf-a1b2") },
			exit: 128, msg: "git rev-parse: fatal: not a git repository"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil || !strings.HasPrefix(err.Error(), tc.msg) || strings.Contains(err.Error(), "exit status") {
				t.Errorf("error = %v, want git's message, starting %q", err, tc.msg)
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("errors.Is(%v, %v) = false, want true", err, tc.is)
			}
			if ee, ok := errors.AsType[*exec.ExitError](err); tc.is == nil && (!ok || ee.ExitCode() != tc.exit) {
				t.Errorf("error %v does not unwrap to git's exit status %d", err, tc.exit)
			}
		})
	}
}
