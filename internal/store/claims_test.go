package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// clock is a settable server clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func clockStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	return openStore(t, newDSN(t), Options{Now: c.now}), c
}

func TestClaimTakeAndTakeover(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})

	_, c1, err := s.StartIssue(ctx, alice, is.ID, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if c1.Epoch != 1 || c1.Holder != alice || !c1.ExpiresAt.Equal(clk.now().Add(DefaultLease)) {
		t.Fatalf("first claim: %+v", c1)
	}

	// The same session again keeps the epoch and extends the lease, with
	// no event.
	clk.add(time.Minute)
	seq := lastSeq(t, s)
	_, c2, err := s.StartIssue(ctx, alice, is.ID, time.Hour, false)
	if err != nil || c2.Epoch != 1 || !c2.ExpiresAt.Equal(clk.now().Add(time.Hour)) {
		t.Fatalf("same session: %+v, %v", c2, err)
	}
	if lastSeq(t, s) != seq {
		t.Error("renewing by start wrote an event")
	}

	// Another session of the same principal takes it over, when asked to.
	_, c3, err := s.StartIssue(ctx, alice2, is.ID, 0, true)
	if err != nil || c3.Epoch != 2 || c3.Holder != alice2 {
		t.Fatalf("takeover: %+v, %v", c3, err)
	}

	// The old session's finish, with its epoch, is fenced off.
	_, _, err = s.FinishIssue(ctx, alice, is.ID, 1, Finish{})
	var stale *StaleEpochError
	if !errors.As(err, &stale) || stale.Current != 2 || stale.By != alice2 || !errors.Is(err, ErrConflict) {
		t.Fatalf("stale finish: %v", err)
	}

	// Another principal is refused while the lease runs.
	if _, _, err := s.StartIssue(ctx, bob, is.ID, 0, false); !errors.As(err, new(*HeldError)) {
		t.Fatalf("bob while held: %v", err)
	}

	// The current epoch finishes, and the claim ends.
	if _, _, err := s.FinishIssue(ctx, alice2, is.ID, 2, Finish{}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.ClaimOf(ctx, is.ID); err != nil || c != nil {
		t.Fatalf("claim after finish: %+v, %v", c, err)
	}
}

func TestClaimExpiry(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	other := mustCreate(t, s, NewIssue{Title: "other"})
	if _, _, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartIssue(ctx, alice, other.ID, time.Hour, false); err != nil {
		t.Fatal(err)
	}

	// Past the lease, the claim holds nothing even before the reaper runs:
	// bob may take it, under a new epoch.
	clk.add(DefaultLease + time.Second)
	got, c, err := s.StartIssue(ctx, bob, is.ID, 0, false)
	if err != nil || c.Epoch != 2 || got.Assignee != "bob" {
		t.Fatalf("take expired: %+v %+v, %v", got, c, err)
	}

	// Let bob's lapse too; the reaper ends it and reopens the issue, and
	// leaves alice's hour-long claim alone.
	clk.add(DefaultLease + time.Second)
	reaped, err := s.ReapClaims(ctx)
	if err != nil || len(reaped) != 1 || reaped[0].Issue != is.ID || reaped[0].Holder != bob {
		t.Fatalf("reap: %+v, %v", reaped, err)
	}
	got, err = s.GetIssue(ctx, is.ID)
	if err != nil || got.Status != StatusOpen || got.Assignee != "" {
		t.Fatalf("after reap: %+v, %v", got, err)
	}
	if c, _ := s.ClaimOf(ctx, other.ID); c == nil || c.Holder != alice {
		t.Fatalf("live claim reaped: %+v", c)
	}
	evs, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(evs); n < 2 || evs[n-2].Op != OpClaimExpire || evs[n-2].Actor != ReaperActor || evs[n-1].Op != OpIssueUpdate {
		t.Fatalf("reap events: %+v", evs[n-2:])
	}

	// A second pass finds nothing; bob's late finish is refused by epoch.
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("second reap: %+v, %v", reaped, err)
	}
	if _, _, err := s.FinishIssue(ctx, bob, is.ID, 1, Finish{}); !errors.As(err, new(*StaleEpochError)) {
		t.Fatalf("stale epoch after reap: %v", err)
	}
	// Taking it again raises the epoch past every earlier holder.
	if _, c, err := s.StartIssue(ctx, alice, is.ID, 0, false); err != nil || c.Epoch != 3 {
		t.Fatalf("retake: %+v, %v", c, err)
	}
}

