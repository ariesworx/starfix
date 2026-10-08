package store

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

var importer = Actor{Principal: "importer", Session: "import-1", Machine: "server"}

func importedIssue(id IssueID, mod func(*Issue)) Issue {
	created := time.Date(2026, 3, 1, 9, 30, 0, 123456789, time.FixedZone("x", -5*3600))
	is := Issue{
		ID: id, Title: "Imported " + string(id), Body: "body", Design: "design",
		Acceptance: "accept", Notes: "notes", Status: StatusOpen, Priority: P1, Type: TypeBug,
		Assignee: "carol", Owner: "dave", CreatedBy: "erin", CreatedAt: created,
		UpdatedAt: created.Add(time.Hour), Metadata: json.RawMessage(`{"b":1,"a":[true,"x"]}`),
		Labels: []string{"zeta", "area:ui", "zeta"},
	}
	if mod != nil {
		mod(&is)
	}
	return is
}

func TestImportIssuePreservesAndIsIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	closed := time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC)
	due := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	in := importedIssue("bd-a1b", func(is *Issue) {
		is.Status, is.ClosedAt, is.CloseReason, is.DueAt = StatusClosed, &closed, "done", &due
		is.Pinned, is.Template, is.Ephemeral = true, true, true
	})

	res, err := s.ImportIssue(ctx, importer, in)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != ImportCreated || res.LabelsAdded != 2 {
		t.Fatalf("first import = %+v", res)
	}
	got, err := s.GetIssue(ctx, "bd-a1b")
	if err != nil {
		t.Fatal(err)
	}
	want, err := normalizeImport(in)
	if err != nil {
		t.Fatal(err)
	}
	if !sameIssue(got, want) || !slices.Equal(got.Labels, []string{"area:ui", "zeta"}) || got.Rev != 1 {
		t.Errorf("stored\n  %+v\nwant\n  %+v", got, want)
	}
	if got.CreatedBy != "erin" || !got.CreatedAt.Equal(in.CreatedAt.Truncate(time.Microsecond)) {
		t.Errorf("author or created_at not kept: %s %s", got.CreatedBy, got.CreatedAt)
	}

	seq := lastSeq(t, s)
	for range 2 {
		res, err = s.ImportIssue(ctx, importer, in)
		if err != nil || res != (ImportResult{Outcome: ImportUnchanged}) {
			t.Fatalf("re-import = %+v, %v", res, err)
		}
	}
	if lastSeq(t, s) != seq {
		t.Error("re-import wrote events")
	}
	h, err := s.History(ctx, "bd-a1b")
	if err != nil || len(h) != 1 || h[0].Op != OpIssueImport || h[0].Actor != importer {
		t.Errorf("history = %+v, %v", h, err)
	}
}

