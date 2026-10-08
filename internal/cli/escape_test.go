package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
)

// Hostile text from another principal, or from a hostile server (C-3):
// OSC 52 sets the clipboard, OSC 8 hides a link, CSI 2K with a carriage
// return rewrites the line, then a C1 CSI, a bidi override and a newline
// that would start a line of its own.
const (
	osc52  = "\x1b]52;c;cm0gLXJmIH4=\x07"
	osc8   = "\x1b]8;;https://evil.example.com\x1b\\here\x1b]8;;\x1b\\"
	erase  = "\x1b[2K\rall good"
	evil   = "x" + osc52 + osc8 + erase + "\u009b2J\u202e" + "\nFORGED line"
	evilMD = "body" + osc52 + osc8 + erase + "\u009b2J\u202e" + "\nsecond line"
)

// rawRunes must never reach a terminal.
var rawRunes = []string{"\x1b", "\x07", "\r", "\u009b", "\u202e"}

// checkOutput fails if out holds a raw control or bidi character, or a
// line that a single-line field forged.
func checkOutput(t *testing.T, name, out string) {
	t.Helper()
	for _, r := range rawRunes {
		if strings.Contains(out, r) {
			t.Errorf("%s: %q reached the output raw:\n%s", name, r, out)
		}
	}
	for l := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "FORGED") {
			t.Errorf("%s: a field started a line of its own:\n%s", name, out)
		}
	}
}

func TestPrintEscapesControl(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	closed := now
	sum := proto.Summary{ID: "sf-a" + evil, Title: evil, Status: "open" + evil, Priority: 1}
	issue := proto.Issue{ID: "sf-a", ParentID: "sf-p" + evil, Title: evil, Body: evilMD, Design: evilMD, Acceptance: evilMD,
		Notes: evilMD, Status: "closed", Type: "task" + evil, Assignee: evil, Owner: evil, CloseReason: evil, CreatedBy: evil,
		CreatedAt: now, UpdatedAt: now, ClosedAt: &closed, Labels: []string{evil, "ok"}}
	show := proto.ShowResult{Issue: issue,
		Deps:    []proto.Dep{{From: "sf-a", To: "sf-b" + evil, Type: "blocks" + evil}, {From: "sf-c" + evil, To: "sf-a", Type: "blocks"}},
		Claim:   &proto.Claim{ID: "sf-a", By: evil, Session: evil, Machine: evil, Epoch: 1, ExpiresAt: now},
		Items:   []proto.AcceptanceItem{{N: 1, Text: evil, State: "waived", Reason: evil, By: evil}, {N: 2, Text: evil, State: "ticked", By: evil}},
		Similar: []proto.Summary{sum},
		Handoff: &proto.Handoff{Author: evil, Body: evilMD, CreatedAt: now,
			State: "partial" + evil, Next: evil, Branch: evil, Worktree: evil, To: evil}}
	digestItem := proto.DigestItem{ID: "sf-d" + evil, Title: evil, By: evil, At: now, Note: evil, From: evil, BlockedBy: []string{evil}}
	tests := []struct {
		name  string
		print func(w *bytes.Buffer)
	}{
		{"show", func(w *bytes.Buffer) {
			printIssue(w, show)
			printItems(w, show.Items)
			printHandoff(w, show.Handoff)
			printSimilar(w, show.Similar)
		}},
		{"list", func(w *bytes.Buffer) { printSummaries(w, []proto.Summary{sum}, nil) }},
		{"blocked", func(w *bytes.Buffer) {
			printBlocked(w, []proto.BlockedIssue{{Summary: sum, BlockedBy: []string{evil}, Via: evil}})
		}},
		{"comments", func(w *bytes.Buffer) {
			printComments(w, []proto.Comment{{Author: evil, Kind: "handoff" + evil, Body: evilMD, CreatedAt: now}})
		}},
		{"history", func(w *bytes.Buffer) {
			after, _ := json.Marshal(map[string]string{"title": evil, "label": evil})
			if err := printHistory(w, []proto.Event{{Seq: 1, At: now, Principal: evil, Op: "issue.create", After: after},
				{Seq: 2, At: now, Principal: evil, Op: "label.add" + evil, After: after}}); err != nil {
				t.Error(err)
			}
		}},
		{"inbox", func(w *bytes.Buffer) {
			printItem(w, proto.InboxItem{ID: 1, Kind: "mention" + evil, Issue: "sf-a" + evil, Body: evil, From: evil, At: now})
		}},
		{"digest", func(w *bytes.Buffer) {
			items := []proto.DigestItem{digestItem}
			printDigest(w, proto.DigestResult{Since: now.Add(-time.Hour), Until: now,
				Totals: proto.DigestTotals{Closed: 1, Started: 1, InProgress: 1, Stalled: 1, Blocked: 1, HandedOff: 1, Created: 1, Discovered: 1},
				Closed: items, Started: items, InProgress: items, Stalled: items, Blocked: items, HandedOff: items, Created: items, Discovered: items})
		}},
		{"who", func(w *bytes.Buffer) {
			printWho(w, "", proto.WhoResult{Now: now, Agents: []proto.Agent{{Principal: evil, Session: evil, Machine: evil,
				Harness: evil, LastSeen: now, Claims: []string{evil}}}})
		}},
		{"server error", func(w *bytes.Buffer) {
			r := &runner{env: Env{Stderr: w}}
			r.fail(&proto.Error{Code: proto.CodeInvalid, Message: evil, Fix: evil})
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tc.print(&b)
			// Run writes through safetext.Writer; the printers alone
			// keep each single-line field on its line.
			var out bytes.Buffer
			w := safetext.NewWriter(&out)
			_, _ = w.Write(b.Bytes())
			_ = w.Flush()
			checkOutput(t, tc.name, out.String())
			for l := range strings.SplitSeq(b.String(), "\n") {
				if strings.HasPrefix(strings.TrimSpace(l), "FORGED") {
					t.Errorf("printer output has a forged line:\n%s", b.String())
				}
			}
		})
	}
}

// Run itself escapes everything it prints, in text and in JSON, and JSON
// stays valid.
func TestRunOutputIsEscaped(t *testing.T) {
	var out, errb bytes.Buffer
	r := &runner{env: Env{Stdout: &out, Stderr: &errb}}
	r.wrapOutput()
	_, _ = r.env.Stdout.Write([]byte(evilMD))
	r.fail(&proto.Error{Code: proto.CodeInvalid, Message: evil, Fix: evil})
	r.flush()
	checkOutput(t, "text", out.String()+errb.String())

	out.Reset()
	r.json = true
	r.emit(map[string]string{"title": evil})
	r.flush()
	checkOutput(t, "json", out.String())
	var back map[string]string
	if err := json.Unmarshal(out.Bytes(), &back); err != nil || back["title"] != evil {
		t.Errorf("--json output %q is not the same value as valid JSON: %v", out.String(), err)
	}
}
