package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrintDigest(t *testing.T) {
	until := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return until.Add(-d) }
	tests := []struct {
		name string
		d    proto.DigestResult
		want string
	}{
		{"quiet", proto.DigestResult{Since: ago(24 * time.Hour), Until: until},
			"since 2026-10-06 12:00 UTC (1d): 0 events\n"},
		{"every section", proto.DigestResult{
			Since: ago(7 * 24 * time.Hour), Until: until, Truncated: true,
			Totals: proto.DigestTotals{Events: 40, Closed: 12, Started: 1, InProgress: 1, Stalled: 1, Blocked: 1,
				HandedOff: 1, Created: 1, Discovered: 1},
			Closed:     []proto.DigestItem{{ID: "sf-a1", Title: "Fix the login redirect", Priority: 1, By: "bob", At: ago(3 * time.Hour)}},
			Started:    []proto.DigestItem{{ID: "sf-b2", Title: "Add digest", Priority: 2, By: "alice", At: ago(90 * time.Minute)}},
			InProgress: []proto.DigestItem{{ID: "sf-b2", Title: "Add digest", Priority: 2, By: "alice", At: ago(28 * time.Hour)}},
			Stalled:    []proto.DigestItem{{ID: "sf-c3", Title: "Old work", Priority: 3, By: "carol", At: ago(9 * 24 * time.Hour)}},
			Blocked:    []proto.DigestItem{{ID: "sf-d4", Title: "Ship it", Priority: 0, BlockedBy: []string{"sf-a1", "sf-b2"}}},
			HandedOff: []proto.DigestItem{{ID: "sf-a1", Title: "Fix the login redirect", Priority: 1, By: "bob", At: ago(3 * time.Hour),
				Note: "cookie path\nis gone; " + strings.Repeat("x", 80)}},
			Created:    []proto.DigestItem{{ID: "sf-e5", Title: "Flaky test", Priority: 2, By: "bob", At: ago(3 * time.Hour)}},
			Discovered: []proto.DigestItem{{ID: "sf-e5", Title: "Flaky test", Priority: 2, By: "bob", At: ago(3 * time.Hour), From: "sf-a1"}},
		}, `since 2026-09-30 12:00 UTC (7d): 40 events, some totals partial
closed 12, 1 shown
  sf-a1  P1  Fix the login redirect  bob  3h ago
started 1
  sf-b2  P2  Add digest  alice  1h ago
in progress 1
  sf-b2  P2  Add digest  alice  for 1d4h
stalled 1
  sf-c3  P3  Old work  carol  idle 9d
blocked 1
  sf-d4  P0  Ship it  by sf-a1, sf-b2
handed off 1
  sf-a1  P1  Fix the login redirect  bob  3h ago  cookie path is gone; ` + strings.Repeat("x", 39) + `…
created 1
  sf-e5  P2  Flaky test  bob  3h ago
discovered 1
  sf-e5  P2  Flaky test  bob  3h ago  from sf-a1
`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printDigest(&b, tc.d)
			if b.String() != tc.want {
				t.Fatalf("got:\n%s\nwant:\n%s", b.String(), tc.want)
			}
		})
	}
}
