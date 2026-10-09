package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustLogHours(t *testing.T, s *Store, a Actor, in NewHours) HoursEntry {
	t.Helper()
	e, err := s.LogHours(t.Context(), a, in)
	if err != nil {
		t.Fatalf("LogHours(%s, %s, %s): %v", a.Principal, in.Issue, in.Duration, err)
	}
	return e
}

// entriesText renders entries for comparison: "who issue date duration note".
func entriesText(es []HoursEntry) string {
	var out []string
	for _, e := range es {
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %s %s %s", e.Principal, e.Issue, e.On.Format(time.DateOnly), e.Duration, e.Note)))
	}
	return strings.Join(out, "; ")
}

func TestLogHours(t *testing.T) {
	s, clk := clockStore(t) // 2026-10-07 12:00 UTC
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	before := lastSeq(t, s)

	e := mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: 90 * time.Minute, Note: "pairing"})
	if e.ID == "" || e.Principal != "alice" || e.Issue != is.ID || e.Duration != 90*time.Minute ||
		!e.On.Equal(day("2026-10-07")) || e.Note != "pairing" || !e.At.Equal(clk.now()) {
		t.Errorf("LogHours(90m, today) = %+v, want alice's 1h30m on 2026-10-07 at %s", e, clk.now())
	}
	mustLogHours(t, s, bob, NewHours{Issue: is.ID, Duration: 2 * time.Hour, On: day("2026-10-06")})

	evs, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	var logged []Event
	for _, ev := range evs {
		if ev.Seq > before && ev.Op == OpHoursLog {
			logged = append(logged, ev)
		}
	}
	if len(logged) != 2 {
		t.Fatalf("History(%s) has %d hours.log events, want 2", is.ID, len(logged))
	}
	var after struct {
		ID      string `json:"id"`
		On      string `json:"on"`
		Seconds int64  `json:"seconds"`
		Note    string `json:"note"`
	}
	if err := json.Unmarshal(logged[0].After, &after); err != nil {
		t.Fatal(err)
	}
	if after.ID != e.ID || after.On != "2026-10-07" || after.Seconds != 5400 || after.Note != "pairing" {
		t.Errorf("hours.log after state = %+v, want entry %s, 2026-10-07, 5400 s, pairing", after, e.ID)
	}

	list, err := s.Hours(ctx, HoursFilter{Issue: is.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := entriesText(list.Entries), fmt.Sprintf("alice %[1]s 2026-10-07 1h30m0s pairing; bob %[1]s 2026-10-06 2h0m0s", is.ID); got != want {
		t.Errorf("Hours(issue) = %q, want %q", got, want)
	}
}

// A retry with the same idempotency key logs nothing more and returns the
// first entry, even once the day has turned.
func TestLogHoursIdempotent(t *testing.T) {
	s, clk := clockStore(t)
	is := mustCreate(t, s, NewIssue{Title: "work"})
	in := NewHours{Issue: is.ID, Duration: time.Hour, Idem: "cli-1"}
	first := mustLogHours(t, s, alice, in)
	again := mustLogHours(t, s, alice, in)
	if again.ID != first.ID {
		t.Errorf("LogHours retried = entry %s, want the first, %s", again.ID, first.ID)
	}
	// A retry after midnight is the same request: it named no day.
	clk.add(13 * time.Hour)
	if late := mustLogHours(t, s, alice, in); late.ID != first.ID || !late.On.Equal(first.On) {
		t.Errorf("LogHours retried the next day = entry %s on %s, want the first, %s on %s", late.ID, late.On, first.ID, first.On)
	}
	list, err := s.Hours(t.Context(), HoursFilter{Issue: is.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 {
		t.Errorf("Hours after a retried log = %d entries, want 1", len(list.Entries))
	}
}

func TestLogHoursRefuses(t *testing.T) {
	s, _ := clockStore(t) // 2026-10-07 12:00 UTC
	is := mustCreate(t, s, NewIssue{Title: "work"})
	tests := []struct {
		name string
		in   NewHours
		err  error
		text string
	}{
		{"no issue", NewHours{Issue: "tst-none", Duration: time.Hour}, ErrNotFound, "tst-none"},
		{"zero", NewHours{Issue: is.ID}, ErrInvalid, "duration"},
		{"under a minute", NewHours{Issue: is.ID, Duration: 59 * time.Second}, ErrInvalid, "duration"},
		{"part of a second", NewHours{Issue: is.ID, Duration: time.Minute + time.Millisecond}, ErrInvalid, "duration"},
		{"over a day", NewHours{Issue: is.ID, Duration: 24*time.Hour + time.Minute}, ErrInvalid, "24h"},
		{"two days on", NewHours{Issue: is.ID, Duration: time.Hour, On: day("2026-10-09")}, ErrInvalid, "future"},
		{"over a year back", NewHours{Issue: is.ID, Duration: time.Hour, On: day("2025-10-06")}, ErrInvalid, "year"},
		{"not a date", NewHours{Issue: is.ID, Duration: time.Hour, On: day("2026-10-06").Add(time.Hour)}, ErrInvalid, "date"},
		{"note too long", NewHours{Issue: is.ID, Duration: time.Hour, Note: strings.Repeat("x", 501)}, ErrInvalid, "note"},
		{"note on two lines", NewHours{Issue: is.ID, Duration: time.Hour, Note: "a\nb"}, ErrInvalid, "note"},
		{"bad idempotency key", NewHours{Issue: is.ID, Duration: time.Hour, Idem: "-x"}, ErrInvalid, "idempotency"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.LogHours(t.Context(), alice, tc.in)
			if !errors.Is(err, tc.err) || !strings.Contains(err.Error(), tc.text) {
				t.Errorf("LogHours(%+v) = %v, want %v naming %q", tc.in, err, tc.err, tc.text)
			}
		})
	}
	// A year back exactly is in range, as is 24h; so is tomorrow in UTC,
	// already today for a logger east of it.
	mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: 24 * time.Hour, On: day("2025-10-07")})
	mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: time.Hour, On: day("2026-10-08")})
}

