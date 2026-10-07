package store

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"
)

// dana is the test stores' admin (openStore).
var dana = Actor{Principal: "dana", Session: "sess-d", Machine: "laptop-d"}

// TestHeldIssueWrites runs every guarded write against an issue alice
// holds, as each kind of actor, and against an unheld one: only the
// holder's principal and admins may change a held issue, an admin's
// change records the override, and a refusal writes nothing (S-1, D1).
func TestHeldIssueWrites(t *testing.T) {
	type op struct {
		name string
		acc  string // the issue's acceptance text
		run  func(s *Store, a Actor, is Issue) error
	}
	ops := []op{
		{name: "update", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.UpdateIssue(t.Context(), a, is.ID, is.Rev, IssuePatch{Title: ptr("retitled"), Body: ptr("new body")})
			return err
		}},
		{name: "reassign", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.UpdateIssue(t.Context(), a, is.ID, is.Rev, IssuePatch{Assignee: ptr("bob")})
			return err
		}},
		{name: "reopen status", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.UpdateIssue(t.Context(), a, is.ID, is.Rev, IssuePatch{Status: ptr(StatusOpen)})
			return err
		}},
		{name: "close", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.CloseIssue(t.Context(), a, is.ID, 0, "done")
			return err
		}},
		{name: "force close", acc: "- a", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.ForceClose(t.Context(), a, is.ID, 0, "obsolete")
			return err
		}},
		{name: "handoff note", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.HandoffIssue(t.Context(), a, is.ID, 0, HandoffNote{Note: "n"}, false, "")
			return err
		}},
		{name: "release", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.HandoffIssue(t.Context(), a, is.ID, 0, HandoffNote{Note: "n"}, true, "")
			return err
		}},
		{name: "accept", acc: "- a", run: func(s *Store, a Actor, is Issue) error {
			_, err := s.Accept(t.Context(), a, is.ID, Acceptance{Tick: []int{1}})
			return err
		}},
		{name: "finish", acc: "- a", run: func(s *Store, a Actor, is Issue) error {
			_, _, err := s.FinishIssue(t.Context(), a, is.ID, 0, Finish{Accept: Acceptance{Tick: []int{1}}})
			return err
		}},
	}
	// want is each op's outcome for one actor; ops absent succeed.
	type want map[string]error
	claimed := want{"reassign": ErrInvalid, "reopen status": ErrInvalid, "force close": ErrForbidden}
	refused := want{}
	for _, o := range ops {
		refused[o.name] = ErrForbidden
	}
	cases := []struct {
		name     string
		actor    Actor
		held     bool
		want     want
		override bool
	}{
		{name: "holder", actor: alice, held: true, want: claimed},
		{name: "same principal other session", actor: alice2, held: true, want: claimed},
		{name: "other principal", actor: bob, held: true, want: refused},
		{name: "admin", actor: dana, held: true, want: want{"reassign": ErrInvalid, "reopen status": ErrInvalid}, override: true},
		{name: "unheld other principal", actor: bob, want: want{"force close": ErrForbidden}},
		{name: "unheld admin", actor: dana, want: want{}},
	}
	s := newStore(t)
	for _, tc := range cases {
		for _, o := range ops {
			t.Run(tc.name+"/"+o.name, func(t *testing.T) {
				is := mustCreate(t, s, NewIssue{Title: "work", Acceptance: o.acc})
				if tc.held {
					var err error
					if is, _, err = s.StartIssue(t.Context(), alice, is.ID, 0, false); err != nil {
						t.Fatal(err)
					}
				}
				seq := lastSeq(t, s)
				err := o.run(s, tc.actor, is)
				if w := tc.want[o.name]; !errors.Is(err, w) || (w == nil) != (err == nil) {
					t.Fatalf("%s by %s/%s = %v, want %v", o.name, tc.actor.Principal, tc.actor.Session, err, w)
				}
				var fe *ForbiddenError
				if errors.As(err, &fe) && fe.Action == "" && (fe.Holder != alice || fe.ID != is.ID || fe.Until.IsZero()) {
					t.Errorf("refusal names %+v, want holder %+v of %s", fe, alice, is.ID)
				}
				if err != nil {
					if got := lastSeq(t, s); got != seq {
						t.Errorf("refused %s wrote %d events", o.name, got-seq)
					}
					return
				}
				evs, herr := s.History(t.Context(), is.ID)
				if herr != nil {
					t.Fatal(herr)
				}
				var overrides []Event
				for _, e := range evs {
					if e.Op == OpAdminOverride {
						overrides = append(overrides, e)
					}
				}
				if !tc.override {
					if len(overrides) != 0 {
						t.Fatalf("override recorded for %s: %+v", tc.name, overrides)
					}
					return
				}
				if len(overrides) != 1 || overrides[0].Actor != dana || overrides[0].Seq != seq+1 {
					t.Fatalf("override events = %+v, want one by dana at seq %d", overrides, seq+1)
				}
				var after struct {
					Holder Actor  `json:"holder"`
					Op     string `json:"op"`
				}
				if err := json.Unmarshal(overrides[0].After, &after); err != nil || after.Holder != alice || after.Op == "" {
					t.Errorf("override after = %s (%v)", overrides[0].After, err)
				}
			})
		}
	}
	assertGapless(t, s)
}

