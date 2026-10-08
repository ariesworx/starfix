package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// filesOf returns an issue's paths as "source:path", in show's order.
func filesOf(t *testing.T, s *Store, actor Actor, id IssueID) []string {
	t.Helper()
	f, err := s.IssueFiles(t.Context(), actor, id, 1000)
	if err != nil {
		t.Fatalf("IssueFiles(%s): %v", id, err)
	}
	var out []string
	for _, p := range f.Paths {
		out = append(out, string(p.Source)+":"+p.Path)
	}
	return out
}

// opsSince returns the ops of the events recorded after seq.
func opsSince(t *testing.T, s *Store, seq int64) []Op {
	t.Helper()
	evs, err := s.Events(t.Context(), seq, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []Op
	for _, e := range evs {
		out = append(out, e.Op)
	}
	return out
}

func mustRenewPaths(t *testing.T, s *Store, actor Actor, paths map[IssueID][]string) {
	t.Helper()
	if _, err := s.RenewClaims(t.Context(), actor, DefaultLease, false, paths); err != nil {
		t.Fatalf("renew with paths: %v", err)
	}
}

// A renew records commit paths for the claims it renews. New paths are
// one issue.paths event per call; a renew that only sees paths already
// recorded refreshes them quietly, and moves them up in show's order.
func TestCommitPathsOnRenew(t *testing.T) {
	s, clk := clockStore(t)
	is := mustCreate(t, s, NewIssue{Title: "work"})
	mustStart(t, s, alice, is.ID)

	seq := lastSeq(t, s)
	mustRenewPaths(t, s, alice, map[IssueID][]string{is.ID: {"b.go", "a.go"}})
	if got, want := opsSince(t, s, seq), []Op{OpIssuePaths}; !slices.Equal(got, want) {
		t.Errorf("first renew with paths recorded %v, want %v", got, want)
	}
	if got, want := filesOf(t, s, alice, is.ID), []string{"commit:a.go", "commit:b.go"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}

	clk.add(time.Minute)
	seq = lastSeq(t, s)
	mustRenewPaths(t, s, alice, map[IssueID][]string{is.ID: {"b.go"}})
	if got := opsSince(t, s, seq); len(got) != 0 {
		t.Errorf("renew with known paths recorded %v, want no event", got)
	}
	if got, want := filesOf(t, s, alice, is.ID), []string{"commit:b.go", "commit:a.go"}; !slices.Equal(got, want) {
		t.Errorf("files after refreshing b.go = %v, want %v (most recent first)", got, want)
	}

	clk.add(time.Minute)
	seq = lastSeq(t, s)
	mustRenewPaths(t, s, alice, map[IssueID][]string{is.ID: {"b.go", "c.go"}})
	if got, want := opsSince(t, s, seq), []Op{OpIssuePaths}; !slices.Equal(got, want) {
		t.Errorf("renew adding c.go recorded %v, want %v", got, want)
	}
}

// Commit paths are recorded only by the holder's principal (or an admin):
// a renew ignores issues it does not renew, and a handoff or finish by
// another principal is refused before anything is written.
func TestCommitPathsGuard(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	held := mustCreate(t, s, NewIssue{Title: "alice's"})
	mustStart(t, s, alice, held.ID)

	if _, err := s.RenewClaims(ctx, bob, DefaultLease, false, map[IssueID][]string{held.ID: {"x.go"}}); err != nil {
		t.Fatalf("bob's renew naming alice's issue: %v", err)
	}
	if _, err := s.HandoffIssue(ctx, bob, held.ID, 0, HandoffNote{Note: "mine now"}, false, "", []string{"y.go"}); !errors.As(err, new(*ForbiddenError)) {
		t.Errorf("bob's handoff with paths: %v, want a ForbiddenError", err)
	}
	if _, _, err := s.FinishIssue(ctx, bob, held.ID, 0, Finish{Paths: []string{"z.go"}}); !errors.As(err, new(*ForbiddenError)) {
		t.Errorf("bob's finish with paths: %v, want a ForbiddenError", err)
	}
	if got := filesOf(t, s, alice, held.ID); len(got) != 0 {
		t.Errorf("files after bob's attempts = %v, want none", got)
	}

	// Another session of the holder's principal may; so may an admin.
	if _, err := s.HandoffIssue(ctx, alice2, held.ID, 0, HandoffNote{Note: "progress"}, false, "", []string{"a2.go"}); err != nil {
		t.Fatalf("alice2's handoff: %v", err)
	}
	if _, err := s.HandoffIssue(ctx, dana, held.ID, 0, HandoffNote{Note: "admin"}, false, "", []string{"d.go"}); err != nil {
		t.Fatalf("admin's handoff: %v", err)
	}
	if _, _, err := s.FinishIssue(ctx, alice, held.ID, 0, Finish{Paths: []string{"done.go"}}); err != nil {
		t.Fatalf("alice's finish: %v", err)
	}
	got := filesOf(t, s, alice, held.ID)
	for _, want := range []string{"commit:a2.go", "commit:d.go", "commit:done.go"} {
		if !slices.Contains(got, want) {
			t.Errorf("files = %v, want %s among them", got, want)
		}
	}
}

// Declared paths are set at create and replaced by update, under update's
// rules: the issue's holder or an admin, at the current rev.
func TestDeclaredPaths(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "planned", Paths: []string{"internal/store/", "docs/a.md"}})
	if got, want := filesOf(t, s, alice, is.ID), []string{"declared:docs/a.md", "declared:internal/store/"}; !slices.Equal(got, want) {
		t.Errorf("files after create = %v, want %v", got, want)
	}

	seq := lastSeq(t, s)
	out, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Paths: &[]string{"docs/a.md", "README.md"}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Rev != is.Rev {
		t.Errorf("rev after a paths-only update = %d, want %d: paths live in their own table, like labels", out.Rev, is.Rev)
	}
	if got, want := opsSince(t, s, seq), []Op{OpIssuePaths}; !slices.Equal(got, want) {
		t.Errorf("update of declared paths recorded %v, want %v", got, want)
	}
	if got, want := filesOf(t, s, alice, is.ID), []string{"declared:README.md", "declared:docs/a.md"}; !slices.Equal(got, want) {
		t.Errorf("files after update = %v, want %v", got, want)
	}

	seq = lastSeq(t, s)
	if _, err := s.UpdateIssue(ctx, alice, is.ID, out.Rev, IssuePatch{Paths: &[]string{"README.md", "docs/a.md"}}); err != nil {
		t.Fatal(err)
	}
	if got := opsSince(t, s, seq); len(got) != 0 {
		t.Errorf("update to the same set recorded %v, want nothing", got)
	}

	if _, err := s.UpdateIssue(ctx, alice, is.ID, out.Rev+1, IssuePatch{Paths: &[]string{"x"}}); !errors.Is(err, ErrConflict) {
		t.Errorf("update at a stale rev: %v, want ErrConflict", err)
	}
	mustStart(t, s, alice, is.ID)
	cur, err := s.GetIssue(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateIssue(ctx, bob, is.ID, cur.Rev, IssuePatch{Paths: &[]string{"x"}}); !errors.As(err, new(*ForbiddenError)) {
		t.Errorf("bob's update of alice's held issue: %v, want a ForbiddenError", err)
	}
	if _, err := s.UpdateIssue(ctx, alice, is.ID, cur.Rev, IssuePatch{Paths: &[]string{}}); err != nil {
		t.Fatal(err)
	}
	if got := filesOf(t, s, alice, is.ID); len(got) != 0 {
		t.Errorf("files after clearing = %v, want none", got)
	}
}

