package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrintWho(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		since string
		w     proto.WhoResult
		want  string
	}{
		{"nobody", "", proto.WhoResult{Now: now}, "nobody seen in the last 5m\n"},
		{"nobody in a window", "2h", proto.WhoResult{Now: now}, "nobody seen in the last 2h\n"},
		{"agents", "", proto.WhoResult{Now: now, Agents: []proto.Agent{
			{Principal: "alice", Session: "cc-1", Machine: "laptop-a", Harness: "claude-code",
				LastSeen: now.Add(-30 * time.Second), Claims: []string{"sf-a1", "sf-b2"}},
			{Principal: "bob", Session: "cli", Machine: "desktop", LastSeen: now.Add(-2*time.Minute - 10*time.Second)},
		}}, "alice/cc-1 on laptop-a (claude-code) seen just now, holds sf-a1 sf-b2\nbob/cli on desktop seen 2m ago\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printWho(&b, tc.since, tc.w)
			if b.String() != tc.want {
				t.Errorf("printWho(%q) =\n%s\nwant:\n%s", tc.since, b.String(), tc.want)
			}
		})
	}
}
