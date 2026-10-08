package proto

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Files to issues (design §12 item 3). A client sends the paths an issue's
// work touched, gathered from git, on renew, finish and handoff, and a
// person declares paths at create and update. ready ranks down an issue
// whose paths overlap those of an issue another session holds, and show
// lists an issue's likely files.

// Path bounds. MaxPaths is the most paths a client sends in one request,
// and the default cap on the paths an issue keeps (the store's
// paths_per_issue limit); MaxPathLen bounds one path, in bytes.
const (
	MaxPaths   = 200
	MaxPathLen = 1024
)

// CheckPath returns why p is not a path the server records, or nil. A
// path is relative to the repository's root, with forward slashes: 1 to
// MaxPathLen bytes of printable UTF-8 (no control, bidi or format
// character, and no space but U+0020), no backslash, no leading "/", and
// no empty, "." or ".." segment. A trailing "/" makes it a directory
// prefix, which covers every path under it; only declared paths may be
// prefixes.
func CheckPath(p string) error {
	switch {
	case p == "" || len(p) > MaxPathLen:
		return fmt.Errorf("path must be 1-%d bytes", MaxPathLen)
	case !utf8.ValidString(p):
		return errors.New("path is not UTF-8")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q is absolute; give it relative to the repository root", p)
	}
	for _, r := range p {
		if r == '\\' || !unicode.IsPrint(r) {
			return fmt.Errorf("path %q holds %U; use forward slashes and printable characters", p, r)
		}
	}
	for seg := range strings.SplitSeq(strings.TrimSuffix(p, "/"), "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q has an empty, . or .. segment", p)
		}
	}
	return nil
}

// IsPathPrefix reports whether p, a path CheckPath accepts, is a
// directory prefix.
func IsPathPrefix(p string) bool { return strings.HasSuffix(p, "/") }

// Path sources: the paths an issue's commits touched, and those a person
// or agent declared.
const (
	PathCommit   = "commit"
	PathDeclared = "declared"
)

// MaxShowPaths is the most paths show lists; Files.More counts the rest.
const MaxShowPaths = 20

// Files are an issue's likely files, for show: declared paths first, then
// those its commits touched, most recent first, at most MaxShowPaths, with
// More counting the rest. Overlaps are the issues other sessions hold now
// whose paths overlap these.
type Files struct {
	Paths    []FilePath `json:"paths,omitempty"`
	More     int        `json:"more,omitempty"`
	Overlaps []Overlap  `json:"overlaps,omitempty"`
}

// FilePath is one of an issue's paths and where it came from (PathCommit
// or PathDeclared).
type FilePath struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}

// Overlap is an issue another session holds whose paths overlap: the
// issue, and its holder's principal and session.
type Overlap struct {
	ID      string `json:"id"`
	By      string `json:"by"`
	Session string `json:"session"`
}
