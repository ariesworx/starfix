package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// agentRow reads an agent's rev and last_seen straight from the table.
func agentRow(t *testing.T, s *Store, a Actor) (rev int64, seen time.Time) {
	t.Helper()
	err := s.r.QueryRowContext(t.Context(), `SELECT rev, last_seen FROM agents WHERE principal = ? AND session = ?`,
		a.Principal, a.Session).Scan(&rev, &seen)
	if err != nil {
		t.Fatalf("agent row %s/%s: %v", a.Principal, a.Session, err)
	}
	return rev, seen.UTC()
}

func TestTouchAgentThrottles(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	seq := lastSeq(t, s)
	start := clk.now()

	tests := []struct {
		name    string
		advance time.Duration
		actor   Actor
		harness string
		rev     int64
		seen    time.Time
	}{
		{name: "first touch inserts", actor: alice, harness: "claude-code", rev: 1, seen: start},
		{name: "within a minute writes nothing", advance: 30 * time.Second, actor: alice, harness: "claude-code", rev: 1, seen: start},
		{name: "empty harness keeps the known one", advance: 10 * time.Second, actor: alice, rev: 1, seen: start},
		{name: "a minute on refreshes last_seen", advance: 20 * time.Second, actor: alice, harness: "claude-code", rev: 2,
			seen: start.Add(time.Minute)},
		{name: "a new harness writes at once", advance: time.Second, actor: alice, harness: "codex", rev: 3,
			seen: start.Add(time.Minute + time.Second)},
		{name: "a new machine writes at once", advance: time.Second,
			actor: Actor{Principal: "alice", Session: alice.Session, Machine: "desktop"}, harness: "codex", rev: 4,
			seen: start.Add(time.Minute + 2*time.Second)},
	}
	for _, tc := range tests {
		clk.add(tc.advance)
		if err := s.TouchAgent(ctx, tc.actor, tc.harness); err != nil {
			t.Fatalf("%s: TouchAgent: %v", tc.name, err)
		}
		if rev, seen := agentRow(t, s, tc.actor); rev != tc.rev || !seen.Equal(tc.seen) {
			t.Errorf("%s: rev %d, last_seen %s; want rev %d, %s", tc.name, rev, seen, tc.rev, tc.seen)
		}
	}
	if got := lastSeq(t, s); got != seq {
		t.Errorf("TouchAgent wrote events: seq %d, want %d", got, seq)
	}
	ws, err := s.Who(ctx, 0)
	if err != nil || len(ws) != 1 {
		t.Fatalf("Who = %+v, %v; want one agent", ws, err)
	}
	if a := ws[0]; a.Machine != "desktop" || a.Harness != "codex" || !a.Started.Equal(start) {
		t.Errorf("Who()[0] = %+v, want machine desktop, harness codex, started %s", a, start)
	}
}

