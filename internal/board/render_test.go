package board

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ariesworx/starfix/internal/proto"
)

var update = flag.Bool("update", false, "rewrite the golden frames in testdata")

// golden compares frame with testdata/name.golden, where an escape shows
// as ^[ so the file reads in an editor, or rewrites it with -update.
func golden(t *testing.T, name string, frame []string) {
	t.Helper()
	got := strings.ReplaceAll(strings.Join(frame, "\n"), "\x1b", "^[") + "\n"
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // a golden file named by the test
	if err != nil {
		t.Fatalf("%v (run go test -update to write it)", err)
	}
	if got != string(want) {
		t.Errorf("frame %s differs from %s:\n--- got\n%s--- want\n%s", name, path, got, want)
	}
}

// live is the board of snapshot, connected, with a short tail.
func live() *Model {
	m := New("demo")
	m.Apply(snapshot())
	m.Apply(Status{State: StateLive})
	m.Apply(event(1, "claim.take", "sf-h1"))
	m.Apply(event(2, "issue.close", "sf-x9"))
	return m
}

func TestFrame(t *testing.T) {
	now := t0.Add(30 * time.Second)
	hostile := func() *Model {
		m := live()
		s := snapshot()
		s.Ready[0].Title = "Clear \x1b[2J the screen \u202eevil"
		s.Held[0].Claim.By = "mallory\x07"
		m.Apply(s)
		m.Apply(Pushed{proto.Event{Seq: 3, At: t0, Principal: "eve", Session: "s\x1b]0;x\x07", Op: "issue.update\n", Issue: "sf-r1"}})
		return m
	}
	detail := func() *Model {
		m := live()
		m.Key(KeyDown)
		m.Key(KeyEnter)
		m.Apply(&Detail{ID: "sf-r2", Show: &proto.ShowResult{
			Issue: proto.Issue{ID: "sf-r2", Title: "Tidy the logs", Status: "open", Priority: 2, Type: "task",
				Assignee: "bob", Labels: []string{"ops"}, Body: "The logs repeat.\nKeep one line per request, and say which \x1b[31mrequest\x1b[0m it was."},
			Deps:  []proto.Dep{{From: "sf-r2", To: "sf-r1", Type: "blocks"}},
			Items: []proto.AcceptanceItem{{N: 1, Text: "one line per request", State: "ticked"}, {N: 2, Text: "request ids"}},
			Files: &proto.Files{Paths: []proto.FilePath{{Path: "internal/log/log.go", Source: "declared"}},
				Overlaps: []proto.Overlap{{ID: "sf-h1", By: "alice", Session: "cc-1234"}}},
		}})
		return m
	}
	tests := []struct {
		name          string
		model         func() *Model
		width, height int
		color         bool
	}{
		{"wide", live, 100, 16, false},
		{"wide-color", live, 100, 16, true},
		{"narrow", live, 50, 20, false},
		{"short", live, 100, 8, false},
		{"empty", func() *Model { return New("demo") }, 80, 10, false},
		{"reconnecting", func() *Model {
			m := live()
			m.Apply(Status{State: StateReconnecting, Note: "the server closed the connection"})
			return m
		}, 80, 10, false},
		{"hostile", hostile, 100, 12, false},
		{"detail", detail, 80, 22, false},
		{"detail-loading", func() *Model { m := live(); m.Key(KeyEnter); return m }, 80, 6, false},
		{"help", func() *Model { m := live(); m.Key(KeyHelp); return m }, 80, 18, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			golden(t, tc.name, tc.model().Frame(tc.width, tc.height, now, tc.color))
		})
	}
}

// Every frame fills the height exactly, no line is wider than the
// terminal, and nothing printed can carry a control character other than
// the board's own color codes.
func TestFrameFits(t *testing.T) {
	now := t0.Add(time.Hour)
	models := map[string]*Model{"board": live(), "empty": New("a long project name that will not fit")}
	d := live()
	d.Key(KeyEnter)
	d.Apply(&Detail{ID: "sf-r1", Show: &proto.ShowResult{Issue: proto.Issue{ID: "sf-r1", Title: strings.Repeat("long ", 40),
		Body: strings.Repeat("word ", 300)}}})
	models["detail"] = d
	h := live()
	h.Key(KeyHelp)
	models["help"] = h
	for name, m := range models {
		for _, w := range []int{1, 2, 10, 30, 59, 60, 61, 120, 300} {
			for _, ht := range []int{1, 2, 3, 5, 12, 40} {
				for _, color := range []bool{false, true} {
					frame := m.Frame(w, ht, now, color)
					if len(frame) != ht {
						t.Errorf("%s at %dx%d: %d lines, want %d", name, w, ht, len(frame), ht)
					}
					for i, l := range frame {
						if n := visible(l); n > w {
							t.Errorf("%s at %dx%d (color %v): line %d is %d wide: %q", name, w, ht, color, i, n, l)
						}
						if !color && strings.ContainsAny(l, "\x1b\x07\r\n") {
							t.Errorf("%s at %dx%d: line %d has a control character: %q", name, w, ht, i, l)
						}
					}
				}
			}
		}
	}
}

// visible is the width of l on a terminal: its runes, less color codes.
func visible(l string) int {
	n := 0
	for i := 0; i < len(l); {
		if strings.HasPrefix(l[i:], "\x1b[") {
			j := strings.IndexByte(l[i:], 'm')
			if j < 0 {
				return n + utf8.RuneCountInString(l[i:])
			}
			i += j + 1
			continue
		}
		_, size := utf8.DecodeRuneInString(l[i:])
		i += size
		n++
	}
	return n
}