// A principal's entries on one day are bounded in number (limits:
// hours_per_day) and sum to at most 24 hours; another day, or another
// principal, is not affected.
func TestLogHoursPerDay(t *testing.T) {
	s := openStore(t, newDSN(t), Options{Limits: Limits{HoursPerDay: 3}, Now: func() time.Time {
		return time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	}})
	is := mustCreate(t, s, NewIssue{Title: "work"})
	for range 3 {
		mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: time.Hour})
	}
	_, err := s.LogHours(t.Context(), alice, NewHours{Issue: is.ID, Duration: time.Hour})
	var capErr *HoursLimitError
	if !errors.As(err, &capErr) || !errors.Is(err, ErrInvalid) || capErr.Max != 3 {
		t.Errorf("LogHours past hours_per_day 3 = %v, want a *HoursLimitError of 3 wrapping ErrInvalid", err)
	}
	mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: time.Hour, On: day("2026-10-06")})
	mustLogHours(t, s, bob, NewHours{Issue: is.ID, Duration: time.Hour})

	// 21h already on 2026-10-05; 3h more fits, a minute past does not.
	mustLogHours(t, s, bob, NewHours{Issue: is.ID, Duration: 21 * time.Hour, On: day("2026-10-05")})
	_, err = s.LogHours(t.Context(), bob, NewHours{Issue: is.ID, Duration: 3*time.Hour + time.Minute, On: day("2026-10-05")})
	if full, ok := errors.AsType[*HoursDayError](err); !ok || !errors.Is(err, ErrInvalid) || full.Logged != 21*time.Hour {
		t.Errorf("LogHours taking a day past 24h = %v, want a *HoursDayError with 21h logged, wrapping ErrInvalid", err)
	}
	mustLogHours(t, s, bob, NewHours{Issue: is.ID, Duration: 3 * time.Hour, On: day("2026-10-05")})
}

// A retried undo, with the same idempotency key, returns the entry it
// undid rather than not found; and a retried log whose entry was since
// undone says so.
func TestHoursRetriedAfterUndo(t *testing.T) {
	s, _ := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	logged := NewHours{Issue: is.ID, Duration: time.Hour, Idem: "cli-log"}
	e := mustLogHours(t, s, alice, logged)
	if _, err := s.DeleteHours(ctx, alice, e.ID, "cli-undo"); err != nil {
		t.Fatal(err)
	}
	seq := lastSeq(t, s)
	got, err := s.DeleteHours(ctx, alice, e.ID, "cli-undo")
	if err != nil || got.ID != e.ID {
		t.Errorf("DeleteHours(%s) retried = %+v, %v; want the entry it undid", e.ID, got, err)
	}
	if _, err := s.DeleteHours(ctx, alice, e.ID, "cli-other"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteHours(%s) anew after its undo = %v, want ErrNotFound", e.ID, err)
	}
	again := mustLogHours(t, s, alice, logged)
	if again.ID != e.ID || !again.Undone {
		t.Errorf("LogHours retried after its undo = %+v, want entry %s marked undone", again, e.ID)
	}
	if lastSeq(t, s) != seq {
		t.Error("a retried undo or log wrote an event")
	}
}

