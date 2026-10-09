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

// The 1-hour cache writes are part of the cache writes, so each part's
// 1-hour writes are part of its cache writes. Divided independently,
// weights 3:1:10 gave 7 writes as 2, 0 and 5 and their 6 1-hour writes
// as 1, 1 and 4: the second part had more 1-hour writes than writes.
func TestShareKeepsCacheWrite1hWithinCacheWrite(t *testing.T) {
	d := division{issues: []IssueID{"tst-a", "tst-b"}, weights: []float64{3, 1, 10}}
	tests := []struct {
		name     string
		in       Tokens
		cw, cw1h []int64 // per part; nil for unknown
	}{
		{"both known", Tokens{CacheWrite: n64(7), CacheWrite1h: n64(6)}, []int64{1, 1, 5}, []int64{1, 1, 4}},
		{"all 1h", Tokens{CacheWrite: n64(6), CacheWrite1h: n64(6)}, []int64{1, 1, 4}, []int64{1, 1, 4}},
		{"1h unknown", Tokens{CacheWrite: n64(7)}, []int64{2, 0, 5}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cw, cw1h int64
			for b := range d.weights {
				got := d.share(tc.in, b)
				if *got.CacheWrite != tc.cw[b] {
					t.Errorf("share(%s, %d).CacheWrite = %d, want %d", tokensText(ModelUsage{Tokens: tc.in}), b, *got.CacheWrite, tc.cw[b])
				}
				cw += *got.CacheWrite
				if tc.cw1h == nil {
					if got.CacheWrite1h != nil {
						t.Errorf("share(%s, %d).CacheWrite1h = %d, want unknown", tokensText(ModelUsage{Tokens: tc.in}), b, *got.CacheWrite1h)
					}
					continue
				}
				if *got.CacheWrite1h != tc.cw1h[b] {
					t.Errorf("share(%s, %d).CacheWrite1h = %d, want %d", tokensText(ModelUsage{Tokens: tc.in}), b, *got.CacheWrite1h, tc.cw1h[b])
				}
				cw1h += *got.CacheWrite1h
			}
			if cw != *tc.in.CacheWrite {
				t.Errorf("parts of %s sum to %d cache writes, want %d", tokensText(ModelUsage{Tokens: tc.in}), cw, *tc.in.CacheWrite)
			}
			if tc.cw1h != nil && cw1h != *tc.in.CacheWrite1h {
				t.Errorf("parts of %s sum to %d 1-hour writes, want %d", tokensText(ModelUsage{Tokens: tc.in}), cw1h, *tc.in.CacheWrite1h)
			}
		})
	}
}
