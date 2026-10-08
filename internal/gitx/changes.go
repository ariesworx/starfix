package gitx

import (
	"context"
	"errors"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Files to issues (design §12 item 3): the paths an issue's work touched,
// read from the repository, for the client to send to the server. No hook
// records them; ReadChanges looks when asked.

// Changes are what the checked-out branch of a repository has changed.
type Changes struct {
	// Branch is the checked-out branch; empty for a detached HEAD.
	Branch string
	// Commits are the commits on HEAD's first-parent line since it left
	// the default branch, newest first, at most maxCommits.
	Commits []Commit
	// Uncommitted are the files staged, changed or untracked and not
	// ignored, in path order.
	Uncommitted []string
}

// Commit is one commit: the issue its Starfix: trailer names, if any, and
// the paths it touched, in git's order.
type Commit struct {
	Trailer string
	Paths   []string
}

// maxCommits bounds the commits ReadChanges reads.
const maxCommits = 500

// BranchFor reports whether branch is the issue id's, as Branch names it:
// its last segment is id, or id and a hyphen and more. Unlike
// IDFromBranch it needs no project prefix, since it knows the id.
func BranchFor(branch, id string) bool {
	if id == "" {
		return false
	}
	seg := branch[strings.LastIndexByte(branch, '/')+1:]
	return seg == id || strings.HasPrefix(seg, id+"-")
}

// Paths returns the paths the work on the issue id touched, most recent
// first, without repeats: when the branch is id's (BranchFor), the
// uncommitted files and then every commit's; otherwise the paths of the
// commits whose trailer names id. Paths are relative to the repository's
// root, with forward slashes, as git gives them; the caller checks them.
func (c Changes) Paths(id string) []string {
	mine := BranchFor(c.Branch, id)
	seen := map[string]bool{}
	var out []string
	add := func(ps []string) {
		for _, p := range ps {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if mine {
		add(c.Uncommitted)
	}
	for _, cm := range c.Commits {
		if mine || cm.Trailer == id {
			add(cm.Paths)
		}
	}
	return out
}

// gitTimeout bounds one look at the repository by IssuePaths, so a slow
// one never holds up a renewal or a finish.
const gitTimeout = 5 * time.Second

// IssuePaths returns the paths the work on each issue in ids touched, in
// the repository holding dir (ReadChanges), as sfx sends them to the
// server: only paths it accepts (proto.CheckPath, and no directory
// prefix), most recent first, at most proto.MaxPaths in all, filled in
// ids' order. Issues with none are left out. It returns nil when git
// tells nothing: no git, no repository, a detached HEAD, or a look that
// takes longer than 5 seconds. Paths are a hint, so it never fails.
func IssuePaths(ctx context.Context, dir string, ids []string) map[string][]string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	ch, err := ReadChanges(ctx, dir)
	if err != nil {
		return nil
	}
	var out map[string][]string
	budget := proto.MaxPaths
	for _, id := range ids {
		var ps []string
		for _, p := range ch.Paths(id) {
			if len(ps) == budget {
				break
			}
			if proto.CheckPath(p) == nil && !proto.IsPathPrefix(p) {
				ps = append(ps, p)
			}
		}
		if len(ps) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]string{}
		}
		out[id] = ps
		budget -= len(ps)
	}
	return out
}

// RepoRelative returns the paths ps, given relative to dir, relative to
// the root of the repository holding dir instead, with forward slashes,
// cleaned, keeping a trailing "/". An absolute path is left as it is, for
// the server to refuse, and outside a repository each is only cleaned. It
// never returns nil, so an empty list still says "none".
func RepoRelative(ctx context.Context, dir string, ps []string) []string {
	prefix := ""
	if len(ps) > 0 {
		prefix, _ = gitRaw(ctx, dir, "rev-parse", "--show-prefix")
		prefix = strings.TrimSuffix(prefix, "\n")
	}
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		p = filepath.ToSlash(p)
		if path.IsAbs(p) {
			out = append(out, p)
			continue
		}
		rel := path.Join(prefix, p)
		if strings.HasSuffix(p, "/") {
			rel += "/"
		}
		out = append(out, rel)
	}
	return out
}

