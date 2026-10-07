package server

import (
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
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