func TestRenewClaims(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	if _, _, err := s.StartIssue(ctx, alice, a.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.StartIssue(ctx, alice2, b.ID, 0, false); err != nil {
		t.Fatal(err)
	}
	start := clk.now()

	// A renewal with most of the lease left writes nothing.
	clk.add(time.Minute)
	got, err := s.RenewClaims(ctx, alice, DefaultLease, false, nil)
	if err != nil || len(got) != 1 || got[0].Issue != a.ID || !got[0].ExpiresAt.Equal(start.Add(DefaultLease)) {
		t.Fatalf("early renew: %+v, %v", got, err)
	}

	// Past half the lease it extends, only for this session.
	clk.add(8 * time.Minute)
	got, err = s.RenewClaims(ctx, alice, DefaultLease, false, nil)
	if err != nil || len(got) != 1 || !got[0].ExpiresAt.Equal(clk.now().Add(DefaultLease)) {
		t.Fatalf("renew: %+v, %v", got, err)
	}
	if c, _ := s.ClaimOf(ctx, b.ID); !c.ExpiresAt.Equal(start.Add(DefaultLease)) {
		t.Fatalf("other session renewed: %+v", c)
	}

	// Going away renews every session's claims, and a later short renewal
	// never shortens them.
	got, err = s.RenewClaims(ctx, alice, 4*time.Hour, true, nil)
	if err != nil || len(got) != 2 {
		t.Fatalf("away: %+v, %v", got, err)
	}
	away := clk.now().Add(4 * time.Hour)
	for _, c := range got {
		if !c.ExpiresAt.Equal(away) {
			t.Fatalf("away expiry: %+v", c)
		}
	}
	if got, err := s.RenewClaims(ctx, alice2, DefaultLease, false, nil); err != nil || !got[0].ExpiresAt.Equal(away) {
		t.Fatalf("short renew after away: %+v, %v", got, err)
	}

	if _, err := s.RenewClaims(ctx, alice, time.Second, false, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short lease: %v", err)
	}
	if _, _, err := s.StartIssue(ctx, alice, a.ID, 8*24*time.Hour, false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long lease: %v", err)
	}
}

func TestClaimEndsOnCloseAndRelease(t *testing.T) {
	s, _ := clockStore(t)
	ctx := t.Context()
	a := mustCreate(t, s, NewIssue{Title: "a"})
	b := mustCreate(t, s, NewIssue{Title: "b"})
	for _, id := range []IssueID{a.ID, b.ID} {
		if _, _, err := s.StartIssue(ctx, alice, id, 0, false); err != nil {
			t.Fatal(err)
		}
	}
	// An admin may close it; the claim ends with it.
	if _, err := s.CloseIssue(ctx, dana, a.ID, 0, "dup"); err != nil {
		t.Fatal(err)
	}
	// Release with a stale epoch is refused; with the current one it ends
	// the claim and lets bob start it.
	if _, err := s.HandoffIssue(ctx, alice, b.ID, 7, HandoffNote{Note: "n"}, true, "", nil); !errors.As(err, new(*StaleEpochError)) {
		t.Fatalf("stale release: %v", err)
	}
	if _, err := s.HandoffIssue(ctx, alice, b.ID, 1, HandoffNote{Note: "over to you"}, true, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, id := range []IssueID{a.ID, b.ID} {
		if c, err := s.ClaimOf(ctx, id); err != nil || c != nil {
			t.Fatalf("claim on %s remains: %+v, %v", id, c, err)
		}
	}
	held, err := s.ClaimsOf(ctx, "alice")
	if err != nil || len(held) != 0 {
		t.Fatalf("alice's claims: %+v, %v", held, err)
	}
	if _, c, err := s.StartIssue(ctx, bob, b.ID, 0, false); err != nil || c.Epoch != 2 {
		t.Fatalf("bob takes released: %+v, %v", c, err)
	}
}

// An issue in_progress without a claim (before claims existed, or
// imported) holds nothing: the reaper leaves it alone, and anyone may
// start it.
func TestUnclaimedInProgressHoldsNothing(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "old", Status: StatusInProgress, Assignee: "alice"})
	clk.add(30 * 24 * time.Hour)
	if reaped, err := s.ReapClaims(ctx); err != nil || len(reaped) != 0 {
		t.Fatalf("reap: %+v, %v", reaped, err)
	}
	if got, c, err := s.StartIssue(ctx, bob, is.ID, 0, false); err != nil || c.Epoch != 1 || got.Assignee != "bob" {
		t.Fatalf("bob starts it: %+v %+v, %v", got, c, err)
	}
}