// Reopen is guarded too, and anyone may reopen an unheld closed issue.
func TestReopenUnheld(t *testing.T) {
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "x"})
	if _, err := s.CloseIssue(t.Context(), alice, is.ID, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReopenIssue(t.Context(), bob, is.ID, 0); err != nil {
		t.Fatalf("reopen unheld: %v", err)
	}
}

// Comments, labels and dependencies stay open to everyone.
func TestHeldIssueStillTakesComments(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x"})
	other := mustCreate(t, s, NewIssue{Title: "y"})
	mustStart(t, s, alice, is.ID)
	if _, err := s.AddComment(ctx, bob, is.ID, "a thought", ""); err != nil {
		t.Errorf("comment: %v", err)
	}
	if err := s.AddLabel(ctx, bob, is.ID, "triage"); err != nil {
		t.Errorf("label: %v", err)
	}
	if err := s.AddDep(ctx, bob, is.ID, other.ID, DepRelated); err != nil {
		t.Errorf("dep: %v", err)
	}
}

// An admin who closes or releases an issue another session holds tells
// that session its claim is gone.
func TestAdminEndTellsHolder(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	mustStart(t, s, alice, a.ID)
	mustStart(t, s, alice, b.ID)
	if _, err := s.CloseIssue(ctx, dana, a.ID, 0, "dup"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HandoffIssue(ctx, dana, b.ID, 0, HandoffNote{Note: "moving it"}, true, ""); err != nil {
		t.Fatal(err)
	}
	page, err := s.Inbox(ctx, alice, false, 10)
	if err != nil {
		t.Fatal(err)
	}
	var lost []IssueID
	for _, it := range page.Items {
		if it.Kind == InboxClaimLost && it.From == "dana" {
			lost = append(lost, it.Issue)
		}
	}
	slices.Sort(lost)
	want := []IssueID{a.ID, b.ID}
	slices.Sort(want)
	if !slices.Equal(lost, want) {
		t.Errorf("claim.lost items for %v, want %v", lost, want)
	}
}

// Same-principal takeover of a live claim needs take; the same session
// (a reconnect) needs nothing, and an expired claim is free to anyone.
func TestTakeoverNeedsTake(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	mustStart(t, s, alice, is.ID)

	seq := lastSeq(t, s)
	_, _, err := s.StartIssue(ctx, alice2, is.ID, 0, false)
	var held *HeldError
	if !errors.As(err, &held) || !held.Own || held.Session != alice.Session || !errors.Is(err, ErrConflict) {
		t.Fatalf("takeover without take = %v, want an own *HeldError", err)
	}
	if lastSeq(t, s) != seq {
		t.Error("refused takeover wrote events")
	}
	if _, c, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil || c.Epoch != 1 {
		t.Fatalf("same session again: %+v, %v", c, err)
	}
	if _, _, err := s.StartIssue(ctx, bob, is.ID, 0, true); !errors.As(err, &held) || held.Own {
		t.Fatalf("take by another principal = %v, want *HeldError", err)
	}
	_, c, err := s.StartIssue(ctx, alice2, is.ID, 0, true)
	if err != nil || c.Epoch != 2 || c.Holder != alice2 {
		t.Fatalf("takeover with take: %+v, %v", c, err)
	}

	clk.add(DefaultLease + time.Second)
	if _, c, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil || c.Epoch != 3 {
		t.Fatalf("expired claim, no take: %+v, %v", c, err)
	}
}

// Holds come only from claims: update cannot set in_progress, and an
// issue in_progress without a claim holds nothing (S-1).
func TestUpdateCannotHold(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x"})
	seq := lastSeq(t, s)
	_, err := s.UpdateIssue(ctx, bob, is.ID, is.Rev, IssuePatch{Status: ptr(StatusInProgress), Assignee: ptr("bob")})
	if !errors.Is(err, ErrStatusInProgress) || !errors.Is(err, ErrInvalid) {
		t.Fatalf("update status in_progress = %v, want ErrStatusInProgress", err)
	}
	if lastSeq(t, s) != seq {
		t.Error("refused update wrote events")
	}
	legacy := mustCreate(t, s, NewIssue{Title: "old", Status: StatusInProgress, Assignee: "alice"})
	if got, _, err := s.StartIssue(ctx, bob, legacy.ID, 0, false); err != nil || got.Assignee != "bob" {
		t.Fatalf("start of unclaimed in_progress: %+v, %v", got, err)
	}
}

// S-3: acceptance text cannot tick items once the issue exists, and an
// edit that drops an open item is refused until it is ticked or waived.
func TestAcceptanceTextCannotTick(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x", Acceptance: "- [ ] tests pass\n- [ ] docs"})

	is, err := s.UpdateIssue(ctx, bob, is.ID, is.Rev, IssuePatch{Acceptance: ptr("- [x] tests pass\n- [x] docs")})
	if err != nil {
		t.Fatal(err)
	}
	var ae *AcceptanceError
	if _, err := s.CloseIssue(ctx, bob, is.ID, 0, ""); !errors.As(err, &ae) || !slices.Equal(ae.Open, []int{1, 2}) {
		t.Fatalf("close after ticking in text = %v, want items 1, 2 open", err)
	}

	for _, tc := range []struct {
		text    string
		dropped []int
	}{{"", []int{1, 2}}, {"- tests pass", []int{2}}, {"- tests pass\n- documentation", []int{2}}} {
		text := tc.text
		seq := lastSeq(t, s)
		_, err := s.UpdateIssue(ctx, bob, is.ID, is.Rev, IssuePatch{Acceptance: &text})
		if !errors.As(err, &ae) || !ae.Dropped || !slices.Equal(ae.Open, tc.dropped) || !errors.Is(err, ErrInvalid) {
			t.Fatalf("update acceptance to %q = %v, want items %v dropped", text, err, tc.dropped)
		}
		if lastSeq(t, s) != seq {
			t.Errorf("refused update to %q wrote events", text)
		}
	}

	// Waived, on the record, the item may go; new items come in open.
	if _, err := s.Accept(ctx, bob, is.ID, Acceptance{Waive: map[int]string{2: "docs live elsewhere"}}); err != nil {
		t.Fatal(err)
	}
	is, err = s.UpdateIssue(ctx, bob, is.ID, is.Rev, IssuePatch{Acceptance: ptr("- tests pass\n- [x] fast")})
	if err != nil {
		t.Fatalf("update after waiving: %v", err)
	}
	if _, err := s.CloseIssue(ctx, bob, is.ID, 0, ""); !errors.As(err, &ae) || !slices.Equal(ae.Open, []int{1, 2}) {
		t.Fatalf("close = %v, want items 1, 2 open", err)
	}
}

// S-12: a start or one session's renewal leases at most a day; only a
// renewal of every session may reach a week.
func TestLeaseCaps(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "x"})
	tests := []struct {
		name string
		run  func() error
		want error
	}{
		{"start a day", func() error { _, _, err := s.StartIssue(ctx, alice, is.ID, MaxClaimLease, false); return err }, nil},
		{"start over a day", func() error {
			_, _, err := s.StartIssue(ctx, alice, is.ID, MaxClaimLease+time.Minute, false)
			return err
		}, ErrInvalid},
		{"renew session over a day", func() error {
			_, err := s.RenewClaims(ctx, alice, MaxClaimLease+time.Minute, false)
			return err
		}, ErrInvalid},
		{"renew all a week", func() error { _, err := s.RenewClaims(ctx, alice, MaxLease, true); return err }, nil},
		{"renew all over a week", func() error { _, err := s.RenewClaims(ctx, alice, MaxLease+time.Minute, true); return err }, ErrInvalid},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// S-15: the server's own principals are reserved and cannot be admins.
func TestReservedPrincipals(t *testing.T) {
	for _, p := range []string{"starfixd", "import"} {
		if !Reserved(p) {
			t.Errorf("Reserved(%q) = false", p)
		}
	}
	if Reserved("alice") {
		t.Error(`Reserved("alice") = true`)
	}
	dsn := newDSN(t)
	for _, admins := range [][]string{{"starfixd"}, {"import"}, {"Not A Name"}} {
		if s, err := Open(t.Context(), dsn, Options{CommitInterval: -1, Admins: admins}); !errors.Is(err, ErrInvalid) {
			if s != nil {
				_ = s.Close()
			}
			t.Errorf("Open with admins %q = %v, want ErrInvalid", admins, err)
		}
	}
	s := newStore(t)
	if !s.IsAdmin("dana") || s.IsAdmin("alice") {
		t.Error("IsAdmin does not follow Options.Admins")
	}
}