func TestPathsRefused(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x"})
	mustStart(t, s, alice, is.ID)
	tests := []struct {
		name string
		do   func() error
	}{
		{"create with an absolute path", func() error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "y", Paths: []string{"/etc/passwd"}})
			return err
		}},
		{"update with a parent segment", func() error {
			_, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev+1, IssuePatch{Paths: &[]string{"a/../b"}})
			return err
		}},
		{"renew with a control character", func() error {
			_, err := s.RenewClaims(ctx, alice, DefaultLease, false, map[IssueID][]string{is.ID: {"a\x1b[2J"}})
			return err
		}},
		{"finish with a bidi override", func() error {
			_, _, err := s.FinishIssue(ctx, alice, is.ID, 0, Finish{Paths: []string{"a\u202eb"}})
			return err
		}},
		{"handoff with a directory prefix", func() error {
			_, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "n"}, false, "", []string{"dir/"})
			return err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seq := lastSeq(t, s)
			if err := tc.do(); !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid", err)
			}
			if got := opsSince(t, s, seq); len(got) != 0 {
				t.Errorf("a refused request recorded %v", got)
			}
		})
	}
}

// paths returns n distinct paths named prefix0 to prefix<n-1>.
func paths(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// Rule 18: an issue keeps at most paths_per_issue paths. Commit paths are
// observations, so past the cap the oldest are dropped and the write
// still succeeds (a renew or finish never fails over bookkeeping);
// declared paths are a person's statement, so too many are refused, and
// they take precedence over commit paths.
func TestPathsCap(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: clk.now, Limits: Limits{Paths: 4}})
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "capped"})
	mustStart(t, s, alice, is.ID)

	tests := []struct {
		name    string
		do      func() error
		wantErr string
		want    []string
	}{
		{"one request over the cap keeps its first, most recent, paths", func() error {
			_, err := s.RenewClaims(ctx, alice, DefaultLease, false, map[IssueID][]string{is.ID: paths("a", 6)})
			return err
		}, "", []string{"commit:a0", "commit:a1", "commit:a2", "commit:a3"}},
		{"newer paths push out the oldest", func() error {
			_, err := s.HandoffIssue(ctx, alice, is.ID, 0, HandoffNote{Note: "n"}, false, "", []string{"b0", "b1"})
			return err
		}, "", []string{"commit:b0", "commit:b1", "commit:a0", "commit:a1"}},
		{"declared paths take precedence over commit paths", func() error {
			cur, err := s.GetIssue(ctx, is.ID)
			if err != nil {
				return err
			}
			_, err = s.UpdateIssue(ctx, alice, is.ID, cur.Rev, IssuePatch{Paths: &[]string{"d0/", "d1"}})
			return err
		}, "", []string{"declared:d0/", "declared:d1", "commit:b0", "commit:b1"}},
		{"too many declared paths are refused", func() error {
			cur, err := s.GetIssue(ctx, is.ID)
			if err != nil {
				return err
			}
			_, err = s.UpdateIssue(ctx, alice, is.ID, cur.Rev, IssuePatch{Paths: ptr(paths("d", 5))})
			return err
		}, "at most 4 paths", []string{"declared:d0/", "declared:d1", "commit:b0", "commit:b1"}},
		{"create with too many declared paths is refused", func() error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "y", Paths: paths("d", 5)})
			return err
		}, "at most 4 paths", []string{"declared:d0/", "declared:d1", "commit:b0", "commit:b1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk.add(time.Minute)
			err := tc.do()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("err = %v, want nil", err)
			case tc.wantErr != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("err = %v, want ErrInvalid naming %q", err, tc.wantErr)
			}
			if got := filesOf(t, s, alice, is.ID); !slices.Equal(got, tc.want) {
				t.Errorf("files = %v, want %v", got, tc.want)
			}
		})
	}
}

