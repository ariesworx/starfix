package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

func TestParseSince(t *testing.T) {
	tests := []struct {
		in     string
		at     time.Time
		window time.Duration
		bad    bool
	}{
		{in: "", window: 24 * time.Hour},
		{in: "90m", window: 90 * time.Minute},
		{in: "24h", window: 24 * time.Hour},
		{in: "7d", window: 7 * 24 * time.Hour},
		{in: "2w", window: 14 * 24 * time.Hour},
		{in: "2026-10-06T09:30:00Z", at: time.Date(2026, 10, 6, 9, 30, 0, 0, time.UTC)},
		{in: "2026-10-06T09:30:00-05:00", at: time.Date(2026, 10, 6, 14, 30, 0, 0, time.UTC)},
		{in: "2026-10-06", at: time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)},
		{in: "0h", bad: true},
		{in: "-24h", bad: true},
		{in: "0d", bad: true},
		{in: "-3d", bad: true},
		{in: "+3d", bad: true},
		{in: "1.5d", bad: true},
		{in: "99999999d", bad: true},
		{in: "d", bad: true},
		{in: "yesterday", bad: true},
		{in: "2026-13-01", bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			at, window, err := parseSince(tc.in)
			if tc.bad {
				if !errors.Is(err, store.ErrInvalid) {
					t.Fatalf("accepted: %v %v %v", at, window, err)
				}
				return
			}
			if err != nil || !at.Equal(tc.at) || window != tc.window {
				t.Fatalf("got %v %v %v, want %v %v", at, window, err, tc.at, tc.window)
			}
		})
	}
}

func TestDispatchDigest(t *testing.T) {
	s := newServer(t)
	p1 := 1
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "first", Priority: &p1, Labels: []string{"ui"}})
	b := mustCall[proto.WriteResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "second"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: a.ID})
	f := mustCall[proto.FinishResult](t, s, alice, proto.OpFinish, proto.FinishArgs{ID: a.ID, Handoff: "over to you",
		Discovered: []proto.Discovered{{Title: "found"}}})
	mustCall[proto.Empty](t, s, bob, proto.OpDepAdd, proto.DepArgs{From: b.ID, To: f.Created[0]})

	d := mustCall[proto.DigestResult](t, s, bob, proto.OpDigest, proto.DigestArgs{})
	if got := d.Until.Sub(d.Since); got != 24*time.Hour {
		t.Errorf("default window %v", got)
	}
	want := proto.DigestTotals{Events: d.Totals.Events, Closed: 1, Started: 1, Blocked: 1, HandedOff: 1, Created: 3, Discovered: 1}
	if d.Totals != want || d.Totals.Events < 8 || d.Truncated {
		t.Fatalf("totals %+v", d.Totals)
	}
	if c := d.Closed[0]; c.ID != a.ID || c.By != "alice" || c.Priority != 1 || c.At.IsZero() {
		t.Errorf("closed %+v", c)
	}
	if h := d.HandedOff[0]; h.Note != "over to you" {
		t.Errorf("handed off %+v", h)
	}
	if x := d.Discovered[0]; x.ID != f.Created[0] || x.From != a.ID {
		t.Errorf("discovered %+v", x)
	}
	if bl := d.Blocked[0]; bl.ID != b.ID || len(bl.BlockedBy) != 1 || bl.BlockedBy[0] != f.Created[0] || !bl.At.IsZero() {
		t.Errorf("blocked %+v", bl)
	}

	byBob := mustCall[proto.DigestResult](t, s, bob, proto.OpDigest, proto.DigestArgs{Since: "7d", By: "bob"})
	if byBob.Totals.Created != 1 || byBob.Totals.Closed != 0 || byBob.Created[0].ID != b.ID {
		t.Errorf("by bob: %+v", byBob)
	}
	ui := mustCall[proto.DigestResult](t, s, bob, proto.OpDigest, proto.DigestArgs{Label: "ui"})
	if ui.Totals.Created != 1 || ui.Totals.Discovered != 1 || ui.Totals.Blocked != 0 {
		t.Errorf("label ui: %+v", ui.Totals)
	}

	for _, tc := range []struct {
		name string
		args proto.DigestArgs
		msg  string
	}{
		{"bad since", proto.DigestArgs{Since: "yesterday"}, `since "yesterday" must be`},
		{"future", proto.DigestArgs{Since: "2999-01-01"}, "in the future"},
		{"too far back", proto.DigestArgs{Since: "400d"}, "more than 366 days"},
		{"bad label", proto.DigestArgs{Label: "a,b"}, "label"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.DigestResult](t, s, bob, proto.OpDigest, tc.args)
			if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, tc.msg) ||
				!strings.Contains(perr.Fix, "`sfx digest -h`") {
				t.Fatalf("got %+v", perr)
			}
		})
	}
}
