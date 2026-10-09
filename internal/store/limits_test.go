package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLimitsDefaultsAndValidate(t *testing.T) {
	d := Limits{}.withDefaults()
	if d != DefaultLimits {
		t.Errorf("Limits{}.withDefaults() = %+v, want %+v", d, DefaultLimits)
	}
	if got := (Limits{Labels: 7}).withDefaults().Labels; got != 7 {
		t.Errorf("withDefaults kept Labels = %d, want 7", got)
	}
	for _, l := range []Limits{{Labels: -1}, {AcceptanceItems: -1}, {Deps: -1}, {Sessions: -1}, {InboxUnread: -1}, {Notices: -1},
		{UsageRecords: -1}, {UsagePerDay: -1}, {Paths: -1}, {MemoryBody: -1}, {MemoryTags: -1}, {MemoryTagLength: -1},
		{Memories: -1}, {MemoryKeyLength: -1}, {Prices: -1}} {
		if err := l.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v.Validate() = %v, want ErrInvalid", l, err)
		}
	}
	// A memory limit cannot pass its column.
	for _, tc := range []struct {
		l    Limits
		name string
	}{
		{Limits{MemoryBody: 65536}, "memory_body"}, {Limits{MemoryTagLength: 256}, "memory_tag_length"},
		{Limits{MemoryKeyLength: 256}, "memory_key_length"},
	} {
		if err := tc.l.Validate(); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("%+v.Validate() = %v, want ErrInvalid naming %s", tc.l, err, tc.name)
		}
	}
	if err := (Limits{MemoryBody: 65535, MemoryTagLength: 255, MemoryKeyLength: 255}).Validate(); err != nil {
		t.Errorf("memory limits at their columns' sizes: %v, want nil", err)
	}
	if err := (Limits{}).Validate(); err != nil {
		t.Errorf("Limits{}.Validate() = %v, want nil", err)
	}
}

// S-4: one request cannot hold the single writer with thousands of
// labels, acceptance items, ticks or edges.
func TestRequestCaps(t *testing.T) {
	s := openStore(t, newDSN(t), Options{Limits: Limits{Labels: 3, AcceptanceItems: 4, Deps: 2}})
	ctx := t.Context()
	labels := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("l%d", i)
		}
		return out
	}
	items := func(n int) string {
		var b strings.Builder
		for i := range n {
			fmt.Fprintf(&b, "- item %d\n", i)
		}
		return b.String()
	}
	is := mustCreate(t, s, NewIssue{Title: "capped", Labels: labels(3), Acceptance: items(4)})
	a, b, c := mustCreate(t, s, NewIssue{Title: "a"}), mustCreate(t, s, NewIssue{Title: "b"}), mustCreate(t, s, NewIssue{Title: "c"})
	if err := s.AddDep(ctx, alice, is.ID, a.ID, DepRelated); err != nil {
		t.Fatal(err)
	}
	if err := s.AddDep(ctx, alice, is.ID, b.ID, DepRelated); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		do   func() error
		want string
	}{
		{"create with too many labels", func() error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "x", Labels: labels(4)})
			return err
		}, "at most 3 labels"},
		{"create with duplicate labels under the cap", func() error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "x", Labels: []string{"a", "a", "b", "b"}})
			return err
		}, ""},
		{"label add over the cap", func() error { return s.AddLabel(ctx, alice, is.ID, "l9") }, "at most 3 labels"},
		{"label add of one it has", func() error { return s.AddLabel(ctx, alice, is.ID, "l0") }, ""},
		{"create with too many items", func() error {
			_, err := s.CreateIssue(ctx, alice, NewIssue{Title: "x", Acceptance: items(5)})
			return err
		}, "at most 4 acceptance items"},
		{"update to too many items", func() error {
			_, err := s.UpdateIssue(ctx, alice, a.ID, a.Rev, IssuePatch{Acceptance: ptr(items(5))})
			return err
		}, "at most 4 acceptance items"},
		{"tick more than the cap", func() error {
			_, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1, 2, 3}, Untick: []int{4}, Waive: map[int]string{5: "no"}})
			return err
		}, "at most 4 acceptance items"},
		{"tick a number past the cap", func() error {
			_, err := s.Accept(ctx, alice, is.ID, Acceptance{Tick: []int{1000000}})
			return err
		}, "at most 4 acceptance items"},
		{"dep over the cap", func() error { return s.AddDep(ctx, alice, is.ID, c.ID, DepRelated) }, "at most 2 dependencies"},
		{"dep it has", func() error { return s.AddDep(ctx, alice, is.ID, a.ID, DepRelated) }, ""},
		{"edge into a full issue", func() error { return s.AddDep(ctx, alice, c.ID, is.ID, DepRelated) }, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.do()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("err = %v, want nil", err)
			case tc.want != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want ErrInvalid naming %q", err, tc.want)
			}
		})
	}
}

