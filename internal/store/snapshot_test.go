package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

// hookedQuerier runs hook just before its second query, so a test can
// commit writes between a read's first query and the rest.
type hookedQuerier struct {
	querier
	queries int
	hook    func()
}

func (h *hookedQuerier) next() {
	if h.queries++; h.queries == 2 {
		h.hook()
	}
}

func (h *hookedQuerier) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	h.next()
	return h.querier.QueryContext(ctx, q, args...)
}

func (h *hookedQuerier) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	h.next()
	return h.querier.QueryRowContext(ctx, q, args...)
}

// Each read of several queries sees one snapshot: writes that commit
// after its first query change nothing it returns.
func TestReadsAreOneSnapshot(t *testing.T) {
	// a is ready, with a label and two acceptance items; c has a label and
	// is blocked by b.
	type fixture struct{ a, b, c IssueID }
	tests := []struct {
		name string
		read func(ctx context.Context, s *Store, f fixture) (any, error)
	}{
		{"GetIssue", func(ctx context.Context, s *Store, f fixture) (any, error) { return s.GetIssue(ctx, f.a) }},
		{"List", func(ctx context.Context, s *Store, _ fixture) (any, error) { return s.List(ctx, Filter{}) }},
		{"Ready", func(ctx context.Context, s *Store, _ fixture) (any, error) { return s.Ready(ctx, 0) }},
		{"Blocked", func(ctx context.Context, s *Store, _ fixture) (any, error) { return s.Blocked(ctx, 0) }},
		{"AcceptanceItems", func(ctx context.Context, s *Store, f fixture) (any, error) {
			return s.AcceptanceItems(ctx, f.a)
		}},
		{"PlanImportIssue", func(ctx context.Context, s *Store, f fixture) (any, error) {
			return s.PlanImportIssue(ctx, importedIssue(f.a, func(is *Issue) { is.Labels = []string{"new", "old"} }))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			ctx := t.Context()
			f := fixture{
				a: mustCreate(t, s, NewIssue{Title: "a", Labels: []string{"old"}, Acceptance: "- one\n- two"}).ID,
				b: mustCreate(t, s, NewIssue{Title: "b"}).ID,
				c: mustCreate(t, s, NewIssue{Title: "c", Labels: []string{"old"}}).ID,
			}
			if err := s.AddDep(ctx, alice, f.c, f.b, DepBlocks); err != nil {
				t.Fatal(err)
			}
			want, err := tc.read(ctx, s, f)
			if err != nil {
				t.Fatal(err)
			}

			// change commits a change to every table the reads' later
			// queries read: labels, a blocker's status, acceptance state.
			change := func() error {
				for _, id := range []IssueID{f.a, f.c} {
					if err := s.AddLabel(ctx, alice, id, "new"); err != nil {
						return err
					}
				}
				if _, err := s.CloseIssue(ctx, alice, f.b, 0, ""); err != nil {
					return err
				}
				_, err := s.Accept(ctx, alice, f.a, Acceptance{Tick: []int{1}})
				return err
			}
			ran := false
			var wrote error
			s.wrapRead = func(q querier) querier {
				return &hookedQuerier{querier: q, hook: func() {
					if !ran {
						ran, wrote = true, change()
					}
				}}
			}
			got, err := tc.read(ctx, s, f)
			if err != nil {
				t.Fatal(err)
			}
			if !ran {
				t.Fatalf("%s ran fewer than two queries, so nothing was written between them", tc.name)
			}
			if wrote != nil {
				t.Fatalf("writing between the queries: %v", wrote)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s with writes committed between its queries = %+v\nwant, as before them, %+v", tc.name, got, want)
			}
		})
	}
}
