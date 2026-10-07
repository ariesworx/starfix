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
	// LabelsAdded counts labels merged into issues that already existed or
	// were created.
	LabelsAdded int `json:"labels_added"`
	// Skipped counts lines that are not issues: memory, tombstone and
	// header records.
	Skipped  int       `json:"skipped"`
	Problems []Problem `json:"problems"`

	merged map[string]int
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
	part := func(name string, c Counts) string {
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
			return "0 " + name + "s"
		}
		return fmt.Sprintf("%d %s (%s)", c.total(), plural(c.total(), name), strings.Join(xs, ", "))
	}
	warnings := len(r.Problems) - r.Errors()
	return fmt.Sprintf("%s %s, %s, %s; %d %s, %d %s", verb,
		part("issue", r.Issues), part("dep", r.Deps), part("comment", r.Comments),
		r.Errors(), plural(r.Errors(), "error"), warnings, plural(warnings, "warning"))
}

// plural returns w for 1 and w+"s" otherwise.
func plural(n int, w string) string {
	if n == 1 {
		return w
	}
	return w + "s"
}

// fail records an error.
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
