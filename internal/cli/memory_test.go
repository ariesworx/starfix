package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrintMemories(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  proto.RecallResult
		want string
	}{
		{"none", proto.RecallResult{}, "no memories\n"},
		{"some", proto.RecallResult{Memories: []proto.Memory{
			{Key: "deploy", Scope: "project", Body: "weekday mornings\nnever Fridays", Tags: []string{"ops", "release"},
				Issue: "sf-a1b2", Pinned: true, UpdatedBy: "alice", UpdatedAt: at, Rev: 2},
			{Key: "style", Scope: "user", Body: "tabs", UpdatedBy: "bob", UpdatedAt: at, Rev: 1, Relevant: true},
		}, More: 3},
			"deploy (project, pinned) rev 2, alice, 2026-10-09 08:00 UTC\n" +
				"  tags: ops, release; issue: sf-a1b2\n" +
				"  weekday mornings\n  never Fridays\n" +
				"style (user, relevant) rev 1, bob, 2026-10-09 08:00 UTC\n" +
				"  tabs\n" +
				"and 3 more; narrow the search, or raise -n\n"},
		{"escaped key", proto.RecallResult{Memories: []proto.Memory{
			{Key: "k\x1b[2J", Scope: "team\nx", Body: "b", UpdatedBy: "carol\u202e", UpdatedAt: at, Rev: 1},
		}}, "k\\x1b[2J (team\\nx) rev 1, carol\\u202e, 2026-10-09 08:00 UTC\n  b\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printMemories(&b, tc.res)
			if b.String() != tc.want {
				t.Errorf("printMemories =\n%s\nwant:\n%s", b.String(), tc.want)
			}
		})
	}
}

// Memory commands refuse bad usage before dialing.
func TestMemoryUsage(t *testing.T) {
	empty := t.TempDir()
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"remember needs text", []string{"-C", empty, "remember", "k"}, "remember needs a key and text"},
		{"forget needs a key", []string{"-C", empty, "forget"}, "forget needs exactly one key"},
		{"pin needs a key", []string{"-C", empty, "pin"}, "pin needs exactly one key"},
		{"unpin takes one key", []string{"-C", empty, "unpin", "a", "b"}, "unpin needs exactly one key"},
		{"memories takes no arguments", []string{"-C", empty, "memories", "x"}, "memories takes no arguments"},
		{"bad scope", []string{"-C", empty, "recall", "--scope", "world"}, "scope must be project, user or team"},
		{"help for remember", []string{"help", "remember"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, tc.args...)
			if tc.stderr == "" {
				if code != ExitOK || !strings.Contains(out, "usage: sfx remember KEY TEXT...|-") {
					t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
				}
				return
			}
			if code != ExitUsage || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: ") {
				t.Fatalf("exit %d (want %d), stderr %q, want it to name %q and a fix", code, ExitUsage, errb, tc.stderr)
			}
		})
	}
}