// readyOverlaps returns Ready's ids for actor, each followed by its overlaps
// in brackets when it has any.
func readyOverlaps(t *testing.T, s *Store, actor Actor) []string {
	t.Helper()
	rs, err := s.Ready(t.Context(), actor, 50)
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	var out []string
	for _, r := range rs {
		id := string(r.ID)
		if len(r.Overlaps) > 0 {
			id += fmt.Sprint(r.Overlaps)
		}
		out = append(out, id)
	}
	return out
}

// ready ranks down, and never hides, an issue whose paths overlap those
// of an issue another session holds; the caller's own claims do not
// count, and the order is otherwise unchanged.
func TestReadyRanksDownOverlaps(t *testing.T) {
	tests := []struct {
		name string
		// held is the path the held issue's commits touched, or its
		// declared paths when declared is set.
		held, cand string
		declared   string // which side declares: "held", "cand" or ""
		reader     Actor
		overlap    bool
	}{
		{name: "same path, another principal", held: "a/x.go", cand: "a/x.go", reader: bob, overlap: true},
		{name: "same path, another session of the holder", held: "a/x.go", cand: "a/x.go", reader: alice2, overlap: true},
		{name: "same path, the holder's own session", held: "a/x.go", cand: "a/x.go", reader: alice, overlap: false},
		{name: "different paths", held: "a/x.go", cand: "a/y.go", reader: bob, overlap: false},
		{name: "held issue declares a prefix covering the path", held: "a/", cand: "a/b/y.go", declared: "held", reader: bob, overlap: true},
		{name: "candidate declares a prefix covering the held path", held: "a/b/x.go", cand: "a/", declared: "cand", reader: bob, overlap: true},
		{name: "prefix is not a string prefix", held: "a/", cand: "ab/y.go", declared: "held", reader: bob, overlap: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			ctx := t.Context()
			var heldIn, candIn NewIssue
			heldIn.Title, candIn.Title = "held", "candidate"
			candIn.Priority = prio(P0)
			if tc.declared == "held" {
				heldIn.Paths = []string{tc.held}
			}
			if tc.declared == "cand" {
				candIn.Paths = []string{tc.cand}
			}
			held := mustCreate(t, s, heldIn)
			cand := mustCreate(t, s, candIn)
			other := mustCreate(t, s, NewIssue{Title: "other", Priority: prio(P3)})
			if tc.declared != "cand" {
				// An earlier holder's commits touched the candidate's path.
				mustStart(t, s, carol, cand.ID)
				if _, err := s.HandoffIssue(ctx, carol, cand.ID, 0, HandoffNote{Note: "later"}, true, "", []string{tc.cand}); err != nil {
					t.Fatal(err)
				}
			}
			mustStart(t, s, alice, held.ID)
			if tc.declared != "held" {
				mustRenewPaths(t, s, alice, map[IssueID][]string{held.ID: {tc.held}})
			}

			want := []string{string(cand.ID), string(other.ID)}
			if tc.overlap {
				want = []string{string(other.ID), fmt.Sprintf("%s[%s]", cand.ID, held.ID)}
			}
			if got := readyOverlaps(t, s, tc.reader); !slices.Equal(got, want) {
				t.Errorf("ready for %s/%s = %v, want %v", tc.reader.Principal, tc.reader.Session, got, want)
			}
		})
	}
}