// Every way a claim ends is in the event log as an event of its own,
// naming the claim it ends, ahead of the change that ended it: the event
// log is the truth (AGENTS.md rule 7), and a release that only a later
// state implies cannot be told from no release.
func TestClaimEndsAreRecorded(t *testing.T) {
	tests := []struct {
		name string
		// end ends alice's claim on id, taken at epoch 1, as by.
		end func(t *testing.T, s *Store, clk *clock, id IssueID) Actor
		// ends is false for a change that keeps the claim.
		ends bool
	}{
		{name: "a close by an admin", ends: true, end: func(t *testing.T, s *Store, _ *clock, id IssueID) Actor {
			if _, err := s.CloseIssue(t.Context(), dana, id, 0, "dup"); err != nil {
				t.Fatal(err)
			}
			return dana
		}},
		{name: "a finish", ends: true, end: func(t *testing.T, s *Store, _ *clock, id IssueID) Actor {
			if _, _, err := s.FinishIssue(t.Context(), alice, id, 1, Finish{Reason: "done"}); err != nil {
				t.Fatal(err)
			}
			return alice
		}},
		{name: "a releasing handoff", ends: true, end: func(t *testing.T, s *Store, _ *clock, id IssueID) Actor {
			if _, err := s.HandoffIssue(t.Context(), alice, id, 1, HandoffNote{Note: "over to you"}, true, "", nil); err != nil {
				t.Fatal(err)
			}
			return alice
		}},
		{name: "an update moving the issue out of progress after the lease lapsed", ends: true, end: func(t *testing.T, s *Store, clk *clock, id IssueID) Actor {
			// Before the reaper ran, bob moved the issue out of progress,
			// which a lapsed claim does not stop; the update ends the
			// claim, and alice's later release finds nothing to end.
			clk.add(DefaultLease + time.Minute)
			is, err := s.GetIssue(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateIssue(t.Context(), bob, id, is.Rev, IssuePatch{Status: ptr(StatusBlocked), Assignee: ptr("")}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.HandoffIssue(t.Context(), alice, id, 1, HandoffNote{Note: "parked"}, true, "", nil); err != nil {
				t.Fatal(err)
			}
			return bob
		}},
		{name: "an update reassigning the issue after the lease lapsed", ends: true, end: func(t *testing.T, s *Store, clk *clock, id IssueID) Actor {
			clk.add(DefaultLease + time.Minute)
			is, err := s.GetIssue(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateIssue(t.Context(), bob, id, is.Rev, IssuePatch{Assignee: ptr("bob")}); err != nil {
				t.Fatal(err)
			}
			return bob
		}},
		{name: "an update of other fields after the lease lapsed", end: func(t *testing.T, s *Store, clk *clock, id IssueID) Actor {
			clk.add(DefaultLease + time.Minute)
			is, err := s.GetIssue(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateIssue(t.Context(), bob, id, is.Rev, IssuePatch{Title: ptr("renamed")}); err != nil {
				t.Fatal(err)
			}
			return bob
		}},
		{name: "a handoff that keeps the claim", end: func(t *testing.T, s *Store, _ *clock, id IssueID) Actor {
			if _, err := s.HandoffIssue(t.Context(), alice, id, 1, HandoffNote{Note: "progress"}, false, "", nil); err != nil {
				t.Fatal(err)
			}
			return alice
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := clockStore(t)
			ctx := t.Context()
			is := mustCreate(t, s, NewIssue{Title: "work"})
			_, c, err := s.StartIssue(ctx, alice, is.ID, 0, false)
			if err != nil {
				t.Fatal(err)
			}
			by := tc.end(t, s, clk, is.ID)
			evs, err := s.History(ctx, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			var releases []Event
			for _, e := range evs {
				if e.Op == OpClaimRelease {
					releases = append(releases, e)
				}
			}
			if !tc.ends {
				if len(releases) > 0 {
					t.Errorf("History(%s) has %d claim.release events, want none", is.ID, len(releases))
				}
				return
			}
			if len(releases) != 1 {
				t.Fatalf("History(%s) has %d claim.release events, want 1: %+v", is.ID, len(releases), evs)
			}
			r := releases[0]
			var before struct {
				Holder    Actor     `json:"holder"`
				Epoch     int64     `json:"epoch"`
				ExpiresAt time.Time `json:"expires_at"`
			}
			if err := json.Unmarshal(r.Before, &before); err != nil {
				t.Fatalf("claim.release before state %s: %v", r.Before, err)
			}
			if r.Actor != by || before.Holder != alice || before.Epoch != 1 || !before.ExpiresAt.Equal(c.ExpiresAt) || len(r.After) != 0 {
				t.Errorf("claim.release = by %+v, before %s, after %s; want by %+v, naming alice's claim at epoch 1 to %s",
					r.Actor, r.Before, r.After, by, c.ExpiresAt)
			}
			// The change that ended the claim follows it, in the same
			// transaction.
			for _, e := range evs {
				if e.Seq > r.Seq && e.At.Equal(r.At) && e.Actor == r.Actor {
					return
				}
			}
			t.Errorf("no event of the same write follows claim.release (seq %d): %+v", r.Seq, evs)
		})
	}
}
