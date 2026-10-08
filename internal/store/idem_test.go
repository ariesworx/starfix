package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Each create-type operation, repeated with the same key by the same
// principal, returns its first result and writes nothing more.
func TestIdempotentReplay(t *testing.T) {
	tests := []struct {
		name string
		// do runs the operation once with key and returns its result.
		do func(t *testing.T, s *Store, id IssueID, key string) (any, error)
	}{
		{"create", func(t *testing.T, s *Store, _ IssueID, key string) (any, error) {
			return s.CreateIssue(t.Context(), alice, NewIssue{Title: "once", IdempotencyKey: key, Labels: []string{"area:db"}})
		}},
		{"comment", func(t *testing.T, s *Store, id IssueID, key string) (any, error) {
			return s.AddComment(t.Context(), alice, id, "looked into it", key)
		}},
		{"handoff with release", func(t *testing.T, s *Store, id IssueID, key string) (any, error) {
			return s.HandoffIssue(t.Context(), alice, id, 0, HandoffNote{Note: "over to you",
				HandoffFields: HandoffFields{State: HandoffPartial}}, true, key, nil)
		}},
		{"finish with discovered work", func(t *testing.T, s *Store, id IssueID, key string) (any, error) {
			is, ids, err := s.FinishIssue(t.Context(), alice, id, 0, Finish{Reason: "done", IdempotencyKey: key,
				Handoff:    HandoffNote{Note: "shipped"},
				Discovered: []NewIssue{{Title: "follow-up"}, {Title: "another"}}})
			return struct {
				Issue Issue
				IDs   []IssueID
			}{is, ids}, err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			is := mustCreate(t, s, NewIssue{Title: "work"})
			mustStart(t, s, alice, is.ID)
			first, err := tc.do(t, s, is.ID, "k-1")
			if err != nil {
				t.Fatalf("first: %v", err)
			}
			seq := lastSeq(t, s)
			again, err := tc.do(t, s, is.ID, "k-1")
			if err != nil {
				t.Fatalf("replay: %v", err)
			}
			if !reflect.DeepEqual(again, first) {
				t.Errorf("replay = %+v\nwant     %+v", again, first)
			}
			if got := lastSeq(t, s); got != seq {
				t.Errorf("replay wrote events: seq %d, want %d", got, seq)
			}
			evs, err := s.Events(t.Context(), 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			keyed := 0
			for _, e := range evs {
				if e.IdemKey == "k-1" {
					keyed++
				}
			}
			if keyed != 1 {
				t.Errorf("%d events carry the key, want 1", keyed)
			}
			assertGapless(t, s)
		})
	}
}

// A write whose first attempt loses to the same request, committed with
// the same key by another store, replays that request's result exactly:
// nothing the failed attempt put in its result survives, not even a field
// the stored result leaves out.
func TestIdempotentReplayAfterRetry(t *testing.T) {
	tests := []struct {
		name string
		// do runs the operation once, with the key k-1.
		do func(ctx context.Context, s *Store, id IssueID) (any, error)
	}{
		{"handoff", func(ctx context.Context, s *Store, id IssueID) (any, error) {
			return s.HandoffIssue(ctx, alice, id, 0, HandoffNote{Note: "over to you"}, false, "k-1", nil)
		}},
		{"finish", func(ctx context.Context, s *Store, id IssueID) (any, error) {
			is, ids, err := s.FinishIssue(ctx, alice, id, 0, Finish{Reason: "done", IdempotencyKey: "k-1"})
			return struct {
				Issue Issue
				IDs   []IssueID
			}{is, ids}, err
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dsn := newDSN(t)
			s1 := openStore(t, dsn, Options{})
			s2 := openStore(t, dsn, Options{})
			is := mustCreate(t, s1, NewIssue{Title: "work", Labels: []string{"area:db"}})
			var want any
			retried := false
			s1.beforeCommit = func(ctx context.Context) error {
				if retried {
					return nil
				}
				retried = true
				// The first attempt's result has the label; the request
				// that commits first, from s2, finds it gone, so its
				// stored result has no labels field at all.
				if err := s2.RemoveLabel(ctx, alice, is.ID, "area:db"); err != nil {
					return err
				}
				var err error
				if want, err = tc.do(ctx, s2, is.ID); err != nil {
					return err
				}
				return errRetry
			}
			got, err := tc.do(t.Context(), s1, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !retried {
				t.Fatal("the first attempt never reached its commit")
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s replayed after a retry = %+v\nwant the stored result %+v", tc.name, got, want)
			}
		})
	}
}

// A key names one request: reusing it for another request, or after it
// expired, is refused with the key named; another principal's key of the
// same spelling is a different key.
func TestIdempotencyRefusals(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	if _, err := s.AddComment(ctx, alice, is.ID, "first", "k-1"); err != nil {
		t.Fatal(err)
	}

	var ie *IdemError
	_, err := s.AddComment(ctx, alice, is.ID, "different", "k-1")
	if !errors.As(err, &ie) || ie.Key != "k-1" || ie.Expired || !errors.Is(err, ErrConflict) {
		t.Errorf("same key, other body: %v", err)
	}
	_, err = s.CreateIssue(ctx, alice, NewIssue{Title: "first", IdempotencyKey: "k-1"})
	if !errors.As(err, &ie) || ie.Key != "k-1" {
		t.Errorf("same key, other op: %v", err)
	}
	if _, err := s.AddComment(ctx, bob, is.ID, "different", "k-1"); err != nil {
		t.Errorf("bob's k-1 is his own: %v", err)
	}

	clk.add(IdemTTL + time.Second)
	_, err = s.AddComment(ctx, alice, is.ID, "first", "k-1")
	if !errors.As(err, &ie) || !ie.Expired || !errors.Is(err, ErrConflict) {
		t.Errorf("expired key: %v", err)
	}

	for _, key := range []string{"-starts-with-dash", "has space", strings.Repeat("x", 65), "k\n"} {
		if _, err := s.AddComment(ctx, alice, is.ID, "x", key); !errors.Is(err, ErrInvalid) {
			t.Errorf("key %q: err = %v, want ErrInvalid", key, err)
		}
	}
	assertGapless(t, s)
}