// start without an id takes what ready lists first, so it passes over an
// overlapping issue while another is ready, and takes it when it is the
// only one.
func TestStartSkipsOverlaps(t *testing.T) {
	s := newStore(t)
	held := mustCreate(t, s, NewIssue{Title: "held", Paths: []string{"a/"}})
	cand := mustCreate(t, s, NewIssue{Title: "candidate", Priority: prio(P0), Paths: []string{"a/x.go"}})
	other := mustCreate(t, s, NewIssue{Title: "other", Priority: prio(P3)})
	mustStart(t, s, alice, held.ID)

	for _, want := range []IssueID{other.ID, cand.ID} {
		is, _, err := s.StartIssue(t.Context(), bob, "", 0, false)
		if err != nil {
			t.Fatal(err)
		}
		if is.ID != want {
			t.Errorf("start took %s, want %s", is.ID, want)
		}
	}
}

// show lists declared paths first, then commit paths most recent first,
// at most n with the rest counted, and the issues others hold whose
// paths overlap, with their holders.
func TestIssueFiles(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "mine", Paths: []string{"z/"}})
	mustStart(t, s, alice, is.ID)
	for _, p := range []string{"c.go", "b.go", "a.go"} {
		clk.add(time.Minute)
		mustRenewPaths(t, s, alice, map[IssueID][]string{is.ID: {p}})
	}
	near := mustCreate(t, s, NewIssue{Title: "near"})
	mustStart(t, s, bob, near.ID)
	mustRenewPaths(t, s, bob, map[IssueID][]string{near.ID: {"z/q.go"}})

	f, err := s.IssueFiles(ctx, alice, is.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range f.Paths {
		got = append(got, string(p.Source)+":"+p.Path)
	}
	if want := []string{"declared:z/", "commit:a.go", "commit:b.go"}; !slices.Equal(got, want) || f.More != 1 {
		t.Errorf("files = %v and %d more, want %v and 1 more", got, f.More, want)
	}
	if len(f.Overlaps) != 1 || f.Overlaps[0].Issue != near.ID || f.Overlaps[0].Holder != bob {
		t.Errorf("overlaps = %+v, want %s held by bob", f.Overlaps, near.ID)
	}
	if _, err := s.IssueFiles(ctx, alice, "tst-none", 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("files of a missing issue: %v, want ErrNotFound", err)
	}
}
