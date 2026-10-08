package board

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

var t0 = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// snapshot is a board with two ready issues, one held and one blocked.
func snapshot() *Snapshot {
	return &Snapshot{
		Ready: []proto.Summary{{ID: "sf-r1", Title: "Write the parser", Status: "open", Priority: 1},
			{ID: "sf-r2", Title: "Tidy the logs", Status: "open", Priority: 2, Overlaps: []string{"sf-h1"}}},
		Held: []Held{{Claim: proto.Claim{ID: "sf-h1", By: "alice", Session: "cc-1234", Machine: "laptop", Epoch: 1,
			ClaimedAt: t0.Add(-42 * time.Minute), ExpiresAt: t0.Add(5 * time.Minute)}, Title: "Fix the lexer", Priority: 0}},
		Blocked: []proto.BlockedIssue{{Summary: proto.Summary{ID: "sf-b1", Title: "Ship it", Status: "open", Priority: 2},
			BlockedBy: []string{"sf-r1", "sf-h1"}}},
		ServerNow: t0, LocalNow: t0,
	}
}

func event(seq int64, op, issue string) Pushed {
	return Pushed{proto.Event{Seq: seq, At: t0.Add(time.Duration(seq) * time.Second), Principal: "bob", Session: "cli", Op: op, Issue: issue}}
}

// Which events change what the board lists: anything but comments,
// labels, acceptance items and the note of an admin's override, and any
// op the board does not know, to be safe.
func TestRelevant(t *testing.T) {
	for op, want := range map[string]bool{
		"issue.create": true, "issue.update": true, "issue.close": true, "issue.reopen": true, "issue.import": true,
		"claim.take": true, "claim.expire": true, "dep.add": true, "dep.remove": true, "issue.paths": true,
		"some.future.op": true,
		"comment.add":    false, "label.add": false, "label.remove": false, "acceptance.tick": false,
		"acceptance.untick": false, "acceptance.waive": false, "admin.override": false,
	} {
		if got := Relevant(op); got != want {
			t.Errorf("Relevant(%q) = %v, want %v", op, got, want)
		}
	}
}

// Pushed events join the tail, oldest dropped past TailMax; a snapshot
// replaces the lists and leaves the tail alone.
func TestModelEvents(t *testing.T) {
	m := New("demo")
	for i := range TailMax + 3 {
		m.Apply(event(int64(i+1), "issue.update", fmt.Sprintf("sf-%d", i)))
	}
	tail := m.Tail()
	if len(tail) != TailMax || tail[0].Seq != 4 || tail[len(tail)-1].Seq != TailMax+3 {
		t.Fatalf("tail = %d events, seq %d to %d; want %d, 4 to %d", len(tail), tail[0].Seq, tail[len(tail)-1].Seq, TailMax, TailMax+3)
	}
	m.Apply(snapshot())
	if got := m.Tail(); len(got) != TailMax {
		t.Errorf("after a snapshot the tail has %d events, want %d", len(got), TailMax)
	}
	if got := m.Selected(); got != "sf-r1" {
		t.Errorf("Selected() after the first snapshot = %q, want the first ready issue", got)
	}
}

