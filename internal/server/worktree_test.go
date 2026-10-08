package server

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// A handoff's worktree is a path on its author's machine; only the author's
// principal reads it back (S-14).
func TestWorktreeOnlyToItsAuthor(t *testing.T) {
	s := newServer(t)
	const wt = "/home/alice/src/wt-secret"
	c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "handed on"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: c.ID})
	mustCall[proto.WriteResult](t, s, alice, proto.OpHandoff, proto.HandoffArgs{ID: c.ID, Note: "half done", Release: true,
		HandoffFields: proto.HandoffFields{Next: "write the tests", Branch: "feature/x", Worktree: wt}})

	tests := []struct {
		name string
		read func() string
	}{
		{"show", func() string {
			r := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: c.ID})
			if r.Handoff == nil || r.Handoff.Next != "write the tests" || r.Handoff.Branch != "feature/x" {
				t.Fatalf("show lost the handoff: %+v", r.Handoff)
			}
			return r.Handoff.Worktree
		}},
		{"history", func() string {
			r := mustCall[proto.HistoryResult](t, s, bob, proto.OpHistory, proto.IDArgs{ID: c.ID})
			var all strings.Builder
			for _, e := range r.Events {
				all.Write(e.Before)
				all.Write(e.After)
			}
			if !strings.Contains(all.String(), "write the tests") {
				t.Fatalf("history lost the handoff: %s", all.String())
			}
			return all.String()
		}},
		{"start", func() string {
			r := mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{ID: c.ID})
			if r.Handoff == nil || r.Handoff.Next != "write the tests" {
				t.Fatalf("start lost the handoff: %+v", r.Handoff)
			}
			return r.Handoff.Worktree
		}},
	}
	for _, tc := range tests {
		if got := tc.read(); strings.Contains(got, wt) {
			t.Errorf("bob's %s shows alice's worktree: %s", tc.name, got)
		}
	}

	r := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: c.ID})
	if r.Handoff == nil || r.Handoff.Worktree != wt {
		t.Errorf("alice's show: handoff %+v, want worktree %s", r.Handoff, wt)
	}
	h := mustCall[proto.HistoryResult](t, s, alice, proto.OpHistory, proto.IDArgs{ID: c.ID})
	found := false
	for _, e := range h.Events {
		found = found || strings.Contains(string(e.After), wt)
	}
	if !found {
		t.Errorf("alice's history leaves out her own worktree")
	}
}

// A comment.add state that another principal reads goes out only when its
// handoff can be read, so that the worktree can be taken out. A state
// planted or corrupted past parsing is withheld whole; a plain comment's,
// or one whose handoff names no worktree, goes out unchanged. The author
// reads hers as stored.
func TestWorktreeWithheldWhenUnreadable(t *testing.T) {
	s, dsn := newServerDSN(t, Limits{})
	const wt = "/home/alice/src/wt-secret"
	tests := []struct {
		name  string
		after string // the comment.add event's after state, as planted
		shown bool   // bob's history carries it
	}{
		{name: "no handoff", after: `{"body": "plain"}`, shown: true},
		{name: "null handoff", after: `{"body": "plain", "handoff": null}`, shown: true},
		{name: "handoff without a worktree", after: `{"body": "plain", "handoff": {"next": "tests"}}`, shown: true},
		{name: "state not an object", after: `"` + wt + `"`},
		{name: "handoff not an object", after: `{"body": "plain", "handoff": "` + wt + `"}`},
		{name: "handoff a list", after: `{"body": "plain", "handoff": ["` + wt + `"]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "planted"})
			mustCall[proto.CommentResult](t, s, alice, proto.OpComment, proto.CommentArgs{ID: c.ID, Body: "plain"})
			plant(t, dsn, "UPDATE events SET after_state = ? WHERE target = ? AND op = 'comment.add'", tc.after, c.ID)
			for _, r := range []struct {
				viewer store.Actor
				shown  bool
			}{{bob, tc.shown}, {alice, true}} {
				h := mustCall[proto.HistoryResult](t, s, r.viewer, proto.OpHistory, proto.IDArgs{ID: c.ID})
				var after json.RawMessage
				for _, e := range h.Events {
					if e.Op == string(store.OpCommentAdd) {
						after = e.After
					}
				}
				switch {
				case r.shown && !sameJSON(after, []byte(tc.after)):
					t.Errorf("%s's history: comment.add after = %s, want %s", r.viewer.Principal, after, tc.after)
				case !r.shown && after != nil:
					t.Errorf("%s's history: comment.add after = %s, want it withheld", r.viewer.Principal, after)
				}
			}
		})
	}
}

// plant runs query on the store's database behind its back, as a corrupt
// row, or a writer with access to the database, would change it.
func plant(t *testing.T, dsn, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("plant %q: %v", query, err)
	}
}

// sameJSON reports whether a and b encode the same JSON value.
func sameJSON(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
