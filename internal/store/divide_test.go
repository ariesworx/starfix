package store

import (
	"fmt"
	"testing"
	"time"
)

// manyHolds is a session that took 1000 issues in turn, each held two
// minutes and overlapping the next by one, and a turn record across all
// of them: the first and last minutes are held by one issue, every other
// minute by two.
func manyHolds() ([]hold, usageRow) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	minute := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	key := sessionKey{"alice", "sess-a"}
	holds := make([]hold, 1000)
	for i := range holds {
		holds[i] = hold{issue: IssueID(fmt.Sprintf("tst-%04d", i)), key: key, start: minute(i), end: minute(i + 2)}
	}
	return holds, usageRow{key: key, model: "opus", from: minute(0), to: minute(1001), Tokens: Tokens{Input: n64(1001 * 1000)}}
}

// A long record over many holds divides exactly, and in time that does
// not grow with the holds times the pieces. When every piece rescanned
// every hold, these 200 records took 1.3s, and 15s under -race; swept,
// they take about 50ms, and 0.4s under -race, so the bound is generous.
func TestDivideManyHolds(t *testing.T) {
	holds, r := manyHolds()
	const records = 200
	x := newHoldIndex(holds)
	start := time.Now()
	var d division
	for range records {
		d = divide(r, x)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("dividing %d records by %d holds took %s, want under 5s", records, len(holds), took)
	}
	parts := d.parts(*r.Input)
	if len(parts) != len(holds)+1 {
		t.Fatalf("divide gave %d parts, want %d issues and unheld", len(parts), len(holds)+1)
	}
	for i, got := range parts {
		want := int64(1000)
		switch i {
		case 0, len(holds) - 1: // one minute alone, one minute shared
			want = 1500
		case len(holds): // unheld
			want = 0
		}
		if got != want {
			t.Errorf("part %d of %d = %d, want %d", i, len(parts), got, want)
		}
	}
}

func BenchmarkDivide(b *testing.B) {
	holds, r := manyHolds()
	x := newHoldIndex(holds)
	for b.Loop() {
		divide(r, x)
	}
}
