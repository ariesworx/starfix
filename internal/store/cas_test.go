package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"
)

// TestUpdateSameRevOneWinner: many concurrent updates from the same rev,
// through one store. Exactly one wins; the rest get ErrConflict.
func TestUpdateSameRevOneWinner(t *testing.T) {
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "start"})

	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			p := IssuePatch{Title: ptr(fmt.Sprintf("writer %d", i))}
			if i%2 == 1 {
				p = IssuePatch{Priority: prio(P0)}
			}
			_, errs[i] = s.UpdateIssue(t.Context(), alice, is.ID, is.Rev, p)
		})
	}
	wg.Wait()
	assertOneWinner(t, errs)
	got, err := s.GetIssue(t.Context(), is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rev != 2 {
		t.Errorf("rev = %d, want 2", got.Rev)
	}
	assertUpdateEvents(t, s, is.ID, 1)
}

// TestUpdateOverlappingTransactions is the stage 0 bug: two transactions
// (two store processes) read the same rev, change different columns, and
// commit. Without write_id Dolt merges both silently; with it, exactly one
// wins and the other gets ErrConflict.
func TestUpdateOverlappingTransactions(t *testing.T) {
	dsn := newDSN(t)
	s1 := openStore(t, dsn, Options{})
	s2 := openStore(t, dsn, Options{})
	is := mustCreate(t, s1, NewIssue{Title: "start", Priority: prio(P2)})

	b := newBarrier(2)
	s1.beforeCommit = b.hook
	s2.beforeCommit = b.hook

	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, errs[0] = s1.UpdateIssue(t.Context(), alice, is.ID, is.Rev, IssuePatch{Title: ptr("from s1")})
	})
	wg.Go(func() {
		_, errs[1] = s2.UpdateIssue(t.Context(), bob, is.ID, is.Rev, IssuePatch{Priority: prio(P0)})
	})
	wg.Wait()
	assertOneWinner(t, errs)
	t.Logf("loser: %v", errors.Join(errs...))

	got, err := s1.GetIssue(t.Context(), is.ID)
	if err != nil {
		t.Fatal(err)
	}
	titleChanged, prioChanged := got.Title != "start", got.Priority != P2
	if titleChanged == prioChanged {
		t.Errorf("exactly one change must apply: title %q, priority %d", got.Title, got.Priority)
	}
	if got.Rev != 2 {
		t.Errorf("rev = %d, want 2", got.Rev)
	}
	assertUpdateEvents(t, s1, is.ID, 1)
	assertGapless(t, s1)
}

// TestDoltNeedsWriteID pins the Dolt behavior the store depends on: a rev
// CAS alone lets two overlapping transactions both commit; adding a unique
// write_id makes the second fail with 1213. If Dolt ever changes this, the
// first half fails and the design should be revisited.
func TestDoltNeedsWriteID(t *testing.T) {
	cases := []struct {
		name     string
		set1     string
		set2     string
		conflict bool
	}{
		{"rev only", "title = 'a'", "priority = 0", false},
		{"rev and write_id", "title = 'a', write_id = 101", "priority = 0, write_id = 202", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newDSN(t)
			s := openStore(t, dsn, Options{})
			is := mustCreate(t, s, NewIssue{Title: "start"})
			db, err := sql.Open("mysql", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			ctx := t.Context()
			tx1, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			tx2, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i, x := range []struct {
				tx  *sql.Tx
				set string
			}{{tx1, tc.set1}, {tx2, tc.set2}} {
				q := "UPDATE issues SET " + x.set + ", rev = rev + 1 WHERE id = ? AND rev = 1" //nolint:gosec // constant test cases
				res, err := x.tx.ExecContext(ctx, q, string(is.ID))
				if err != nil {
					t.Fatalf("tx%d: %v", i+1, err)
				}
				if n, _ := res.RowsAffected(); n != 1 {
					t.Fatalf("tx%d: CAS matched %d rows", i+1, n)
				}
			}
			if err := tx1.Commit(); err != nil {
				t.Fatalf("tx1 commit: %v", err)
			}
			err = tx2.Commit()
			var me *mysql.MySQLError
			gotConflict := errors.As(err, &me) && me.Number == 1213
			if gotConflict != tc.conflict {
				t.Fatalf("tx2 commit: %v; want conflict=%v", err, tc.conflict)
			}
		})
	}
}

func assertOneWinner(t *testing.T, errs []error) {
	t.Helper()
	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrConflict):
		default:
			t.Errorf("writer %d: unexpected error %v", i, err)
		}
	}
	if wins != 1 {
		t.Errorf("%d writers won from the same rev, want exactly 1", wins)
	}
}

func assertUpdateEvents(t *testing.T, s *Store, id IssueID, want int) {
	t.Helper()
	h, err := s.History(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range h {
		if e.Op == OpIssueUpdate {
			n++
		}
	}
	if n != want {
		t.Errorf("%d update events, want %d", n, want)
	}
}

func assertGapless(t *testing.T, s *Store) {
	t.Helper()
	evs, err := s.Events(t.Context(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range evs {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: sequence has a gap", i, e.Seq)
		}
	}
}
