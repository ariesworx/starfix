package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// ids lists a section's issue ids, then its total: "a b /3".
func ids(sec DigestSection) string {
	var out []string
	for _, it := range sec.Items {
		out = append(out, string(it.ID))
	}
	return strings.Join(out, " ") + fmt.Sprintf(" /%d", sec.Total)
}

func TestDigest(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	clk := &clock{t: t0}
	s := openStore(t, newDSN(t), Options{Now: clk.now})
	ctx := t.Context()
	carol := Actor{Principal: "carol", Session: "sess-c", Machine: "laptop-c"}
	create := func(a Actor, title string, p Priority, labels ...string) IssueID {
		t.Helper()
		is, err := s.CreateIssue(ctx, a, NewIssue{Title: title, Priority: &p, Labels: labels})
		if err != nil {
			t.Fatal(err)
		}
		return is.ID
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	// Before the 24h window that ends at t0+10d.
	old := create(alice, "old", P2)
	stale := create(alice, "stale", P2)
	clk.t = t0.Add(24 * time.Hour)
	_, _, err := s.StartIssue(ctx, alice, stale, 0)
	must(err)
	clk.t = t0.Add(8 * 24 * time.Hour)
	closedEarly := create(bob, "closed early", P2)
	_, err = s.CloseIssue(ctx, bob, closedEarly, 0, "")
	must(err)

	// Exactly on the window's start: included.
	clk.t = t0.Add(9 * 24 * time.Hour)
	edge := create(carol, "edge", P2)

	// Inside the window.
	clk.t = t0.Add(9*24*time.Hour + 12*time.Hour)
	a := create(bob, "a", P1, "ui")
	_, _, err = s.StartIssue(ctx, bob, a, 0)
	must(err)
	_, found, err := s.FinishIssue(ctx, bob, a, 0, Finish{Reason: "done", Handoff: HandoffNote{Note: strings.Repeat("n", 300)},
		Discovered: []NewIssue{{Title: "found"}}})
	must(err)
	b := create(carol, "b", P0, "ui")
	must(s.AddDep(ctx, carol, b, old, DepBlocks))
	fresh := create(alice, "fresh", P3)
	clk.t = t0.Add(9*24*time.Hour + 23*time.Hour)
	_, _, err = s.StartIssue(ctx, alice, fresh, 0)
	must(err)

	clk.t = t0.Add(10 * 24 * time.Hour)
	day := DigestFilter{Window: 24 * time.Hour}
	tests := []struct {
		name string
		f    DigestFilter
		want map[string]string
	}{
		{"last 24h", day, map[string]string{
			"closed":      string(a) + " /1",
			"started":     string(fresh) + " " + string(a) + " /2",
			"in progress": string(stale) + " " + string(fresh) + " /2",
			"stalled":     string(stale) + " /1",
			"blocked":     string(b) + " /1",
			"handed off":  string(a) + " /1",
			"created":     string(b) + " " + string(a) + " " + string(found[0]) + " " + string(edge) + " " + string(fresh) + " /5",
			"discovered":  string(found[0]) + " /1",
		}},
		{"by bob", DigestFilter{Window: 24 * time.Hour, By: "bob"}, map[string]string{
			"closed": string(a) + " /1", "started": string(a) + " /1", "in progress": " /0", "stalled": " /0",
			"blocked": " /0", "handed off": string(a) + " /1", "created": string(a) + " " + string(found[0]) + " /2",
			"discovered": string(found[0]) + " /1",
		}},
		{"by alice holds the stalled one", DigestFilter{Window: 24 * time.Hour, By: "alice"}, map[string]string{
			"closed": " /0", "in progress": string(stale) + " " + string(fresh) + " /2", "stalled": string(stale) + " /1",
			"created": string(fresh) + " /1",
		}},
		{"label ui", DigestFilter{Window: 24 * time.Hour, Label: "ui"}, map[string]string{
			"closed": string(a) + " /1", "started": string(a) + " /1", "in progress": " /0", "blocked": string(b) + " /1",
			"created": string(b) + " " + string(a) + " /2", "discovered": string(found[0]) + " /1",
		}},
		{"since a time", DigestFilter{Since: t0.Add(9*24*time.Hour + time.Second)}, map[string]string{
			"created": string(b) + " " + string(a) + " " + string(found[0]) + " " + string(fresh) + " /4",
		}},
		{"ten days", DigestFilter{Window: 10 * 24 * time.Hour}, map[string]string{
			"closed":  string(a) + " " + string(closedEarly) + " /2",
			"started": string(fresh) + " " + string(a) + " " + string(stale) + " /3",
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := s.Digest(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{
				"closed": ids(d.Closed), "started": ids(d.Started), "in progress": ids(d.InProgress),
				"stalled": ids(d.Stalled), "blocked": ids(d.Blocked), "handed off": ids(d.HandedOff),
				"created": ids(d.Created), "discovered": ids(d.Discovered),
			}
			for k, want := range tc.want {
				if strings.TrimSpace(got[k]) != strings.TrimSpace(want) {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
			if d.Capped || !d.Now.Equal(clk.t) {
				t.Errorf("capped %v now %v", d.Capped, d.Now)
			}
		})
	}

	// Who, when and what each section reports.
	d, err := s.Digest(ctx, day)
	must(err)
	if !d.Since.Equal(t0.Add(9*24*time.Hour)) || d.Events == 0 {
		t.Fatalf("since %v events %d", d.Since, d.Events)
	}
	if it := d.Closed.Items[0]; it.By != "bob" || it.Title != "a" || it.Priority != P1 || !it.At.Equal(t0.Add(9*24*time.Hour+12*time.Hour)) {
		t.Errorf("closed: %+v", it)
	}
	if it := d.InProgress.Items[0]; it.By != "alice" || !it.At.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("in progress: %+v", it)
	}
	if it := d.Stalled.Items[0]; it.By != "alice" || !it.At.Equal(t0.Add(24*time.Hour)) {
		t.Errorf("stalled: %+v", it)
	}
	if it := d.HandedOff.Items[0]; it.By != "bob" || len(it.Note) != digestNote+len("…") || !strings.HasSuffix(it.Note, "…") {
		t.Errorf("handed off: %+v", it)
	}
	if it := d.Blocked.Items[0]; it.By != "" || !it.At.IsZero() || !slices.Equal(it.BlockedBy, []IssueID{old}) {
		t.Errorf("blocked: %+v", it)
	}
	if it := d.Discovered.Items[0]; it.From != a || it.By != "bob" {
		t.Errorf("discovered: %+v", it)
	}

	// A comment is activity: stale is no longer stalled.
	_, err = s.AddComment(ctx, alice, stale, "still on it", "")
	must(err)
	if d, err := s.Digest(ctx, day); err != nil || d.Stalled.Total != 0 || d.InProgress.Total != 2 {
		t.Fatalf("after a comment: stalled %+v, %v", d.Stalled, err)
	}

	// Reading changes nothing.
	seq := lastSeq(t, s)
	if _, err := s.Digest(ctx, day); err != nil || lastSeq(t, s) != seq {
		t.Fatalf("digest wrote: %v", err)
	}
}

func TestDigestCaps(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	for i := range 12 {
		is := mustCreate(t, s, NewIssue{Title: fmt.Sprintf("i%02d", i), Priority: prio(Priority(4 - i%5))})
		if _, _, err := s.StartIssue(ctx, alice, is.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	d, err := s.Digest(ctx, DigestFilter{Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if d.Created.Total != 12 || len(d.Created.Items) != DigestCreated || d.Created.Items[0].Priority != P0 ||
		d.Created.Items[1].Priority != P0 || d.Created.Items[2].Priority != P1 {
		t.Errorf("created %d, %+v", d.Created.Total, d.Created.Items)
	}
	if d.InProgress.Total != 12 || len(d.InProgress.Items) != DigestItems || d.Started.Total != 12 || len(d.Started.Items) != DigestItems {
		t.Errorf("in progress %d/%d, started %d/%d", len(d.InProgress.Items), d.InProgress.Total, len(d.Started.Items), d.Started.Total)
	}
}

func TestDigestRefuses(t *testing.T) {
	s := newStore(t)
	future := time.Now().Add(time.Hour)
	for _, f := range []DigestFilter{
		{},
		{Window: -time.Hour},
		{Since: future},
		{Window: MaxDigestWindow + time.Hour},
		{Window: time.Hour, Label: "two words"},
		{Window: time.Hour, By: "bad\x00name"},
	} {
		if _, err := s.Digest(t.Context(), f); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v, want ErrInvalid", f, err)
		}
	}
}