// S-6: a principal's registry rows are capped, the least recently seen
// pruned first, and Prune drops rows not seen within the window, except
// each principal's latest, which keeps it mentionable.
func TestAgentRegistryBounded(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: clk.now, Limits: Limits{Sessions: 3}})
	ctx := t.Context()
	for i := range 5 {
		clk.add(time.Second)
		if err := s.TouchAgent(ctx, Actor{Principal: "mallory", Session: fmt.Sprintf("s%d", i), Machine: "m"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TouchAgent(ctx, bob, ""); err != nil {
		t.Fatal(err)
	}
	sessions := func() []string {
		t.Helper()
		as, err := s.Who(ctx, MaxAgentWindow)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range as {
			out = append(out, a.Principal+"/"+a.Session)
		}
		return out
	}
	if got, want := strings.Join(sessions(), " "), "bob/sess-b mallory/s4 mallory/s3 mallory/s2"; got != want {
		t.Errorf("after 5 mallory sessions with a cap of 3: %s, want %s", got, want)
	}

	clk.add(8 * 24 * time.Hour)
	if err := s.TouchAgent(ctx, alice, ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(ctx, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n.Agents != 2 {
		t.Errorf("Prune removed %d agent rows, want 2 (mallory's older sessions)", n.Agents)
	}
	var left []string
	rows, err := s.r.QueryContext(ctx, `SELECT principal, session FROM agents ORDER BY principal, session`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p, sess string
		if err := rows.Scan(&p, &sess); err != nil {
			t.Fatal(err)
		}
		left = append(left, p+"/"+sess)
	}
	_ = rows.Close()
	if got, want := strings.Join(left, " "), "alice/sess-a bob/sess-b mallory/s4"; got != want {
		t.Errorf("rows after Prune: %s, want %s", got, want)
	}
	// Pruning again changes nothing.
	if n, err := s.Prune(ctx, 7*24*time.Hour, 30*24*time.Hour); err != nil || n != (Pruned{}) {
		t.Errorf("second Prune = %+v, %v; want nothing pruned", n, err)
	}
}

// S-7: notices from one sender to one recipient are rate-limited, unread
// items are capped (the oldest marked read), and read items are purged.
func TestInboxBounded(t *testing.T) {
	clk := &clock{t: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}
	s := openStore(t, newDSN(t), Options{Now: clk.now, Limits: Limits{InboxUnread: 5, Notices: 3}})
	ctx := t.Context()
	victim := Actor{Principal: "victor", Session: "sv", Machine: "m"}
	if err := s.TouchAgent(ctx, victim, ""); err != nil {
		t.Fatal(err)
	}
	is := mustCreate(t, s, NewIssue{Title: "spam"})
	unread := func() int {
		t.Helper()
		p, err := s.Inbox(ctx, victim, false, 1)
		if err != nil {
			t.Fatal(err)
		}
		return p.Unread
	}
	for range 10 {
		if _, err := s.AddComment(ctx, bob, is.ID, "@victor look", ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := unread(); got != 3 {
		t.Errorf("10 mentions from bob in a minute left %d unread, want 3 (notices_per_minute)", got)
	}
	// Assignments count against the same sender's allowance.
	if _, err := s.CreateIssue(ctx, bob, NewIssue{Title: "more", Assignee: "victor"}); err != nil {
		t.Fatal(err)
	}
	if got := unread(); got != 3 {
		t.Errorf("an assignment over the allowance left %d unread, want 3", got)
	}
	// A minute later bob may notify again; with alice too, the cap of 5
	// marks the oldest read.
	clk.add(time.Minute)
	for range 3 {
		if _, err := s.AddComment(ctx, bob, is.ID, "@victor again", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddComment(ctx, alice, is.ID, "@victor also", ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := unread(); got != 5 {
		t.Errorf("unread = %d, want the cap of 5", got)
	}
	all, err := s.Inbox(ctx, victim, true, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 9 || all.Items[len(all.Items)-1].ReadAt == nil {
		t.Errorf("inbox --all has %d items, oldest read=%v; want 9 kept, the oldest marked read", len(all.Items), all.Items[len(all.Items)-1].ReadAt)
	}
	// Read items older than the keep window are purged; unread ones stay.
	clk.add(31 * 24 * time.Hour)
	n, err := s.Prune(ctx, 7*24*time.Hour, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n.Inbox != 4 {
		t.Errorf("Prune purged %d read items, want 4", n.Inbox)
	}
	if got := unread(); got != 5 {
		t.Errorf("unread after Prune = %d, want 5", got)
	}
}

// S-10: an event keeps a long text as its start, length and hash, not
// the whole text, so an edit loop cannot grow the log by 256 KiB a write.
func TestEventCompactsLongText(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	big := strings.Repeat("A", maxText)
	is := mustCreate(t, s, NewIssue{Title: "big", Body: big, Design: big, Notes: big})
	up, err := s.UpdateIssue(ctx, alice, is.ID, is.Rev, IssuePatch{Body: ptr(strings.Repeat("B", maxText))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddComment(ctx, bob, up.ID, strings.Repeat("C", maxText), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateIssue(ctx, alice, is.ID, up.Rev, IssuePatch{Title: ptr("still short")}); err != nil {
		t.Fatal(err)
	}
	evs, err := s.History(ctx, is.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 4 {
		t.Fatalf("history has %d events, want 4", len(evs))
	}
	for _, e := range evs {
		if n := len(e.Before) + len(e.After); n > 4*EventTextMax {
			t.Errorf("%s event holds %d bytes, want at most %d", e.Op, n, 4*EventTextMax)
		}
	}
	var after map[string]any
	if err := json.Unmarshal(evs[1].After, &after); err != nil {
		t.Fatal(err)
	}
	body, _ := after["body"].(string)
	if !strings.HasPrefix(body, "BBBB") || !strings.Contains(body, fmt.Sprintf("%d bytes", maxText)) || !strings.Contains(body, "sha256:") {
		t.Errorf("compacted body = %.80q…, want its start, length and sha256", body)
	}
	var c Comment
	if err := json.Unmarshal(evs[2].After, &c); err != nil {
		t.Errorf("a compacted comment event no longer decodes as a comment: %v", err)
	}
	var title map[string]any
	if err := json.Unmarshal(evs[3].After, &title); err != nil || title["title"] != "still short" {
		t.Errorf("short fields are kept whole: %s", evs[3].After)
	}
}

// S-13: show and create read closed titles from a cache, which a close or
// reopen through the store refreshes.
func TestSimilarCache(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	closeIt := func(title string) Issue {
		t.Helper()
		is := mustCreate(t, s, NewIssue{Title: title})
		out, err := s.CloseIssue(ctx, alice, is.ID, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ids := func(title string) string {
		t.Helper()
		sim, err := s.SimilarClosed(ctx, title, "", MaxSimilar)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range sim {
			out = append(out, string(x.ID))
		}
		return strings.Join(out, " ")
	}
	a := closeIt("reaper misses expired leases")
	if got := ids("expired leases reaper"); got != string(a.ID) {
		t.Fatalf("similar = %q, want %s", got, a.ID)
	}
	// A row written behind the store's back is not seen: the cache holds.
	if _, err := s.w.ExecContext(ctx, `UPDATE issues SET title = 'unrelated words entirely', write_id = 1 WHERE id = ?`, string(a.ID)); err != nil {
		t.Fatal(err)
	}
	if got := ids("expired leases reaper"); got != string(a.ID) {
		t.Errorf("similar after a direct write = %q, want the cached %s", got, a.ID)
	}
	// A close through the store refreshes it.
	b := closeIt("expired leases outlive the reaper")
	if got := ids("expired leases reaper"); got != string(b.ID) {
		t.Errorf("similar after a close = %q, want only %s", got, b.ID)
	}
	if _, err := s.ReopenIssue(ctx, alice, b.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got := ids("expired leases reaper"); got != "" {
		t.Errorf("similar after a reopen = %q, want none", got)
	}
}