// A person may undo their own entry and an admin anyone's; anyone else
// is refused. Each undo records an event on the issue.
func TestDeleteHours(t *testing.T) {
	s, _ := clockStore(t)
	ctx := t.Context()
	is := mustCreate(t, s, NewIssue{Title: "work"})
	mine := mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: time.Hour, Note: "mine"})
	theirs := mustLogHours(t, s, bob, NewHours{Issue: is.ID, Duration: 2 * time.Hour})
	other := mustLogHours(t, s, bob, NewHours{Issue: is.ID, Duration: 3 * time.Hour})

	seq := lastSeq(t, s)
	_, err := s.DeleteHours(ctx, alice, theirs.ID, "")
	var forbidden *ForbiddenError
	if !errors.As(err, &forbidden) {
		t.Errorf("DeleteHours(bob's entry) as alice = %v, want a *ForbiddenError", err)
	}
	if lastSeq(t, s) != seq {
		t.Error("a refused DeleteHours wrote an event")
	}
	got, err := s.DeleteHours(ctx, alice, mine.ID, "")
	if err != nil || got.ID != mine.ID || got.Note != "mine" {
		t.Errorf("DeleteHours(own entry) = %+v, %v; want the entry removed", got, err)
	}
	if _, err := s.DeleteHours(ctx, dana, theirs.ID, ""); err != nil {
		t.Errorf("DeleteHours(bob's entry) as admin = %v, want nil", err)
	}
	if _, err := s.DeleteHours(ctx, alice, mine.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteHours(an entry already undone) = %v, want ErrNotFound", err)
	}
	if _, err := s.DeleteHours(ctx, alice, "not an id!", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("DeleteHours(malformed id) = %v, want ErrInvalid", err)
	}
	list, err := s.Hours(ctx, HoursFilter{Issue: is.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].ID != other.ID {
		t.Errorf("Hours after two undos = %q, want only bob's 3h", entriesText(list.Entries))
	}
	evs, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	var undone []string
	for _, ev := range evs {
		if ev.Op == OpHoursDelete {
			undone = append(undone, ev.Actor.Principal)
		}
	}
	if strings.Join(undone, ",") != "dana,alice" && strings.Join(undone, ",") != "alice,dana" {
		t.Errorf("hours.delete events by %v, want alice's and dana's", undone)
	}
}

// Hours lists entries newest day first, filtered by issue and by who
// logged them, and counts the entries past the limit.
func TestHoursList(t *testing.T) {
	s, _ := clockStore(t)
	a, b := mustCreate(t, s, NewIssue{Title: "a"}), mustCreate(t, s, NewIssue{Title: "b"})
	mustLogHours(t, s, alice, NewHours{Issue: a.ID, Duration: time.Hour, On: day("2026-10-05")})
	mustLogHours(t, s, alice, NewHours{Issue: b.ID, Duration: 2 * time.Hour, On: day("2026-10-07")})
	mustLogHours(t, s, bob, NewHours{Issue: a.ID, Duration: 3 * time.Hour, On: day("2026-10-06")})
	tests := []struct {
		name string
		f    HoursFilter
		want string
		more int
	}{
		{"all", HoursFilter{}, "alice B 2026-10-07 2h0m0s; bob A 2026-10-06 3h0m0s; alice A 2026-10-05 1h0m0s", 0},
		{"issue", HoursFilter{Issue: a.ID}, "bob A 2026-10-06 3h0m0s; alice A 2026-10-05 1h0m0s", 0},
		{"person", HoursFilter{Principal: "alice"}, "alice B 2026-10-07 2h0m0s; alice A 2026-10-05 1h0m0s", 0},
		{"both", HoursFilter{Issue: a.ID, Principal: "alice"}, "alice A 2026-10-05 1h0m0s", 0},
		{"limit", HoursFilter{Limit: 1}, "alice B 2026-10-07 2h0m0s", 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			list, err := s.Hours(t.Context(), tc.f)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.NewReplacer(" A ", " "+string(a.ID)+" ", " B ", " "+string(b.ID)+" ").Replace(tc.want)
			if got := entriesText(list.Entries); got != want || list.More != tc.more {
				t.Errorf("Hours(%+v) = %q and %d more, want %q and %d more", tc.f, got, list.More, want, tc.more)
			}
		})
	}
	for _, f := range []HoursFilter{{Limit: -1}, {Limit: MaxHoursList + 1}, {Issue: "bad id!"}} {
		if _, err := s.Hours(t.Context(), f); !errors.Is(err, ErrInvalid) {
			t.Errorf("Hours(%+v) = %v, want ErrInvalid", f, err)
		}
	}
}

