// Package gitx is starfix's git awareness, on the client only: the branch
// name an issue suggests ([Branch]), the issue a branch or commit message
// names ([IDFromBranch], [IDFromMessage]), and creating that branch or a
// worktree on it ([Switch], [AddWorktree]). The design is
// docs/design/starfix.md §12 item 2, and the branch scheme is in §5, "As
// built (stage 2, first slice)".
//
// It installs no hooks. Switch and AddWorktree run the git on PATH with
// explicit arguments, never through a shell, and kill it if their context
// ends first. Their errors name the git command that failed, carry git's
// own message, and unwrap to the cause, such as git's *exec.ExitError or,
// when the context ended before git started, the context's error.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// branchTypes maps an issue type to its branch prefix. The prefixes are
// the starfix repository's own branch types: feature/, fix/, maintenance/
// and docs/.
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
// single hyphens, at most 40 characters. A longer slug is cut to 40, then
// back to its last word boundary if that keeps more than 20.
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
	// trailer is a "Starfix: <id>" line, capturing the id.
	trailer = regexp.MustCompile(`^Starfix:[ \t]*(\S+)[ \t]*$`)
)

// IDFromBranch returns the issue ID at the start of a branch's last
// segment, as Branch writes it, and whether there is one. A branch name
// cannot mark where a hyphenated ID prefix ends, so prefix names the
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

// IDFromMessage returns the issue ID a commit message names in a
// "Starfix: <id>" trailer, and whether it names one. The last such line
// wins; a value not shaped like an issue ID is skipped.
func IDFromMessage(msg string) (string, bool) {
	lines := strings.Split(strings.TrimRight(msg, "\n"), "\n")
	for _, line := range slices.Backward(lines) {
		if m := trailer.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil && idShape.MatchString(m[1]) {
			return m[1], true
		}
	}
	return "", false
}

// gitError is a failed git command: msg names the command and carries
// git's own message, and err is the cause, such as an *exec.ExitError or
// the context's error.
type gitError struct {
	msg string
	err error
}

func (e *gitError) Error() string { return e.msg }

func (e *gitError) Unwrap() error { return e.err }

// git runs git in dir and returns its trimmed standard output. A failure
// carries git's own message, and unwraps to the cause.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // fixed binary, explicit argv, no shell
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", &gitError{msg: fmt.Sprintf("git %s: %s", args[0], msg), err: err}
	}
	return strings.TrimSpace(out.String()), nil
}

// hasBranch reports whether the local branch exists. git exits 1 when it
// does not; any other failure is an error.
func hasBranch(ctx context.Context, dir, branch string) (bool, error) {
	_, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	var ee *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return false, nil
	}
	return false, err
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

// AddWorktree creates a worktree at path, for the repository at dir, on
// branch, creating the branch from HEAD if it does not exist. A relative
// path is relative to dir. git refuses a branch that another worktree has
// checked out.
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