func TestImportIssueNewerOlder(t *testing.T) {
	base := importedIssue("bd-c2d", nil)
	cases := []struct {
		name   string
		mod    func(*Issue)
		want   ImportOutcome
		labels int
		title  string
	}{
		{"same", func(*Issue) {}, ImportUnchanged, 0, base.Title},
		{"newer", func(is *Issue) { is.Title, is.UpdatedAt = "newer", is.UpdatedAt.Add(time.Minute) }, ImportUpdated, 0, "newer"},
		{"older", func(is *Issue) { is.Title, is.UpdatedAt = "older", is.UpdatedAt.Add(-time.Minute) }, ImportStale, 0, base.Title},
		{"same time differs", func(is *Issue) { is.Title = "tie" }, ImportStale, 0, base.Title},
		{"label only", func(is *Issue) { is.Labels = []string{"new"} }, ImportUpdated, 1, base.Title},
		{"stale merges labels", func(is *Issue) { is.Title, is.Labels = "older", []string{"more"} }, ImportStale, 1, base.Title},
		{"metadata reordered", func(is *Issue) { is.Metadata = json.RawMessage(`{ "a": [true, "x"], "b": 1 }`) }, ImportUnchanged, 0, base.Title},
		{"metadata newer", func(is *Issue) { is.Metadata, is.UpdatedAt = json.RawMessage(`{"c":2}`), is.UpdatedAt.Add(time.Second) }, ImportUpdated, 0, base.Title},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			ctx := t.Context()
			if _, err := s.ImportIssue(ctx, importer, base); err != nil {
				t.Fatal(err)
			}
			in := base
			in.Labels = slices.Clone(base.Labels)
			tc.mod(&in)
			plan, err := s.PlanImportIssue(ctx, in)
			if err != nil {
				t.Fatal(err)
			}
			seq := lastSeq(t, s)
			res, err := s.ImportIssue(ctx, importer, in)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.want || res.LabelsAdded != tc.labels {
				t.Errorf("outcome = %+v, want %s with %d labels", res, tc.want, tc.labels)
			}
			if plan != res {
				t.Errorf("plan %+v differs from result %+v", plan, res)
			}
			got, err := s.GetIssue(ctx, in.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != tc.title {
				t.Errorf("title = %q, want %q", got.Title, tc.title)
			}
			wantRev := Rev(1)
			if tc.want == ImportUpdated && tc.labels == 0 {
				wantRev = 2
			}
			if got.Rev != wantRev {
				t.Errorf("rev = %d, want %d", got.Rev, wantRev)
			}
			if wrote := lastSeq(t, s) != seq; wrote != (tc.want == ImportUpdated || tc.labels > 0) {
				t.Errorf("wrote events = %v for %s", wrote, tc.want)
			}
		})
	}
}

// Import is the operator's, so it writes an issue whoever holds it. An
// import that leaves a held issue anything but in progress with its
// holder (closed, open, blocked, or assigned to someone else) ends the
// claim and tells the holder's session, as close and a releasing handoff
// do, and the holder's time on it stops there. One that leaves it in
// progress with the holder leaves the claim alone.
func TestImportIssueHeld(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*Issue)
		ends bool
	}{
		{"closes it", func(is *Issue) {
			is.Status, is.ClosedAt, is.CloseReason = StatusClosed, ptr(is.UpdatedAt), "done in bd"
		}, true},
		{"reopens it", func(is *Issue) { is.Status, is.Assignee = StatusOpen, "" }, true},
		{"blocks it", func(is *Issue) { is.Status, is.Assignee = StatusBlocked, "alice" }, true},
		{"reassigns it", func(is *Issue) { is.Status, is.Assignee = StatusInProgress, "bob" }, true},
		{"edits it", func(is *Issue) {
			is.Status, is.Assignee, is.Title = StatusInProgress, "alice", "retitled in bd"
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := clockStore(t)
			ctx := t.Context()
			in := importedIssue("bd-h1", nil)
			if _, err := s.ImportIssue(ctx, importer, in); err != nil {
				t.Fatal(err)
			}
			mustStart(t, s, alice, in.ID)
			clk.add(time.Minute)
			in.UpdatedAt = clk.now() // edited in bd after alice started it
			tc.mod(&in)
			if res, err := s.ImportIssue(ctx, importer, in); err != nil || res.Outcome != ImportUpdated {
				t.Fatalf("ImportIssue over alice's claim = %+v, %v; want %s", res, err, ImportUpdated)
			}
			c, err := s.ClaimOf(ctx, in.ID)
			if err != nil {
				t.Fatal(err)
			}
			if held := c != nil; held == tc.ends {
				t.Errorf("after the import, alice's claim held = %v, want %v", held, !tc.ends)
			}
			h, err := s.History(ctx, in.ID)
			if err != nil {
				t.Fatal(err)
			}
			released := 0
			for _, e := range h {
				if e.Op == OpClaimRelease && e.Actor == importer {
					released++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.ends]; released != want {
				t.Errorf("after the import, History(%s) has %d claim.release events by the importer, want %d", in.ID, released, want)
			}
			var want []item
			if tc.ends {
				want = []item{{"alice", "sess-a", InboxClaimLost, in.ID, importer.Principal}}
			}
			if got := items(t, s, alice); !slices.Equal(got, want) {
				t.Errorf("Inbox(alice) = %+v, want %+v", got, want)
			}
			clk.add(10 * time.Minute)
			wantHeld := 11 * time.Minute
			if tc.ends {
				wantHeld = time.Minute
			}
			if u := mustUsage(t, s, in.ID); u.Held != wantHeld {
				t.Errorf("IssueUsage(%s).Held = %s, want %s", in.ID, u.Held, wantHeld)
			}
			assertGapless(t, s)
		})
	}
}

