package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// The soak's checker must catch what it claims to: these feed it event
// logs that break one invariant each, and one that breaks none, without
// a store.

// logBuilder writes an event log, a minute of store time per event
// unless told otherwise.
type logBuilder struct {
	t0  time.Time
	seq int64
	at  time.Duration
	evs []sevent
}

func newLog() *logBuilder {
	return &logBuilder{t0: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)}
}

// ts is the store time d after the log began, as JSON.
func (b *logBuilder) ts(d time.Duration) string {
	return `"` + b.t0.Add(d).Format(time.RFC3339Nano) + `"`
}

// add appends an event dt after the previous one.
func (b *logBuilder) add(dt time.Duration, who sessKey, op, target, before, after string) *logBuilder {
	b.seq++
	b.at += dt
	b.evs = append(b.evs, sevent{seq: b.seq, at: b.t0.Add(b.at), actor: who, op: op, target: target,
		before: []byte(before), after: []byte(after)})
	return b
}

func (b *logBuilder) create(who sessKey, id string) *logBuilder {
	return b.add(time.Minute, who, "issue.create", id, "",
		fmt.Sprintf(`{"id": %q, "title": "task %s", "status": "open", "type": "task", "priority": 2, "created_at": %s}`, id, id, b.ts(b.at+time.Minute)))
}

// take records who taking id at epoch, with a lease of lease; prev, when
// set, is the claim it replaced.
func (b *logBuilder) take(who sessKey, id string, epoch int64, lease time.Duration, prev string) *logBuilder {
	return b.add(time.Minute, who, "claim.take", id, prev,
		fmt.Sprintf(`{"epoch": %d, "expires_at": %s}`, epoch, b.ts(b.at+time.Minute+lease)))
}

// claimOf is a claim's before state.
func (b *logBuilder) claimOf(who sessKey, epoch int64, expires time.Duration) string {
	return fmt.Sprintf(`{"holder": {"principal": %q, "session": %q}, "epoch": %d, "expires_at": %s}`,
		who.principal, who.session, epoch, b.ts(expires))
}