// show's usage carries the issue's logged hours, in total and by who
// logged them, most first; another issue's hours are not its.
func TestIssueUsageHours(t *testing.T) {
	s, _ := clockStore(t)
	is, other := mustCreate(t, s, NewIssue{Title: "work"}), mustCreate(t, s, NewIssue{Title: "other"})
	mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: time.Hour})
	mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: 30 * time.Minute, On: day("2026-01-01")})
	mustLogHours(t, s, bob, NewHours{Issue: is.ID, Duration: 2 * time.Hour})
	mustLogHours(t, s, bob, NewHours{Issue: other.ID, Duration: 5 * time.Hour})
	u := mustUsage(t, s, is.ID)
	var by []string
	for _, p := range u.LoggedBy {
		by = append(by, fmt.Sprintf("%s %s", p.Principal, p.Duration))
	}
	if u.Logged != 3*time.Hour+30*time.Minute || strings.Join(by, ", ") != "bob 2h0m0s, alice 1h30m0s" {
		t.Errorf("IssueUsage(%s) logged %s by %v, want 3h30m0s by bob 2h0m0s, alice 1h30m0s", is.ID, u.Logged, by)
	}
	if u := mustUsage(t, s, mustCreate(t, s, NewIssue{Title: "none"}).ID); u.Logged != 0 || len(u.LoggedBy) != 0 {
		t.Errorf("IssueUsage(an issue with no hours) logged %s by %v, want none", u.Logged, u.LoggedBy)
	}
}

// An entry for tomorrow in UTC, already today for a logger east of it,
// counts in a digest and in a cost report that reach now, though no UTC
// moment of its day has come yet.
func TestHoursAheadOfUTC(t *testing.T) {
	s, _ := clockStore(t) // 2026-10-07 12:00 UTC
	is := mustCreate(t, s, NewIssue{Title: "work"})
	mustLogHours(t, s, alice, NewHours{Issue: is.ID, Duration: time.Hour, On: day("2026-10-08")})
	d, err := s.Digest(t.Context(), DigestFilter{Window: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if d.Usage.Logged != time.Hour {
		t.Errorf("Digest(last hour).Usage = %+v, want 1h logged", d.Usage)
	}
	for _, until := range []time.Time{{}, day("2026-10-07").Add(12 * time.Hour)} {
		r, err := s.CostReport(t.Context(), CostFilter{By: CostByPerson, Since: day("2026-10-07"), Until: until})
		if err != nil {
			t.Fatal(err)
		}
		if r.Total.Logged != time.Hour {
			t.Errorf("CostReport(from 7 Oct to %s).Total.Logged = %s, want 1h", until, r.Total.Logged)
		}
	}
	r, err := s.CostReport(t.Context(), CostFilter{By: CostByPerson, Since: day("2026-10-06"), Until: day("2026-10-07")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Total.Logged != 0 {
		t.Errorf("CostReport(6 Oct).Total.Logged = %s, want none", r.Total.Logged)
	}
}

// A digest adds up the hours of the days its window overlaps, so a
// window from mid-morning counts that whole day, filtered as its other
// sections are: by who logged them, or by the issue's label.
func TestDigestUsageHours(t *testing.T) {
	s, _ := clockStore(t) // 2026-10-07 12:00 UTC
	web, plain := mustCreate(t, s, NewIssue{Title: "web", Labels: []string{"web"}}), mustCreate(t, s, NewIssue{Title: "plain"})
	mustLogHours(t, s, alice, NewHours{Issue: web.ID, Duration: time.Hour})                              // today
	mustLogHours(t, s, alice, NewHours{Issue: plain.ID, Duration: 2 * time.Hour, On: day("2026-10-06")}) // yesterday
	mustLogHours(t, s, bob, NewHours{Issue: web.ID, Duration: 4 * time.Hour, On: day("2026-10-05")})     // two days back
	tests := []struct {
		name string
		f    DigestFilter
		want time.Duration
	}{
		{"24h reaches into yesterday", DigestFilter{Window: 24 * time.Hour}, 3 * time.Hour},
		{"from midnight today", DigestFilter{Since: day("2026-10-07")}, time.Hour},
		{"three days", DigestFilter{Window: 72 * time.Hour}, 7 * time.Hour},
		{"by bob", DigestFilter{Window: 72 * time.Hour, By: "bob"}, 4 * time.Hour},
		{"label web", DigestFilter{Window: 72 * time.Hour, Label: "web"}, 5 * time.Hour},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := s.Digest(t.Context(), tc.f)
			if err != nil {
				t.Fatal(err)
			}
			if d.Usage.Logged != tc.want {
				t.Errorf("Digest(%+v).Usage.Logged = %s, want %s", tc.f, d.Usage.Logged, tc.want)
			}
		})
	}
}