// hexHash is a commit hash as git prints it.
var hexHash = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// ReadChanges reads the changes of the repository holding dir: the
// checked-out branch, its commits since the default branch, following
// first parents only, so commits a merge brought in are not the branch's,
// and its uncommitted files. A detached HEAD has no changes. The default
// branch is origin's HEAD, else main or master (origin's, then local,
// which may be stale), else the branch's upstream; with none of them,
// there are no commits, only uncommitted files. An error, such as no git or no repository, comes with
// no changes; callers that only want what there is treat it as none.
//
// It runs git with fixed arguments and without optional locks, so it
// never contends with the person's own git commands.
func ReadChanges(ctx context.Context, dir string) (Changes, error) {
	top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return Changes{}, err
	}
	branch, err := git(ctx, top, "symbolic-ref", "--quiet", "--short", "HEAD")
	if ee, ok := errors.AsType[*exec.ExitError](err); ok && ee.ExitCode() == 1 {
		return Changes{}, nil // detached
	}
	if err != nil {
		return Changes{}, err
	}
	ch := Changes{Branch: branch}
	if ch.Uncommitted, err = uncommitted(ctx, top); err != nil {
		return Changes{}, err
	}
	base, err := defaultBase(ctx, top)
	if err != nil || base == "" {
		return ch, err
	}
	if ch.Commits, err = commitsSince(ctx, top, base); err != nil {
		return Changes{}, err
	}
	return ch, nil
}

// defaultBase returns the merge base of HEAD and the default branch, or ""
// when HEAD has no commit or there is no default branch to measure from.
func defaultBase(ctx context.Context, top string) (string, error) {
	if _, err := git(ctx, top, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		return "", nil // an unborn branch: nothing committed yet
	}
	for _, ref := range []string{"refs/remotes/origin/HEAD", "refs/remotes/origin/main", "refs/remotes/origin/master",
		"refs/heads/main", "refs/heads/master", "@{upstream}"} {
		if _, err := git(ctx, top, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
			continue
		}
		base, err := git(ctx, top, "merge-base", ref, "HEAD")
		if err != nil || !hexHash.MatchString(base) {
			continue // unrelated histories
		}
		return base, nil
	}
	return "", nil
}

// commitsSince reads the commits in base..HEAD along first parents,
// newest first: each one's trailer and paths, a merge's being none. Split on NUL, a record is two empty fields, which no
// message or path can make (a message holds no NUL, and a path is never
// empty), the hash, the message, and the paths, the first after a
// newline.
func commitsSince(ctx context.Context, top, base string) ([]Commit, error) {
	out, err := gitRaw(ctx, top, "-c", "log.showSignature=false", "log", "--no-color", "--first-parent", "--diff-merges=off", "--no-renames", "--name-only",
		"-z", "--max-count="+strconv.Itoa(maxCommits), "--format=%x00%x00%H%x00%B", base+"..HEAD", "--")
	if err != nil {
		return nil, err
	}
	return parseLog(out), nil
}

// parseLog parses commitsSince's git log output.
func parseLog(out string) []Commit {
	f := strings.Split(out, "\x00")
	var cs []Commit
	for i := 0; i+3 < len(f); {
		if f[i] != "" || f[i+1] != "" {
			i++ // not at a record's start; git never writes this
			continue
		}
		var c Commit
		c.Trailer, _ = IDFromMessage(f[i+3])
		i += 4
		for ; i < len(f) && f[i] != ""; i++ {
			if p := strings.TrimPrefix(f[i], "\n"); p != "" {
				c.Paths = append(c.Paths, p)
			}
		}
		cs = append(cs, c)
	}
	return cs
}

// uncommitted reads the files staged, changed or untracked and not
// ignored, in path order. Renames are read as the delete and the add.
func uncommitted(ctx context.Context, top string) ([]string, error) {
	out, err := gitRaw(ctx, top, "--no-optional-locks", "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "-z",
		"--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, err
	}
	return parseStatus(out), nil
}

// parseStatus parses uncommitted's git status output: "XY path" entries,
// NUL-terminated.
func parseStatus(out string) []string {
	var ps []string
	for e := range strings.SplitSeq(out, "\x00") {
		if len(e) > 3 {
			ps = append(ps, e[3:])
		}
	}
	slices.Sort(ps)
	return slices.Compact(ps)
}