func TestReplayCatches(t *testing.T) {
	alice, alice2, bob := sessKey{"alice", "s1"}, sessKey{"alice", "s2"}, sessKey{"bob", "s1"}
	tests := []struct {
		name string
		log  func(b *logBuilder) *logBuilder
		want string // a substring of the one violation; empty: none
		// guards and overs count the checks deferred to the end of a run.
		guards, overs int
	}{
		{
			name: "a clean story",
			log: func(b *logBuilder) *logBuilder {
				return b.create(alice, "sf-1").
					take(alice, "sf-1", 1, 15*time.Minute, "").
					add(0, alice, "issue.update", "sf-1", `{"status": "open"}`, `{"status": "in_progress", "assignee": "alice"}`).
					add(time.Minute, bob, "comment.add", "sf-1", "", `{"body": "note bob-1", "kind": "note"}`).
					add(time.Minute, alice, "claim.release", "sf-1", b.claimOf(alice, 1, 17*time.Minute), "").
					add(0, alice, "issue.close", "sf-1", `{"status": "in_progress"}`, `{"status": "closed", "close_reason": "done alice-2"}`)
			},
		},
		{
			name: "a close of an issue still held",
			log: func(b *logBuilder) *logBuilder {
				return b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "").
					add(time.Minute, alice, "issue.close", "sf-1", `{"status": "in_progress"}`, `{"status": "closed", "close_reason": "done alice-2"}`)
			},
			want: "closes sf-1 while alice/s1 holds epoch 1",
		},
		{
			name: "a release, then another principal's take",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				b.add(time.Minute, alice, "claim.release", "sf-1", b.claimOf(alice, 1, 17*time.Minute), "").
					add(0, alice, "comment.add", "sf-1", "", `{"body": "handoff alice-2", "kind": "handoff"}`)
				return b.take(bob, "sf-1", 2, 15*time.Minute, "")
			},
		},
		{
			name: "a release of a claim no one held",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1")
				return b.add(time.Minute, alice, "claim.release", "sf-1", b.claimOf(alice, 1, 17*time.Minute), "")
			},
			want: "releases alice/s1 at epoch 1, but no one held epoch 1",
		},
		{
			name: "a release naming the wrong claim",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				return b.add(time.Minute, alice2, "claim.release", "sf-1", b.claimOf(alice2, 1, 17*time.Minute), "")
			},
			want: "releases alice/s2 at epoch 1, but alice/s1 held epoch 1",
		},
		{
			name: "a gap in seq",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1")
				b.seq++
				return b.add(time.Minute, alice, "comment.add", "sf-1", "", `{"body": "x"}`)
			},
			want: "the log has a gap",
		},
		{
			name: "time running backward",
			log: func(b *logBuilder) *logBuilder {
				return b.create(alice, "sf-1").add(-2*time.Minute, bob, "comment.add", "sf-1", "", `{"body": "x"}`)
			},
			want: "is earlier than event",
		},
		{
			name: "an event on an issue never created",
			log: func(b *logBuilder) *logBuilder {
				return b.add(time.Minute, alice, "comment.add", "sf-9", "", `{"body": "x"}`)
			},
			want: "which no event created",
		},
		{
			name: "an issue created twice",
			log: func(b *logBuilder) *logBuilder {
				return b.create(alice, "sf-1").create(bob, "sf-1")
			},
			want: "creates issue sf-1 again",
		},
		{
			name: "another principal takes a live claim",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				return b.take(bob, "sf-1", 2, 15*time.Minute, b.claimOf(alice, 1, 17*time.Minute))
			},
			want: "bob/s1 took epoch 2 while alice/s1's claim ran to",
		},
		{
			name: "another principal takes a lapsed claim",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, time.Minute, "")
				b.at += 5 * time.Minute
				return b.take(bob, "sf-1", 2, 15*time.Minute, b.claimOf(alice, 1, 3*time.Minute))
			},
		},
		{
			name: "a session of the same principal takes a live claim",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				return b.take(alice2, "sf-1", 2, 15*time.Minute, b.claimOf(alice, 1, 17*time.Minute))
			},
			overs: 1,
		},
		{
			name: "an epoch skipped",
			log: func(b *logBuilder) *logBuilder {
				return b.create(alice, "sf-1").take(alice, "sf-1", 2, 15*time.Minute, "")
			},
			want: "takes epoch 2 after epoch 0",
		},
		{
			name: "a take that does not name the holder it replaced",
			log: func(b *logBuilder) *logBuilder {
				return b.create(alice, "sf-1").take(alice, "sf-1", 1, time.Minute, "").take(bob, "sf-1", 2, time.Minute, "")
			},
			want: "without naming alice/s1",
		},
		{
			name: "a take that names the wrong holder",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, time.Minute, "")
				b.at += 5 * time.Minute
				return b.take(bob, "sf-1", 2, time.Minute, b.claimOf(alice2, 1, 3*time.Minute))
			},
			want: "replaces alice/s2 at epoch 1, but alice/s1 held epoch 1",
		},
		{
			name: "a claim expired by someone other than the reaper",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, time.Minute, "")
				return b.add(5*time.Minute, bob, "claim.expire", "sf-1", b.claimOf(alice, 1, 3*time.Minute), "")
			},
			want: "claim.expire by bob/s1, not the reaper",
		},
		{
			name: "the reaper ends a lease early",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				return b.add(time.Minute, reaper, "claim.expire", "sf-1", b.claimOf(alice, 1, 17*time.Minute), "")
			},
			want: "before its lease ran out",
		},
		{
			name: "an edge added twice",
			log: func(b *logBuilder) *logBuilder {
				edge := `{"from": "sf-1", "to": "sf-2", "type": "blocks"}`
				return b.create(alice, "sf-1").create(alice, "sf-2").
					add(time.Minute, bob, "dep.add", "sf-1", "", edge).
					add(time.Minute, bob, "dep.add", "sf-1", "", edge)
			},
			want: "which exists",
		},
		{
			name: "an override by someone not an admin",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				return b.add(time.Minute, bob, "admin.override", "sf-1", "", `{"holder": {"principal": "alice", "session": "s1"}, "epoch": 1}`)
			},
			want: "who is not an admin",
		},
		{
			name: "a change by another principal under a claim",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				return b.add(time.Minute, bob, "issue.update", "sf-1", `{"title": "a"}`, `{"title": "retitled bob-1"}`)
			},
			guards: 1,
		},
		{
			name: "a close by another principal under a claim, after its release",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				b.add(time.Minute, bob, "claim.release", "sf-1", b.claimOf(alice, 1, 17*time.Minute), "")
				return b.add(0, bob, "issue.close", "sf-1", `{"status": "in_progress"}`, `{"status": "closed", "close_reason": "closed bob-1"}`)
			},
			guards: 1, // the close, made under alice's claim, which its release ended
		},
		{
			name: "a change by an admin under a claim, with its override",
			log: func(b *logBuilder) *logBuilder {
				b.create(alice, "sf-1").take(alice, "sf-1", 1, 15*time.Minute, "")
				b.add(time.Minute, sessKey{"dave", "s1"}, "admin.override", "sf-1", "", `{"holder": {"principal": "alice", "session": "s1"}, "epoch": 1}`)
				return b.add(0, sessKey{"dave", "s1"}, "issue.update", "sf-1", `{"title": "a"}`, `{"title": "retitled dave-1"}`)
			},
		},
		{
			name: "an op the soak does not expect",
			log: func(b *logBuilder) *logBuilder {
				return b.create(alice, "sf-1").add(time.Minute, alice, "issue.vanish", "sf-1", "", "")
			},
			want: `op "issue.vanish"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			r := newReplay(func(_, format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, []string{"dave"})
			r.feed(tc.log(newLog()).evs)
			r.flush()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("replay found %q, want no violation", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("replay found %q, want one violation containing %q", got, tc.want)
			}
			if len(r.guards) != tc.guards || len(r.overs) != tc.overs {
				t.Errorf("replay deferred %d guard and %d takeover checks, want %d and %d", len(r.guards), len(r.overs), tc.guards, tc.overs)
			}
		})
	}
}

func TestReplayCompare(t *testing.T) {
	base := func() tables {
		return tables{
			issues: map[string]tIssue{"sf-1": {title: "task sf-1", status: "open", typ: "task", priority: 2, rev: 1}},
			labels: map[string]map[string]bool{}, deps: map[depKey]bool{}, claims: map[string]rClaim{},
		}
	}
	tests := []struct {
		name   string
		change func(*tables)
		want   string
	}{
		{name: "the same", change: func(*tables) {}},
		{name: "a lost update", change: func(tb *tables) {
			is := tb.issues["sf-1"]
			is.rev = 2
			tb.issues["sf-1"] = is
		}, want: "the table has"},
		{name: "a row no event made", change: func(tb *tables) {
			tb.issues["sf-2"] = tIssue{title: "ghost"}
		}, want: "no event created it"},
		{name: "a label no event added", change: func(tb *tables) {
			tb.labels["sf-1"] = map[string]bool{"ui": true}
		}, want: "labels [ui] in the table"},
		{name: "an edge no event added", change: func(tb *tables) {
			tb.deps[depKey{"sf-1", "sf-2", "blocks"}] = true
		}, want: "in the deps table but not in the event log"},
		{name: "a claim no event took", change: func(tb *tables) {
			tb.claims["sf-1"] = rClaim{holder: sessKey{"bob", "s1"}, epoch: 1}
		}, want: "no event took it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			r := newReplay(func(_, format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, nil)
			r.feed(newLog().create(sessKey{"alice", "s1"}, "sf-1").evs)
			r.flush()
			tb := base()
			tc.change(&tb)
			r.compare(tb)
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("compare found %q, want nothing", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0], tc.want)):
				t.Errorf("compare found %q, want one violation containing %q", got, tc.want)
			}
		})
	}
}

func TestMonitorCatches(t *testing.T) {
	alice, alice2 := sessKey{"alice", "s1"}, sessKey{"alice", "s2"}
	exp := time.Date(2026, 1, 2, 3, 15, 0, 0, time.UTC)
	start := func(who sessKey, epoch int64) call {
		return call{who: who, op: proto.OpStart, out: acked, args: proto.StartArgs{ID: "sf-1"},
			res: proto.StartResult{Issue: proto.Issue{ID: "sf-1"}, Claim: &proto.Claim{ID: "sf-1", By: who.principal, Session: who.session, Epoch: epoch, ExpiresAt: exp}}}
	}
	update := func(who sessKey, rev, got int64, title string) call {
		return call{who: who, op: proto.OpUpdate, out: acked, args: proto.UpdateArgs{ID: "sf-1", Rev: rev, Title: &title},
			res: proto.WriteResult{ID: "sf-1", Rev: got}}
	}
	tests := []struct {
		name  string
		calls []call
		want  string
	}{
		{name: "one epoch each", calls: []call{start(alice, 1), start(alice2, 2), start(alice2, 2)}},
		{name: "one epoch acknowledged to two sessions", calls: []call{start(alice, 1), start(alice2, 1)}, want: "acknowledged to two sessions"},
		{name: "a start answered with another's claim", calls: []call{{who: alice, op: proto.OpStart, out: acked, args: proto.StartArgs{},
			res: proto.StartResult{Claim: &proto.Claim{ID: "sf-1", By: "bob", Session: "s1", Epoch: 1, ExpiresAt: exp}}}}, want: "answered a claim held by bob/s1"},
		{name: "updates in turn", calls: []call{update(alice, 1, 2, "a"), update(alice2, 2, 3, "b")}},
		{name: "an update answered a rev it skipped", calls: []call{update(alice, 1, 3, "a")}, want: "update at rev 1 answered rev 3"},
		{name: "two updates acknowledged one rev", calls: []call{update(alice, 1, 2, "a"), update(alice2, 1, 2, "b")}, want: "a lost update"},
		{name: "a refused update is not checked", calls: []call{update(alice, 1, 2, "a"), {who: alice2, op: proto.OpUpdate, out: refused,
			args: proto.UpdateArgs{ID: "sf-1", Rev: 1}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := newMonitor()
			for _, c := range tc.calls {
				m.record(c)
			}
			got := m.violations()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("monitor found %v, want nothing", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0].msg, tc.want)):
				t.Errorf("monitor found %v, want one violation containing %q", got, tc.want)
			}
		})
	}
}

func TestCheckMarkers(t *testing.T) {
	alice := sessKey{"alice", "s1"}
	comment := func(out outcome) call {
		return call{who: alice, op: proto.OpComment, out: out, args: proto.CommentArgs{ID: "sf-1", Body: "note alice-1"}}
	}
	tests := []struct {
		name  string
		calls []call
		times int // events carrying the marker
		want  string
	}{
		{name: "acknowledged and applied", calls: []call{comment(acked)}, times: 1},
		{name: "acknowledged and missing", calls: []call{comment(acked)}, want: "in the event log 0 times; acked true"},
		{name: "applied twice", calls: []call{comment(lost), comment(acked)}, times: 2, want: "in the event log 2 times"},
		{name: "lost and applied", calls: []call{comment(lost)}, times: 1},
		{name: "lost and not applied", calls: []call{comment(lost)}},
		{name: "refused and applied", calls: []call{comment(refused)}, times: 1, want: "acked false, refused true"},
		{name: "applied, never sent", times: 1, want: "but no session sent it"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &soak{mon: newMonitor(), rep: newReplay(func(string, string, ...any) {}, nil)}
			if tc.times > 0 {
				s.rep.markers["note alice-1"] = tc.times
			}
			s.checkMarkers(tc.calls)
			got := s.mon.violations()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("checkMarkers found %v, want nothing", got)
			case tc.want != "" && (len(got) != 1 || !strings.Contains(got[0].msg, tc.want)):
				t.Errorf("checkMarkers found %v, want one violation containing %q", got, tc.want)
			}
		})
	}
}

func TestCheckFencing(t *testing.T) {
	alice, bob := sessKey{"alice", "s1"}, sessKey{"bob", "s1"}
	finish := func(out outcome, epoch int64) call {
		return call{who: alice, op: proto.OpFinish, out: out, args: proto.FinishArgs{ID: "sf-1", Epoch: epoch, Reason: "done alice-1"}}
	}
	tests := []struct {
		name string
		call call
		at   atEpoch // the claim the close was made under
		want string
	}{
		{name: "under its epoch", call: finish(acked, 2), at: atEpoch{epoch: 2, holder: alice}},
		{name: "after its lease lapsed, before anyone took it", call: finish(acked, 2), at: atEpoch{epoch: 2}},
		{name: "under a later epoch", call: finish(acked, 1), at: atEpoch{epoch: 2, holder: bob}, want: "fenced to epoch 1 was applied under epoch 2"},
		{name: "lost, then applied under a later epoch", call: finish(lost, 1), at: atEpoch{epoch: 2, holder: bob}, want: "fenced to epoch 1"},
		{name: "under its epoch, held by another", call: finish(acked, 2), at: atEpoch{epoch: 2, holder: bob}, want: "while bob/s1 held it"},
		{name: "unfenced", call: finish(acked, 0), at: atEpoch{epoch: 2, holder: bob}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &soak{mon: newMonitor(), rep: newReplay(func(string, string, ...any) {}, nil)}
			tc.at.issue = "sf-1"
			s.rep.closedAt["done alice-1"] = tc.at
			s.checkFencing([]call{tc.call})
			got := s.mon.violations()
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("checkFencing found %v, want nothing", got)
			case tc.want != "" && (len(got) == 0 || !strings.Contains(got[0].msg, tc.want)):
				t.Errorf("checkFencing found %v, want a violation containing %q", got, tc.want)
			}
		})
	}
}

// A claim that ends with no event of its own, as a releasing handoff of
// an issue not in progress once did, fails the replay: the next take
// replaces a holder the log says is still there.
func TestReplaySilentRelease(t *testing.T) {
	alice, bob := sessKey{"alice", "s1"}, sessKey{"bob", "s1"}
	b := newLog().create(alice, "sf-1").take(alice, "sf-1", 1, time.Minute, "")
	b.add(0, alice, "issue.update", "sf-1", `{"status": "open"}`, `{"status": "in_progress", "assignee": "alice"}`)
	b.at += 5 * time.Minute // the lease lapsed; the reaper has not run
	b.add(time.Minute, bob, "issue.update", "sf-1", `{"status": "in_progress", "assignee": "alice"}`, `{"status": "blocked", "assignee": null}`)
	b.add(time.Minute, alice, "comment.add", "sf-1", "", `{"body": "handoff alice-2", "kind": "handoff"}`) // no claim.release
	b.take(bob, "sf-1", 2, time.Minute, "")
	var got []string
	r := newReplay(func(_, format string, args ...any) { got = append(got, fmt.Sprintf(format, args...)) }, nil)
	r.feed(b.evs)
	r.flush()
	if want := "without naming alice/s1"; len(got) != 1 || !strings.Contains(got[0], want) {
		t.Errorf("replay of a silent release found %q, want one violation containing %q", got, want)
	}
}