// Keys move the selection through ready, held and blocked issues in that
// order, open an issue's detail and go back, toggle help, refresh and
// quit. The selection follows its issue across a new snapshot.
func TestModelKeys(t *testing.T) {
	type step struct {
		key  Key
		sel  string
		act  Action
		view View
	}
	none := Action{}
	tests := []struct {
		name  string
		steps []step
	}{
		{"move", []step{
			{KeyDown, "sf-r2", none, ViewBoard}, {KeyDown, "sf-h1", none, ViewBoard}, {KeyDown, "sf-b1", none, ViewBoard},
			{KeyDown, "sf-b1", none, ViewBoard}, {KeyUp, "sf-h1", none, ViewBoard}, {KeyHome, "sf-r1", none, ViewBoard},
			{KeyUp, "sf-r1", none, ViewBoard}, {KeyEnd, "sf-b1", none, ViewBoard}, {KeyPageUp, "sf-r1", none, ViewBoard},
			{KeyPageDown, "sf-b1", none, ViewBoard},
		}},
		{"detail and back", []step{
			{KeyDown, "sf-r2", none, ViewBoard},
			{KeyEnter, "sf-r2", Action{Do: DoShow, ID: "sf-r2"}, ViewDetail},
			{KeyDown, "sf-r2", none, ViewDetail}, // scrolls the detail
			{KeyEnter, "sf-r2", none, ViewDetail},
			{KeyBack, "sf-r2", Action{Do: DoHide}, ViewBoard},
			{KeyBack, "sf-r2", none, ViewBoard},
		}},
		{"help", []step{
			{KeyHelp, "sf-r1", none, ViewHelp}, {KeyDown, "sf-r1", none, ViewHelp}, {KeyHelp, "sf-r1", none, ViewBoard},
			{KeyHelp, "sf-r1", none, ViewHelp}, {KeyBack, "sf-r1", none, ViewBoard},
		}},
		{"refresh anywhere", []step{
			{KeyRefresh, "sf-r1", Action{Do: DoRefresh}, ViewBoard},
			{KeyEnter, "sf-r1", Action{Do: DoShow, ID: "sf-r1"}, ViewDetail},
			{KeyRefresh, "sf-r1", Action{Do: DoRefresh}, ViewDetail},
		}},
		{"quit from the board", []step{{KeyQuit, "sf-r1", Action{Do: DoQuit}, ViewBoard}}},
		{"quit from help", []step{{KeyHelp, "sf-r1", none, ViewHelp}, {KeyQuit, "sf-r1", Action{Do: DoQuit}, ViewHelp}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New("demo")
			m.Apply(snapshot())
			for i, st := range tc.steps {
				act := m.Key(st.key)
				if act != st.act || m.Selected() != st.sel || m.View() != st.view {
					t.Fatalf("step %d: Key(%v) = %+v, selected %q, view %v; want %+v, %q, %v",
						i, st.key, act, m.Selected(), m.View(), st.act, st.sel, st.view)
				}
			}
		})
	}
}

func TestModelSelectionFollowsItsIssue(t *testing.T) {
	m := New("demo")
	m.Apply(snapshot())
	m.Key(KeyDown)
	m.Key(KeyDown) // sf-h1
	s := snapshot()
	s.Ready = s.Ready[1:]
	m.Apply(s)
	if got := m.Selected(); got != "sf-h1" {
		t.Errorf("after sf-r1 left, Selected() = %q, want sf-h1 still", got)
	}
	s = snapshot()
	s.Ready, s.Held = s.Ready[:1], nil
	m.Apply(s)
	if got := m.Selected(); got != "sf-b1" {
		t.Errorf("after sf-h1 was released, Selected() = %q, want the row now in its place, sf-b1", got)
	}
	m.Apply(&Snapshot{})
	if got, act := m.Selected(), m.Key(KeyEnter); got != "" || act != (Action{}) {
		t.Errorf("on an empty board, Selected() = %q and Enter = %+v; want none", got, act)
	}
}

// A detail arriving for another issue than the one open, say after a
// quick back and forth, is dropped.
func TestModelDetail(t *testing.T) {
	m := New("demo")
	m.Apply(snapshot())
	m.Key(KeyEnter)
	m.Apply(&Detail{ID: "sf-r2", Show: &proto.ShowResult{Issue: proto.Issue{ID: "sf-r2"}}})
	if d := m.Detail(); d != nil {
		t.Errorf("Detail() = %+v after a stale reply, want none yet", d)
	}
	m.Apply(&Detail{ID: "sf-r1", Show: &proto.ShowResult{Issue: proto.Issue{ID: "sf-r1"}}})
	if d := m.Detail(); d == nil || d.ID != "sf-r1" {
		t.Errorf("Detail() = %+v, want sf-r1's", d)
	}
	if got := m.View(); got != ViewDetail {
		t.Errorf("View() = %v, want detail", got)
	}
}

// Scrolling a detail stops at its end, so going back up moves at once.
func TestModelDetailScrollStops(t *testing.T) {
	m := New("demo")
	m.Apply(snapshot())
	m.Resize(40, 6) // four rows of body
	m.Key(KeyEnter)
	body := ""
	for i := range 10 {
		body += fmt.Sprintf("line %d\n", i)
	}
	m.Apply(&Detail{ID: "sf-r1", Show: &proto.ShowResult{Issue: proto.Issue{ID: "sf-r1", Title: "t", Body: body}}})
	for range 50 {
		m.Key(KeyDown)
	}
	m.Key(KeyUp)
	frame := m.Frame(40, 6, t0, false)
	// The detail is 13 lines: title, facts, a blank and ten lines, the
	// final newline dropped. The last four start at "line 6", and one up
	// at "line 5".
	if got := frame[1]; got != "line 5" {
		t.Errorf("after scrolling past the end and back one, the first row is %q, want %q\n%s", got, "line 5", strings.Join(frame, "\n"))
	}
}
