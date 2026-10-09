package bdimport

import (
	"fmt"
	"strings"
)

// Counts tallies outcomes for one kind of record.
type Counts struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	// Stale rows differ from the store, which is kept (see store.ImportStale).
	Stale  int `json:"stale"`
	Failed int `json:"failed"`
}

// total counts the records of every outcome.
func (c Counts) total() int { return c.Created + c.Updated + c.Unchanged + c.Stale + c.Failed }

// Levels of a Problem.
const (
	LevelError   = "error"
	LevelWarning = "warning"
)

// Problem is one failure or warning. Warnings of the same kind are merged
// into one Problem listing every affected ID.
type Problem struct {
	Level   string   `json:"level"`
	Kind    string   `json:"kind"`
	Message string   `json:"message"`
	IDs     []string `json:"ids,omitempty"`
	Lines   []int    `json:"lines,omitempty"`
	Fix     string   `json:"fix"`
}

// Report is the outcome of an import, or with DryRun what it would be.
type Report struct {
	DryRun   bool   `json:"dry_run"`
	Lines    int    `json:"lines"`
	Issues   Counts `json:"issues"`
	Deps     Counts `json:"deps"`
	Comments Counts `json:"comments"`
	Memories Counts `json:"memories"`
	// LabelsAdded counts labels merged into issues that already existed or
	// were created.
	LabelsAdded int `json:"labels_added"`
	// Skipped counts lines that are neither issues nor memories:
	// tombstone, header and other records.
	Skipped  int       `json:"skipped"`
	Problems []Problem `json:"problems"`

	merged map[string]int // a warning's key → its index in Problems
}

// Errors counts error-level problems.
func (r *Report) Errors() int {
	n := 0
	for _, p := range r.Problems {
		if p.Level == LevelError {
			n++
		}
	}
	return n
}

// Summary is the one-line result.
func (r *Report) Summary() string {
	verb := "imported"
	if r.DryRun {
		verb = "would import"
	}
	part := func(name, names string, c Counts) string {
		var xs []string
		for _, f := range []struct {
			n    int
			word string
		}{{c.Created, "created"}, {c.Updated, "updated"}, {c.Unchanged, "unchanged"}, {c.Stale, "kept"}, {c.Failed, "failed"}} {
			if f.n > 0 {
				xs = append(xs, fmt.Sprintf("%d %s", f.n, f.word))
			}
		}
		if len(xs) == 0 {
			return "0 " + names
		}
		if c.total() == 1 {
			names = name
		}
		return fmt.Sprintf("%d %s (%s)", c.total(), names, strings.Join(xs, ", "))
	}
	parts := []string{part("issue", "issues", r.Issues), part("dep", "deps", r.Deps), part("comment", "comments", r.Comments)}
	// Most bd backlogs hold no memories; the count shows when one does.
	if r.Memories.total() > 0 {
		parts = append(parts, part("memory", "memories", r.Memories))
	}
	warnings := len(r.Problems) - r.Errors()
	return fmt.Sprintf("%s %s; %d %s, %d %s", verb, strings.Join(parts, ", "),
		r.Errors(), plural(r.Errors(), "error"), warnings, plural(warnings, "warning"))
}

// plural returns w for 1 and w+"s" otherwise.
func plural(n int, w string) string {
	if n == 1 {
		return w
	}
	return w + "s"
}

// fail records an error and returns it, so the caller can add its IDs.
// The pointer is valid only until the next problem is recorded.
func (r *Report) fail(kind string, lineNo int, fix, format string, a ...any) *Problem {
	p := Problem{Level: LevelError, Kind: kind, Message: fmt.Sprintf(format, a...), Fix: fix}
	if lineNo > 0 {
		p.Lines = []int{lineNo}
	}
	r.Problems = append(r.Problems, p)
	return &r.Problems[len(r.Problems)-1]
}

// warn records a warning, merged with others of the same key.
func (r *Report) warn(key, kind, id string, lineNo int, fix, message string) {
	if r.merged == nil {
		r.merged = map[string]int{}
	}
	i, ok := r.merged[key]
	if !ok {
		r.Problems = append(r.Problems, Problem{Level: LevelWarning, Kind: kind, Message: message, Fix: fix})
		i = len(r.Problems) - 1
		r.merged[key] = i
	}
	p := &r.Problems[i]
	if id != "" && (len(p.IDs) == 0 || p.IDs[len(p.IDs)-1] != id) {
		p.IDs = append(p.IDs, id)
	}
	if lineNo > 0 && (len(p.Lines) == 0 || p.Lines[len(p.Lines)-1] != lineNo) {
		p.Lines = append(p.Lines, lineNo)
	}
}

// Text renders a problem as two lines, "<level>: <message> (<ids>)" and
// "fix: <fix>", listing at most five IDs.
func (p Problem) Text() string {
	msg := p.Message
	if len(p.IDs) > 0 {
		ids := p.IDs
		more := ""
		if len(ids) > 5 {
			ids, more = ids[:5], fmt.Sprintf(" and %d more", len(p.IDs)-5)
		}
		msg += fmt.Sprintf(" (%s%s)", strings.Join(ids, ", "), more)
	} else if len(p.Lines) > 0 && !strings.HasPrefix(msg, "line ") {
		msg += fmt.Sprintf(" (line %d)", p.Lines[0])
	}
	return fmt.Sprintf("%s: %s\nfix: %s", p.Level, msg, p.Fix)
}
