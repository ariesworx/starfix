package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Files to issues through the protocol: renew records the paths a
// session's work touched, ready ranks down and marks an issue that
// overlaps them for another principal, and show lists the issue's files
// and who holds the overlapping work.
func TestDispatchPaths(t *testing.T) {
	s := newServer(t)
	p0, p3 := 0, 3
	held := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "held"})
	cand := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "candidate", Priority: &p0,
		Paths: []string{"internal/store/"}})
	other := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "other", Priority: &p3})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: held.ID})
	mustCall[proto.ClaimsResult](t, s, alice, proto.OpRenew, proto.RenewArgs{
		Paths: map[string][]string{held.ID: {"internal/store/paths.go", "docs/cli.md"}}})

	r := mustCall[proto.ListResult](t, s, bob, proto.OpReady, proto.LimitArgs{})
	var got []string
	for _, is := range r.Issues {
		got = append(got, is.ID+strings.Join(is.Overlaps, ","))
	}
	if want := []string{other.ID, cand.ID + held.ID}; !slices.Equal(got, want) {
		t.Errorf("bob's ready = %v, want %v (the overlapping issue last, naming %s)", got, want, held.ID)
	}

	sh := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: cand.ID})
	if sh.Files == nil || !slices.Equal(sh.Files.Paths, []proto.FilePath{{Path: "internal/store/", Source: proto.PathDeclared}}) ||
		!slices.Equal(sh.Files.Overlaps, []proto.Overlap{{ID: held.ID, By: alice.Principal, Session: alice.Session}}) {
		t.Errorf("show %s files = %+v, want its declared prefix and %s held by alice", cand.ID, sh.Files, held.ID)
	}
	sh = mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: held.ID})
	if sh.Files == nil || len(sh.Files.Paths) != 2 || sh.Files.Paths[0].Source != proto.PathCommit {
		t.Errorf("show %s files = %+v, want its two commit paths", held.ID, sh.Files)
	}

	cur := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: cand.ID})
	mustCall[proto.WriteResult](t, s, alice, proto.OpUpdate, proto.UpdateArgs{ID: cand.ID, Rev: cur.Issue.Rev, Paths: &[]string{}})
	if sh := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: cand.ID}); sh.Files != nil {
		t.Errorf("show after clearing the declared paths: files = %+v, want none", sh.Files)
	}

	mustCall[proto.WriteResult](t, s, alice, proto.OpHandoff, proto.HandoffArgs{ID: held.ID, Note: "halfway", Paths: []string{"a.go"}})
	mustCall[proto.FinishResult](t, s, alice, proto.OpFinish, proto.FinishArgs{ID: held.ID, Paths: []string{"b.go"}})
	sh = mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: held.ID})
	var paths []string
	for _, p := range sh.Files.Paths {
		paths = append(paths, p.Path)
	}
	for _, want := range []string{"a.go", "b.go"} {
		if !slices.Contains(paths, want) {
			t.Errorf("paths after handoff and finish = %v, want %s among them", paths, want)
		}
	}
}

// A protocol 2 client sends none of the new fields; every op it uses
// still works, and records no paths.
func TestDispatchPathsProtocol2(t *testing.T) {
	s := newServer(t)
	raw := func(a store.Actor, op, args string) json.RawMessage {
		t.Helper()
		res, perr := s.Dispatch(t.Context(), a, op, json.RawMessage(args))
		if perr != nil {
			t.Fatalf("protocol 2 %s %s: %v", op, args, perr)
		}
		b, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var w proto.WriteResult
	if err := json.Unmarshal(raw(alice, proto.OpCreate, `{"title":"old client"}`), &w); err != nil {
		t.Fatal(err)
	}
	raw(alice, proto.OpUpdate, `{"id":"`+w.ID+`","rev":1,"title":"old client, renamed"}`)
	raw(alice, proto.OpStart, `{"id":"`+w.ID+`","lease":"15m"}`)
	raw(alice, proto.OpRenew, `{"lease":"15m"}`)
	raw(alice, proto.OpHandoff, `{"id":"`+w.ID+`","note":"later"}`)
	raw(alice, proto.OpFinish, `{"id":"`+w.ID+`","reason":"done"}`)
	if b := raw(alice, proto.OpShow, `{"id":"`+w.ID+`"}`); strings.Contains(string(b), `"files"`) {
		t.Errorf("show of an issue with no paths = %s, want no files", b)
	}
}

func TestDispatchPathsRefused(t *testing.T) {
	s := newServerWith(t, Limits{Limits: store.Limits{Paths: 2}})
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "x"})
	tests := []struct {
		name string
		op   string
		args any
		msg  string
	}{
		{"absolute declared path", proto.OpCreate, proto.CreateArgs{Title: "y", Paths: []string{"/etc/passwd"}}, "absolute"},
		{"too many declared paths", proto.OpUpdate, proto.UpdateArgs{ID: is.ID, Rev: is.Rev, Paths: &[]string{"a", "b", "c"}}, "at most 2 paths"},
		{"parent segment in a renew", proto.OpRenew, proto.RenewArgs{Paths: map[string][]string{is.ID: {"../x"}}}, ". or .. segment"},
		{"prefix from git", proto.OpHandoff, proto.HandoffArgs{ID: is.ID, Note: "n", Paths: []string{"dir/"}}, "only declared paths"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.Empty](t, s, alice, tc.op, tc.args)
			if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, tc.msg) || perr.Fix == "" {
				t.Errorf("%s = %+v, want invalid naming %q, with a fix", tc.op, perr, tc.msg)
			}
		})
	}
}

// A start refused because another principal holds the issue names the
// next ready issue as the caller's own ready would: the caller's own
// claims do not count as overlaps.
func TestHeldNamesCallersNextReady(t *testing.T) {
	s := newServer(t)
	p0, p1 := 0, 1
	mine := mustCall[proto.WriteResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "bob's"})
	near := mustCall[proto.WriteResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "near bob's work", Priority: &p0,
		Paths: []string{"a.go"}})
	mustCall[proto.WriteResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "apart", Priority: &p1})
	theirs := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "alice's"})
	mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{ID: mine.ID})
	mustCall[proto.ClaimsResult](t, s, bob, proto.OpRenew, proto.RenewArgs{Paths: map[string][]string{mine.ID: {"a.go"}}})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: theirs.ID})

	_, perr := call[proto.Empty](t, s, bob, proto.OpStart, proto.StartArgs{ID: theirs.ID})
	if perr == nil || !strings.Contains(perr.Message, "next ready: "+near.ID) {
		t.Errorf("bob's start of alice's issue = %+v, want it to name %s, which only bob's own work overlaps", perr, near.ID)
	}
}