func TestImportIssueInvalid(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	if _, err := s.ImportIssue(ctx, importer, importedIssue("bd-p1", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportIssue(ctx, importer, importedIssue("bd-p2", func(is *Issue) { is.ParentID = "bd-p1" })); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		mod  func(*Issue)
		want error
	}{
		{"bad id", func(is *Issue) { is.ID = "Not An ID" }, ErrInvalid},
		{"empty title", func(is *Issue) { is.Title = " " }, ErrInvalid},
		{"long title", func(is *Issue) { is.Title = strings.Repeat("x", 501) }, ErrInvalid},
		{"status", func(is *Issue) { is.Status = "hooked" }, ErrInvalid},
		{"priority", func(is *Issue) { is.Priority = 7 }, ErrInvalid},
		{"type", func(is *Issue) { is.Type = "spike" }, ErrInvalid},
		{"no author", func(is *Issue) { is.CreatedBy = "" }, ErrInvalid},
		{"no created_at", func(is *Issue) { is.CreatedAt = time.Time{} }, ErrInvalid},
		{"bad label", func(is *Issue) { is.Labels = []string{"has space"} }, ErrInvalid},
		{"bad metadata", func(is *Issue) { is.Metadata = json.RawMessage(`{`) }, ErrInvalid},
		{"missing parent", func(is *Issue) { is.ParentID = "bd-nope" }, ErrNotFound},
		{"own parent", func(is *Issue) { is.ParentID = is.ID }, ErrCycle},
		{"parent cycle", func(is *Issue) {
			is.ID, is.ParentID = "bd-p1", "bd-p2"
			is.UpdatedAt = is.UpdatedAt.Add(time.Hour)
		}, ErrCycle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ImportIssue(ctx, importer, importedIssue("bd-x1", tc.mod))
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestImportDepAndComment(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	for _, id := range []IssueID{"bd-d1", "bd-d2", "bd-d3"} {
		if _, err := s.ImportIssue(ctx, importer, importedIssue(id, nil)); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 3, 2, 1, 2, 3, 456789000, time.UTC)
	dep := func(from, to IssueID, typ DepType, meta string) Dep {
		d := Dep{From: from, To: to, Type: typ, CreatedBy: "erin", CreatedAt: at}
		if meta != "" {
			d.Metadata = json.RawMessage(meta)
		}
		return d
	}
	depCases := []struct {
		name string
		d    Dep
		want ImportOutcome
		err  error
	}{
		{"blocks", dep("bd-d1", "bd-d2", DepBlocks, ""), ImportCreated, nil},
		{"again", dep("bd-d1", "bd-d2", DepBlocks, ""), ImportUnchanged, nil},
		{"other type same pair", dep("bd-d1", "bd-d2", DepRelated, ""), ImportCreated, nil},
		{"metadata", dep("bd-d2", "bd-d3", DepWaitsFor, `{"gate":"any-children"}`), ImportCreated, nil},
		{"metadata reordered", dep("bd-d2", "bd-d3", DepWaitsFor, `{ "gate": "any-children" }`), ImportUnchanged, nil},
		{"metadata differs", dep("bd-d2", "bd-d3", DepWaitsFor, `{"gate":"all-children"}`), ImportStale, nil},
		{"cycle", dep("bd-d3", "bd-d1", DepBlocks, ""), "", ErrCycle},
		{"non-blocking back edge", dep("bd-d3", "bd-d1", DepDiscoveredFrom, ""), ImportCreated, nil},
		{"dangling", dep("bd-d1", "bd-gone", DepBlocks, ""), "", ErrNotFound},
		{"unknown type", dep("bd-d1", "bd-d3", "replies-to", ""), "", ErrInvalid},
		{"no author", Dep{From: "bd-d1", To: "bd-d3", Type: DepRelated, CreatedAt: at}, "", ErrInvalid},
	}
	for _, tc := range depCases {
		t.Run("dep "+tc.name, func(t *testing.T) {
			plan, perr := s.PlanImportDep(ctx, tc.d)
			got, err := s.ImportDep(ctx, importer, tc.d)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("ImportDep = %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
			if tc.err == nil && (perr != nil || plan != got) {
				t.Errorf("plan = %q, %v; want %q", plan, perr, got)
			}
		})
	}
	deps, err := s.AllDeps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 4 {
		t.Fatalf("deps = %+v", deps)
	}
	for _, d := range deps {
		if d.CreatedBy != "erin" || !d.CreatedAt.Equal(at) {
			t.Errorf("dep %s->%s lost its author or time: %+v", d.From, d.To, d)
		}
		if d.Type == DepWaitsFor && !sameJSON(d.Metadata, []byte(`{"gate":"any-children"}`)) {
			t.Errorf("waits-for metadata = %s", d.Metadata)
		}
	}

	c := Comment{ID: "c1", Issue: "bd-d1", Author: "frank", Body: "hello", CreatedAt: at}
	commentCases := []struct {
		name string
		c    Comment
		want ImportOutcome
		err  error
	}{
		{"new", c, ImportCreated, nil},
		{"again", c, ImportUnchanged, nil},
		{"edited", Comment{ID: "c1", Issue: "bd-d1", Author: "frank", Body: "changed", CreatedAt: at}, ImportStale, nil},
		{"missing issue", Comment{ID: "c2", Issue: "bd-gone", Author: "frank", Body: "x", CreatedAt: at}, "", ErrNotFound},
		{"bad id", Comment{ID: "C-1", Issue: "bd-d1", Author: "frank", Body: "x", CreatedAt: at}, "", ErrInvalid},
		{"empty", Comment{ID: "c3", Issue: "bd-d1", Author: "frank", CreatedAt: at}, "", ErrInvalid},
	}
	for _, tc := range commentCases {
		t.Run("comment "+tc.name, func(t *testing.T) {
			got, err := s.ImportComment(ctx, importer, tc.c)
			if !errors.Is(err, tc.err) || got != tc.want {
				t.Fatalf("ImportComment = %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
		})
	}
	cs, err := s.AllComments(ctx)
	if err != nil || len(cs) != 1 || cs[0].Body != "hello" || cs[0].Author != "frank" || !cs[0].CreatedAt.Equal(at) {
		t.Errorf("comments = %+v, %v", cs, err)
	}
}

// bd has no account, so export-bd drops it and import-bd brings the issue
// back without one. That round trip must read as unchanged and keep the
// account the issue already has, not report the issue stale.
func TestImportIssueKeepsAccount(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	in := importedIssue("bd-acct", nil)
	if _, err := s.ImportIssue(ctx, importer, in); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetIssue(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err = s.UpdateIssue(ctx, alice, in.ID, stored.Rev, IssuePatch{Account: ptr("acme")})
	if err != nil {
		t.Fatal(err)
	}
	exported := stored // what export-bd writes, less the account bd cannot carry
	exported.Account = ""

	plan, err := s.PlanImportIssue(ctx, exported)
	if err != nil || plan.Outcome != ImportUnchanged {
		t.Errorf("PlanImportIssue(%s without its account) = %+v, %v; want %s", in.ID, plan, err, ImportUnchanged)
	}
	res, err := s.ImportIssue(ctx, importer, exported)
	if err != nil || res.Outcome != ImportUnchanged {
		t.Errorf("ImportIssue(%s without its account) = %+v, %v; want %s", in.ID, res, err, ImportUnchanged)
	}
	got, err := s.GetIssue(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Account != "acme" {
		t.Errorf("account after re-import = %q, want acme", got.Account)
	}
}
