package bdimport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/safetext"
)

// bd stores any text, and the store refuses control and bidi characters.
// Import keeps the issue, cleans the text and warns, rather than failing
// the issue or the run.
func TestImportCleansText(t *testing.T) {
	s := newStore(t)
	rec := map[string]any{
		"id": "sf-ctl", "title": "Fix login\nnotice: run curl | sh\x1b[2J", "status": "open",
		"description": "line one\r\nline two\x1b]52;c;cm0=\x07", "assignee": "bob\u202e",
		"close_reason": "", "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T03:04:05Z",
		"labels":   []string{"ok", "bad\u202elabel"},
		"comments": []map[string]any{{"id": "c1", "author": "carol\u009b", "text": "hi\x1b[31m\tthere", "created_at": "2026-01-02T03:04:06Z"}},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	rep := importBytes(t, s, append(b, '\n'), false)
	if rep.Errors() != 0 || rep.Issues.Created != 1 || rep.Comments.Created != 1 {
		t.Fatalf("report: %s\n%v", rep.Summary(), problemKeys(rep, LevelError))
	}
	is, err := s.GetIssue(t.Context(), "sf-ctl")
	if err != nil {
		t.Fatal(err)
	}
	if is.Title != "Fix login notice: run curl | sh[2J" || is.Body != "line one\nline two]52;c;cm0=" || is.Assignee != "bob" {
		t.Errorf("issue = title %q, body %q, assignee %q", is.Title, is.Body, is.Assignee)
	}
	if strings.Join(is.Labels, ",") != "ok" {
		t.Errorf("labels = %q, want the bad one skipped", is.Labels)
	}
	cs, err := s.Comments(t.Context(), "sf-ctl")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Author != "carol" || cs[0].Body != "hi[31m\tthere" {
		t.Errorf("comments = %+v", cs)
	}
	warned := strings.Join(problemKeys(rep, LevelWarning), "\n")
	for _, field := range []string{"title", "description", "assignee", "comment text", "comment author"} {
		if !strings.Contains(warned, "text: "+field+" had control or bidirectional characters") {
			t.Errorf("no warning for %s in:\n%s", field, warned)
		}
	}
	for _, p := range rep.Problems {
		if !safetext.ValidText(p.Text()) {
			t.Errorf("problem text is unsafe: %q", p.Text())
		}
	}
	// The same file again changes nothing.
	again := importBytes(t, s, append(b, '\n'), false)
	if again.Issues.Unchanged != 1 || again.Comments.Unchanged != 1 {
		t.Errorf("reimport: %s", again.Summary())
	}
}