// An empty harness keeps the one recorded when the write runs: a touch
// retried after another writer changed it keeps the change, not what the
// first attempt read.
func TestTouchAgentRetryKeepsNewHarness(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	dsn := newDSN(t)
	s := openStore(t, dsn, Options{Now: clk.now})
	other := openStore(t, dsn, Options{Now: clk.now})
	ctx := t.Context()
	if err := s.TouchAgent(ctx, alice, "claude-code"); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	s.beforeCommit = func(ctx context.Context) error {
		attempts++
		if attempts > 1 {
			return nil
		}
		if err := other.TouchAgent(ctx, alice, "codex"); err != nil {
			return err
		}
		return errRetry
	}
	if err := s.TouchAgent(ctx, alice, ""); err != nil {
		t.Fatalf("TouchAgent: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("TouchAgent made %d attempts, want 2", attempts)
	}
	ws, err := s.Who(ctx, 0)
	if err != nil || len(ws) != 1 {
		t.Fatalf("Who = %+v, %v; want one agent", ws, err)
	}
	if got := ws[0].Harness; got != "codex" {
		t.Errorf("harness after a retried TouchAgent with none = %q, want codex, which the other writer recorded", got)
	}
}

func TestTouchAgentRefusesBadHarness(t *testing.T) {
	s, _ := clockStore(t)
	for _, h := range []string{"Claude", "claude code", "a_b", "x;drop", strings.Repeat("a", 33)} {
		if err := s.TouchAgent(t.Context(), alice, h); !errors.Is(err, ErrInvalid) {
			t.Errorf("TouchAgent(harness %q) = %v, want ErrInvalid", h, err)
		}
	}
}

func TestWho(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	carol := Actor{Principal: "carol", Session: "sess-c", Machine: "laptop-c"}
	alice2 := Actor{Principal: "alice", Session: "sess-a2", Machine: "desktop"}

	// carol was here an hour ago; bob ten minutes ago; alice twice, now.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.TouchAgent(ctx, carol, ""))
	clk.add(50 * time.Minute)
	must(s.TouchAgent(ctx, bob, "gemini"))
	clk.add(9 * time.Minute)
	must(s.TouchAgent(ctx, alice, "claude-code"))
	clk.add(time.Minute)
	must(s.TouchAgent(ctx, alice2, "codex"))

	// alice holds two issues in her first session, one of them under a
	// lease that has already run out; alice2 holds one; a closed claim
	// counts for nobody.
	a1 := mustCreate(t, s, NewIssue{Title: "one"})
	a2 := mustCreate(t, s, NewIssue{Title: "two"})
	lapsed := mustCreate(t, s, NewIssue{Title: "lapsed"})
	b1 := mustCreate(t, s, NewIssue{Title: "three"})
	for _, take := range []struct {
		a     Actor
		id    IssueID
		lease time.Duration
	}{{alice, lapsed.ID, time.Minute}, {alice, a2.ID, 0}, {alice, a1.ID, 0}, {alice2, b1.ID, 0}} {
		if _, _, err := s.StartIssue(ctx, take.a, take.id, take.lease, false); err != nil {
			t.Fatal(err)
		}
	}
	clk.add(2 * time.Minute)
	if _, err := s.RenewClaims(ctx, alice, DefaultLease, false); err != nil { // renews a1, a2; lapsed is gone
		t.Fatal(err)
	}
	clk.add(-time.Minute) // the clock may step back; Who still answers

	type row struct {
		who    string
		claims []IssueID
	}
	sorted := func(ids ...IssueID) []IssueID { slices.Sort(ids); return ids }
	tests := []struct {
		name  string
		since time.Duration
		want  []row
	}{
		{"default is five minutes", 0, []row{{"alice/sess-a2", []IssueID{b1.ID}}, {"alice/sess-a", sorted(a1.ID, a2.ID)}}},
		{"a wider window", 15 * time.Minute, []row{{"alice/sess-a2", []IssueID{b1.ID}}, {"alice/sess-a", sorted(a1.ID, a2.ID)},
			{"bob/sess-b", nil}}},
		{"a week", 7 * 24 * time.Hour, []row{{"alice/sess-a2", []IssueID{b1.ID}}, {"alice/sess-a", sorted(a1.ID, a2.ID)},
			{"bob/sess-b", nil}, {"carol/sess-c", nil}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ws, err := s.Who(ctx, tc.since)
			if err != nil {
				t.Fatal(err)
			}
			var got []row
			for _, a := range ws {
				got = append(got, row{a.Principal + "/" + a.Session, a.Claims})
			}
			if len(got) != len(tc.want) {
				t.Fatalf("Who(%s) = %+v, want %+v", tc.since, got, tc.want)
			}
			for i := range got {
				if got[i].who != tc.want[i].who || !slices.Equal(got[i].claims, tc.want[i].claims) {
					t.Errorf("Who(%s)[%d] = %+v, want %+v", tc.since, i, got[i], tc.want[i])
				}
			}
		})
	}
	for _, d := range []time.Duration{-time.Minute, 7*24*time.Hour + time.Second} {
		if _, err := s.Who(ctx, d); !errors.Is(err, ErrInvalid) {
			t.Errorf("Who(%s) = %v, want ErrInvalid", d, err)
		}
	}
}
