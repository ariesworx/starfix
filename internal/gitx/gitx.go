// Package gitx is starfix's git awareness, on the client only (design §12
// item 2): the branch name an issue suggests, the issue a branch or commit
// names, and creating that branch or a worktree for it. It installs no
// hooks. git runs with explicit arguments, never through a shell.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// branchTypes maps an issue type to its branch prefix. The prefixes are the
// repository's own: feature/, fix/, maintenance/, docs/.
var branchTypes = map[string]string{
	"bug":     "fix",
	"feature": "feature",
	"task":    "maintenance",
	"chore":   "maintenance",
	"docs":    "docs",
}

// BranchType returns the branch prefix for an issue type: feature for any
// type without one of its own.
func BranchType(issueType string) string {
	if t, ok := branchTypes[issueType]; ok {
		return t
	}
	return "feature"
}

// maxSlug bounds the slug part of a branch name.
const maxSlug = 40

// Slug turns a title into lowercase ASCII letters and digits joined by
// single hyphens, at most 40 characters, cut at a word boundary where
// there is one.
func Slug(title string) string {
	var b strings.Builder
	gap := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if gap && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			gap = false
			continue
		}
		gap = true
	}
	s := b.String()
	if len(s) <= maxSlug {
		return s
	}
	s = s[:maxSlug]
	if i := strings.LastIndexByte(s, '-'); i > maxSlug/2 {
		s = s[:i]
	}
	return strings.TrimSuffix(s, "-")
}

// Branch is the branch an issue suggests: <type>/<id>-<slug>, or
// <type>/<id> when the title has nothing to slug.
func Branch(issueType, id, title string) string {
	b := BranchType(issueType) + "/" + id
	if s := Slug(title); s != "" {
		b += "-" + s
	}
	return b
}

var (
	// idShape is an issue ID: a prefix, a hyphen, a suffix and optional
	// .N child parts. It matches the server's rule loosely; the server is
	// the judge.
	idShape = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*-[a-z0-9]+(\.[0-9]+)*$`)
	// branchID is the ID at the start of a branch's last segment, with a
	// one-word prefix: sf-a1b2 or sf-a1b2.1.
	branchID = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9]+(\.[0-9]+)*`)
	trailer  = regexp.MustCompile(`^Starfix:[ \t]*(\S+)[ \t]*$`)
)

// IDFromBranch returns the issue ID a branch made by Branch names. Branch
// names cannot mark where a hyphenated prefix ends, so prefix names the
// project's prefix when it has a hyphen ("" assumes it has none).
func IDFromBranch(branch, prefix string) (string, bool) {
	seg := branch[strings.LastIndexByte(branch, '/')+1:]
	if prefix != "" {
		if !strings.HasPrefix(seg, prefix+"-") {
			return "", false
		}
		rest := branchID.FindString("x-" + seg[len(prefix)+1:])
		if rest == "" {
			return "", false
		}
		return prefix + rest[1:], true
	}
	id := branchID.FindString(seg)
	return id, id != ""
}

// IDFromMessage returns the issue a commit message names in a
// "Starfix: <id>" trailer; the last one wins.
func IDFromMessage(msg string) (string, bool) {
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if m := trailer.FindStringSubmatch(strings.TrimRight(lines[i], "\r")); m != nil && idShape.MatchString(m[1]) {
			return m[1], true
		}
	}
	return "", false
}

// git runs git in dir and returns its trimmed standard output. A failure
// carries git's own message.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed binary, explicit argv, no shell
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", args[0], msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// hasBranch reports whether the local branch exists.
func hasBranch(ctx context.Context, dir, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch) //nolint:gosec // fixed binary, explicit argv, no shell
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	}
	return false, fmt.Errorf("git rev-parse: %w", err)
}

// Switch checks out branch in the repository at dir, creating it from HEAD
// if it does not exist. It is safe to repeat.
func Switch(ctx context.Context, dir, branch string) error {
	ok, err := hasBranch(ctx, dir, branch)
	if err != nil {
		return err
	}
	args := []string{"switch", branch}
	if !ok {
		args = []string{"switch", "-c", branch}
	}
	_, err = git(ctx, dir, args...)
	return err
}

// AddWorktree creates a worktree at path on branch, creating the branch
// from HEAD if it does not exist.
func AddWorktree(ctx context.Context, dir, path, branch string) error {
	if strings.HasPrefix(path, "-") {
		path = "./" + path // never an option
	}
	ok, err := hasBranch(ctx, dir, branch)
	if err != nil {
		return err
	}
	args := []string{"worktree", "add", path, branch}
	if !ok {
		args = []string{"worktree", "add", "-b", branch, path}
	}
	_, err = git(ctx, dir, args...)
	return err
}
