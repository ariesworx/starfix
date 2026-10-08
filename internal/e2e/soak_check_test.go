package e2e

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// The soak's invariants. The monitor checks what each acknowledged answer
// says as it arrives; the replay rebuilds the store from its event log,
// checking each transaction as it goes, and is compared with the tables
// in the same snapshot. Both record violations rather than stop the run,
// so a failure reports every broken invariant and the story of each
// issue involved.

// violation is one broken invariant, about issue when it names one.
type violation struct {
	issue string
	msg   string
}

// issueEpoch names one claim: an issue under one epoch.
type issueEpoch struct {
	issue string
	epoch int64
}

// takeRec is what the clients were told of one claim.
type takeRec struct {
	who sessKey
	// at is when the store took it, from the expiry it answered less the
	// lease asked for; zero when only a renewal reported the claim.
	at time.Time
	// maxExp is the latest expiry acknowledged for it, by start or renew.
	maxExp time.Time
	// take: the start asked to take over the principal's own live claim.
	take bool
}

// monitor checks answers as they arrive and keeps the journal.
type monitor struct {
	mu      sync.Mutex
	fails   []violation
	journal []call
	takes   map[issueEpoch]*takeRec
	// revs maps an issue's revision to the one write acknowledged with it.
	revs map[issueEpoch]string
	// writes counts acknowledged writes that produce a revision, so each
	// has an identity even without a marker.
	writes    int
	dialFails int
	retries   map[string]int
	vanishes  int
	forgotten int
}

func newMonitor() *monitor {
	return &monitor{takes: map[issueEpoch]*takeRec{}, revs: map[issueEpoch]string{}, retries: map[string]int{}}
}

// fail records a violation; issue may be empty.
func (m *monitor) fail(issue, format string, args ...any) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fails = append(m.fails, violation{issue: issue, msg: fmt.Sprintf(format, args...)})
}

func (m *monitor) dialFailed(sessKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dialFails++
}

func (m *monitor) retried(op string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries[op]++
}

func (m *monitor) vanished(sessKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vanishes++
}

func (m *monitor) forgot(sessKey, string, int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forgotten++
}

// leaseOf is the lease a start or renew asked for.
func leaseOf(s string) time.Duration {
	if s == "" {
		return 15 * time.Minute
	}
	d, err := proto.ParseDuration(s)
	if err != nil {
		return 15 * time.Minute
	}
	return d
}

// record journals cl and checks what it acknowledged.
func (m *monitor) record(cl call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.journal = append(m.journal, cl)
	if cl.out != acked {
		return
	}
	switch a := cl.args.(type) {
	case proto.StartArgs:
		r := cl.res.(proto.StartResult)
		c := r.Claim
		if c == nil {
			m.failLocked(r.Issue.ID, "%s: start answered no claim", cl.who)
			return
		}
		if c.By != cl.who.principal || c.Session != cl.who.session {
			m.failLocked(c.ID, "%s: start answered a claim held by %s/%s", cl.who, c.By, c.Session)
		}
		k := issueEpoch{c.ID, c.Epoch}
		rec := m.takes[k]
		switch {
		case rec == nil:
			m.takes[k] = &takeRec{who: cl.who, at: c.ExpiresAt.Add(-leaseOf(a.Lease)), maxExp: c.ExpiresAt, take: a.Take}
		case rec.who != cl.who:
			m.failLocked(c.ID, "epoch %d acknowledged to two sessions: %s and %s", c.Epoch, rec.who, cl.who)
		default:
			rec.maxExp = maxTime(rec.maxExp, c.ExpiresAt)
			rec.take = rec.take || a.Take
		}
	case proto.RenewArgs:
		for _, c := range cl.res.(proto.ClaimsResult).Claims {
			if !a.All && (c.By != cl.who.principal || c.Session != cl.who.session) {
				m.failLocked(c.ID, "%s: renew answered %s/%s's claim (epoch %d)", cl.who, c.By, c.Session, c.Epoch)
				continue
			}
			k := issueEpoch{c.ID, c.Epoch}
			rec := m.takes[k]
			switch {
			case rec == nil:
				m.takes[k] = &takeRec{who: cl.who, maxExp: c.ExpiresAt}
			case rec.who != cl.who:
				m.failLocked(c.ID, "%s renewed epoch %d, which %s took", cl.who, c.Epoch, rec.who)
			default:
				rec.maxExp = maxTime(rec.maxExp, c.ExpiresAt)
			}
		}
	case proto.CreateArgs:
		m.revLocked(cl.res.(proto.CreateResult).ID, 1, "create "+a.Title)
	case proto.UpdateArgs:
		r := cl.res.(proto.WriteResult)
		if r.Rev != a.Rev+1 {
			m.failLocked(a.ID, "%s: update at rev %d answered rev %d", cl.who, a.Rev, r.Rev)
		}
		m.revLocked(a.ID, r.Rev, "update "+deref(a.Title))
	case proto.CloseArgs:
		m.revLocked(a.ID, cl.res.(proto.WriteResult).Rev, "close "+a.Reason)
	case proto.ReopenArgs:
		m.writes++
		m.revLocked(a.ID, cl.res.(proto.WriteResult).Rev, fmt.Sprintf("reopen #%d by %s", m.writes, cl.who))
	case proto.FinishArgs:
		m.revLocked(a.ID, cl.res.(proto.FinishResult).Rev, "finish "+a.Reason)
	}
}

func (m *monitor) failLocked(issue, format string, args ...any) {
	m.fails = append(m.fails, violation{issue: issue, msg: fmt.Sprintf(format, args...)})
}

// revLocked records that write was acknowledged with issue's rev. Two
// writes acknowledged with one revision means one overwrote the other.
func (m *monitor) revLocked(issue string, rev int64, write string) {
	k := issueEpoch{issue, rev}
	if prev, ok := m.revs[k]; ok && prev != write {
		m.failLocked(issue, "rev %d acknowledged to two writes, %q and %q: a lost update", rev, prev, write)
		return
	}
	m.revs[k] = write
}

// maxExp is the latest expiry acknowledged for a claim, or zero.
func (m *monitor) maxExp(k issueEpoch) time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.takes[k]; r != nil {
		return r.maxExp
	}
	return time.Time{}
}

// calls returns the journal so far.
func (m *monitor) calls() []call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.journal)
}

// violations returns the violations so far.
func (m *monitor) violations() []violation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.fails)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func stamp(t time.Time) string { return t.UTC().Format("15:04:05.000000") }

// sevent is one row of the events table.
type sevent struct {
	seq    int64
	at     time.Time
	actor  sessKey
	op     string
	target string
	before json.RawMessage
	after  json.RawMessage
	idem   string
}

// reaper is who the reaper's changes are recorded as.
var reaper = sessKey{"starfixd", "reaper"}

// rIssue is an issue as the replay rebuilds it.
type rIssue struct {
	id, title, status, typ, assignee, parent, closeReason string
	priority                                              int
	createdAt                                             time.Time
	labels                                                map[string]bool
	rev                                                   int64
}

// rClaim is an issue's claim as the replay rebuilds it. A releasing
// handoff of an issue not in progress and unassigned clears the holder
// with no event of its own, which the replay cannot tell from a handoff
// that did not release: maybeReleased marks the holder as possibly gone.
type rClaim struct {
	holder        sessKey
	epoch         int64
	maybeReleased bool
}

// depKey is one edge.
type depKey struct{ from, to, typ string }

// hold is a session's hold on an issue, for attribution: from its claim's
// take until the event that ended it, at the lease's expiry if that came
// first (design §12.1).
type hold struct {
	issue      string
	who        sessKey
	start, end time.Time
	open       bool
}

// ended is how a claim ended: its final lease expiry, when the event that
// ended it recorded one.
type ended struct {
	at, exp time.Time
	op      string
}

// guardCheck is a change to an issue another principal held under a claim
// whose lease the replay cannot yet tell had lapsed.
type guardCheck struct {
	issue string
	epoch int64
	at    time.Time
	actor sessKey
	op    string
	seq   int64
}

// takeover is a take of a claim another session of the same principal
// still held: it is right only if the start asked to take it.
type takeover struct {
	issue string
	epoch int64
	actor sessKey
	seq   int64
}

// notice is a claim.lost inbox item the log says must exist.
type notice struct {
	issue string
	to    sessKey
	at    time.Time
	seq   int64
}

// atEpoch is the claim an issue was under when a marked change was made.
type atEpoch struct {
	issue  string
	epoch  int64
	holder sessKey
	actor  sessKey
	seq    int64
}

// replay rebuilds the store from the event log, a transaction at a time,
// and checks each.
type replay struct {
	fail   func(issue, format string, args ...any)
	admins map[string]bool

	seq    int64
	lastAt time.Time
	issues map[string]*rIssue
	deps   map[depKey]bool
	claims map[string]*rClaim

	holds    map[string][]*hold // by issue, oldest first
	takers   map[issueEpoch]sessKey
	ends     map[issueEpoch]ended
	guards   []guardCheck
	overs    []takeover
	notices  []notice
	markers  map[string]int       // marker → events carrying it
	closedAt map[string]atEpoch   // close reason → claim then
	handedAt map[string]atEpoch   // handoff note → claim then
	usage    map[sessKey]int      // usage.add events by session
	causes   map[string]bool      // issue and time of every change, for claim.lost
	ops      map[string]int       // events by op
	created  map[string]time.Time // issue → created_at
	tx       []sevent             // events of the transaction being read
	txs      int

	mu        sync.Mutex // guards the replay between snapshots
	openCount int        // issues not closed, as of the last snapshot
}

func newReplay(fail func(string, string, ...any), admins []string) *replay {
	r := &replay{fail: fail, admins: map[string]bool{}, issues: map[string]*rIssue{}, deps: map[depKey]bool{},
		claims: map[string]*rClaim{}, holds: map[string][]*hold{}, takers: map[issueEpoch]sessKey{},
		ends: map[issueEpoch]ended{}, markers: map[string]int{}, closedAt: map[string]atEpoch{},
		handedAt: map[string]atEpoch{}, usage: map[sessKey]int{}, causes: map[string]bool{}, ops: map[string]int{},
		created: map[string]time.Time{}}
	for _, a := range admins {
		r.admins[a] = true
	}
	return r
}

// feed applies events, which continue the log; each transaction is
// applied once the next begins. A snapshot holds whole transactions, so
// call flush at its end.
func (r *replay) feed(evs []sevent) {
	for _, e := range evs {
		if e.seq != r.seq+1 {
			r.fail(e.target, "event seq %d follows %d: the log has a gap", e.seq, r.seq)
		}
		if e.at.Before(r.lastAt) {
			r.fail(e.target, "event %d at %s is earlier than event %d at %s", e.seq, stamp(e.at), r.seq, stamp(r.lastAt))
		}
		r.seq, r.lastAt = e.seq, e.at
		r.ops[e.op]++
		if n := len(r.tx); n > 0 && (r.tx[n-1].at != e.at || r.tx[n-1].actor != e.actor) {
			r.flush()
		}
		r.tx = append(r.tx, e)
	}
}

// flush applies the transaction read so far.
func (r *replay) flush() {
	tx := r.tx
	r.tx = nil
	if len(tx) == 0 {
		return
	}
	r.txs++
	r.apply(tx)
}

// txFacts are what a transaction does to each issue, for rules that look
// at the whole transaction.
type txFacts struct {
	create, update, handoff, override map[string]bool
}

func factsOf(tx []sevent) txFacts {
	f := txFacts{create: map[string]bool{}, update: map[string]bool{}, handoff: map[string]bool{}, override: map[string]bool{}}
	for _, e := range tx {
		switch e.op {
		case "issue.create":
			f.create[e.target] = true
		case "issue.update":
			f.update[e.target] = true
		case "admin.override":
			f.override[e.target] = true
		case "comment.add":
			var c struct {
				Kind string `json:"kind"`
			}
			if json.Unmarshal(e.after, &c) == nil && c.Kind == "handoff" {
				f.handoff[e.target] = true
			}
		}
	}
	return f
}

func (r *replay) apply(tx []sevent) {
	f := factsOf(tx)
	for _, e := range tx {
		if e.op != "usage.add" {
			r.causes[causeKey(e.target, e.at)] = true
			if _, ok := r.issues[e.target]; !ok && e.op != "issue.create" {
				r.fail(e.target, "event %d (%s) is on issue %s, which no event created", e.seq, e.op, e.target)
				continue
			}
		}
		switch e.op {
		case "issue.create":
			r.create(e)
		case "issue.update":
			if e.actor != reaper {
				r.guard(e, f)
			}
			r.update(e, f)
		case "issue.close":
			r.guard(e, f)
			r.close(e)
		case "issue.reopen":
			r.guard(e, f)
			r.patch(e)
			r.issues[e.target].rev++
		case "issue.paths":
			r.guard(e, f)
			var p struct {
				Source string `json:"source"`
			}
			_ = json.Unmarshal(e.after, &p)
			if p.Source == "declared" && !f.create[e.target] && !f.update[e.target] {
				r.issues[e.target].rev++ // a paths-only update writes the issue row
			}
		case "acceptance.tick", "acceptance.untick", "acceptance.waive":
			r.guard(e, f)
		case "comment.add":
			r.comment(e, f)
		case "label.add", "label.remove":
			var l struct {
				Label string `json:"label"`
			}
			state := e.after
			if e.op == "label.remove" {
				state = e.before
			}
			_ = json.Unmarshal(state, &l)
			if e.op == "label.add" {
				r.issues[e.target].labels[l.Label] = true
			} else {
				delete(r.issues[e.target].labels, l.Label)
			}
		case "dep.add", "dep.remove":
			var d struct{ From, To, Type string }
			state := e.after
			if e.op == "dep.remove" {
				state = e.before
			}
			_ = json.Unmarshal(state, &d)
			k := depKey{d.From, d.To, d.Type}
			if e.op == "dep.add" {
				if r.deps[k] {
					r.fail(e.target, "event %d adds edge %v, which exists", e.seq, k)
				}
				r.deps[k] = true
			} else {
				if !r.deps[k] {
					r.fail(e.target, "event %d removes edge %v, which does not exist", e.seq, k)
				}
				delete(r.deps, k)
			}
		case "claim.take":
			r.take(e)
		case "claim.expire":
			r.expire(e)
		case "admin.override":
			r.override(e)
		case "usage.add":
			r.usage[e.actor]++
		default:
			r.fail(e.target, "event %d has op %q, which the soak does not expect", e.seq, e.op)
		}
	}
}

func causeKey(issue string, at time.Time) string {
	return issue + "@" + at.UTC().Format(time.RFC3339Nano)
}

// issueState is the part of an event's issue state the replay keeps.
type issueState struct {
	ID          *string    `json:"id"`
	Title       *string    `json:"title"`
	Status      *string    `json:"status"`
	Priority    *int       `json:"priority"`
	Type        *string    `json:"type"`
	CreatedAt   *time.Time `json:"created_at"`
	Labels      []string   `json:"labels"`
	CloseReason *string    `json:"close_reason"`
}

func (r *replay) create(e sevent) {
	if _, ok := r.issues[e.target]; ok {
		r.fail(e.target, "event %d creates issue %s again", e.seq, e.target)
		return
	}
	var s issueState
	_ = json.Unmarshal(e.after, &s)
	is := &rIssue{id: e.target, labels: map[string]bool{}, rev: 1, createdAt: deref(s.CreatedAt)}
	r.issues[e.target] = is
	r.patch(e)
	for _, l := range s.Labels {
		is.labels[l] = true
	}
	r.created[e.target] = is.createdAt
	r.markers[is.title]++
}

// patch applies the fields an event's after state sets; a field set to
// null is cleared.
func (r *replay) patch(e sevent) {
	is := r.issues[e.target]
	var m map[string]json.RawMessage
	if json.Unmarshal(e.after, &m) != nil {
		return
	}
	str := func(k string, dst *string) {
		if v, ok := m[k]; ok {
			*dst = ""
			_ = json.Unmarshal(v, dst)
		}
	}
	str("title", &is.title)
	str("status", &is.status)
	str("type", &is.typ)
	str("assignee", &is.assignee)
	str("parent_id", &is.parent)
	str("close_reason", &is.closeReason)
	if v, ok := m["priority"]; ok {
		_ = json.Unmarshal(v, &is.priority)
	}
}

func (r *replay) update(e sevent, f txFacts) {
	is := r.issues[e.target]
	var s issueState
	_ = json.Unmarshal(e.after, &s)
	if s.Title != nil {
		r.markers[*s.Title]++
	}
	r.patch(e)
	is.rev++
	if s.Status != nil && *s.Status != "in_progress" {
		r.endHold(e.target, e.at, time.Time{})
	}
	c := r.claim(e.target)
	if f.handoff[e.target] && c.holder != (sessKey{}) {
		// A releasing handoff: the claim ended before its note.
		r.ends[issueEpoch{e.target, c.epoch}] = ended{at: e.at, op: "release"}
		c.holder, c.maybeReleased = sessKey{}, false
	}
}

func (r *replay) close(e sevent) {
	is := r.issues[e.target]
	if is.status == "closed" {
		r.fail(e.target, "event %d closes %s, already closed", e.seq, e.target)
	}
	var s issueState
	_ = json.Unmarshal(e.after, &s)
	reason := deref(s.CloseReason)
	r.markers[reason]++
	c := r.claim(e.target)
	r.closedAt[reason] = atEpoch{issue: e.target, epoch: c.epoch, holder: c.holder, actor: e.actor, seq: e.seq}
	if c.holder != (sessKey{}) {
		r.ends[issueEpoch{e.target, c.epoch}] = ended{at: e.at, op: "close"}
	}
	c.holder, c.maybeReleased = sessKey{}, false
	r.endHold(e.target, e.at, time.Time{})
	r.patch(e)
	is.rev++
}

func (r *replay) comment(e sevent, f txFacts) {
	var c struct {
		Body string `json:"body"`
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(e.after, &c)
	r.markers[c.Body]++
	if c.Kind != "handoff" {
		return
	}
	r.guard(e, f)
	cl := r.claim(e.target)
	r.handedAt[c.Body] = atEpoch{issue: e.target, epoch: cl.epoch, holder: cl.holder, actor: e.actor, seq: e.seq}
	if !f.update[e.target] && cl.holder != (sessKey{}) && r.issues[e.target].status != "in_progress" {
		cl.maybeReleased = true
	}
}

func (r *replay) claim(id string) *rClaim {
	c := r.claims[id]
	if c == nil {
		c = &rClaim{}
		r.claims[id] = c
	}
	return c
}

// claimState is a claim.take's before state, or a claim.expire's.
type claimState struct {
	Holder    struct{ Principal, Session string }
	Epoch     int64
	ExpiresAt time.Time `json:"expires_at"`
}

func (r *replay) take(e sevent) {
	c := r.claim(e.target)
	var after struct {
		Epoch     int64     `json:"epoch"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	_ = json.Unmarshal(e.after, &after)
	if after.Epoch != c.epoch+1 {
		r.fail(e.target, "event %d takes epoch %d after epoch %d", e.seq, after.Epoch, c.epoch)
	}
	var before *claimState
	if len(e.before) > 0 {
		before = &claimState{}
		_ = json.Unmarshal(e.before, before)
	}
	switch {
	case before == nil && c.holder != (sessKey{}) && !c.maybeReleased:
		r.fail(e.target, "event %d takes epoch %d without naming %s, who held epoch %d", e.seq, after.Epoch, c.holder, c.epoch)
	case before != nil && c.holder == (sessKey{}):
		r.fail(e.target, "event %d replaces a claim by %s/%s, but no claim was held", e.seq, before.Holder.Principal, before.Holder.Session)
	case before != nil && (before.Holder.Principal != c.holder.principal || before.Holder.Session != c.holder.session || before.Epoch != c.epoch):
		r.fail(e.target, "event %d replaces %s/%s at epoch %d, but %s held epoch %d", e.seq,
			before.Holder.Principal, before.Holder.Session, before.Epoch, c.holder, c.epoch)
	}
	end := e.at
	if before != nil {
		prev := sessKey{before.Holder.Principal, before.Holder.Session}
		r.ends[issueEpoch{e.target, c.epoch}] = ended{at: e.at, exp: before.ExpiresAt, op: "take"}
		live := before.ExpiresAt.After(e.at)
		switch {
		case prev == e.actor:
		case prev.principal != e.actor.principal && live:
			r.fail(e.target, "event %d: %s took epoch %d while %s's claim ran to %s", e.seq, e.actor, after.Epoch, prev, stamp(before.ExpiresAt))
		case live:
			r.overs = append(r.overs, takeover{issue: e.target, epoch: after.Epoch, actor: e.actor, seq: e.seq})
		}
		if prev != e.actor {
			r.notices = append(r.notices, notice{issue: e.target, to: prev, at: e.at, seq: e.seq})
		}
		if before.ExpiresAt.Before(end) {
			end = before.ExpiresAt
		}
	}
	r.endHold(e.target, e.at, end)
	c.holder, c.epoch, c.maybeReleased = e.actor, after.Epoch, false
	r.takers[issueEpoch{e.target, after.Epoch}] = e.actor
	r.holds[e.target] = append(r.holds[e.target], &hold{issue: e.target, who: e.actor, start: e.at, open: true})
}

func (r *replay) expire(e sevent) {
	c := r.claim(e.target)
	var b claimState
	_ = json.Unmarshal(e.before, &b)
	who := sessKey{b.Holder.Principal, b.Holder.Session}
	if e.actor != reaper {
		r.fail(e.target, "event %d: claim.expire by %s, not the reaper", e.seq, e.actor)
	}
	if (who != c.holder && !c.maybeReleased) || b.Epoch != c.epoch {
		r.fail(e.target, "event %d expires %s at epoch %d, but %s held epoch %d", e.seq, who, b.Epoch, c.holder, c.epoch)
	}
	if b.ExpiresAt.After(e.at) {
		r.fail(e.target, "event %d: the reaper ended %s's claim at %s, before its lease ran out at %s", e.seq, who, stamp(e.at), stamp(b.ExpiresAt))
	}
	r.ends[issueEpoch{e.target, b.Epoch}] = ended{at: e.at, exp: b.ExpiresAt, op: "expire"}
	r.notices = append(r.notices, notice{issue: e.target, to: who, at: e.at, seq: e.seq})
	end := e.at
	if b.ExpiresAt.Before(end) {
		end = b.ExpiresAt
	}
	r.endHold(e.target, e.at, end)
	c.holder, c.maybeReleased = sessKey{}, false
}

func (r *replay) override(e sevent) {
	c := r.claim(e.target)
	var a struct {
		Holder struct{ Principal, Session string }
		Epoch  int64
	}
	_ = json.Unmarshal(e.after, &a)
	if !r.admins[e.actor.principal] {
		r.fail(e.target, "event %d: admin.override by %s, who is not an admin", e.seq, e.actor)
	}
	if (sessKey{a.Holder.Principal, a.Holder.Session} != c.holder && !c.maybeReleased) || a.Epoch != c.epoch {
		r.fail(e.target, "event %d overrides %s/%s at epoch %d, but %s held epoch %d", e.seq,
			a.Holder.Principal, a.Holder.Session, a.Epoch, c.holder, c.epoch)
	}
}

// guard checks a change to an issue against its claim: another
// principal than the holder's may change it only as an admin, recorded by
// an admin.override in the same transaction, or once the lease lapsed,
// which is checked when the claim's end is known.
func (r *replay) guard(e sevent, f txFacts) {
	c := r.claim(e.target)
	if c.holder == (sessKey{}) || c.holder.principal == e.actor.principal || e.actor == reaper {
		return
	}
	if f.override[e.target] && r.admins[e.actor.principal] {
		return
	}
	r.guards = append(r.guards, guardCheck{issue: e.target, epoch: c.epoch, at: e.at, actor: e.actor, op: e.op, seq: e.seq})
}

// endHold ends the open hold on issue at end, or at at when end is zero.
func (r *replay) endHold(issue string, at, end time.Time) {
	hs := r.holds[issue]
	if len(hs) == 0 || !hs[len(hs)-1].open {
		return
	}
	if end.IsZero() {
		end = at
	}
	h := hs[len(hs)-1]
	h.end, h.open = maxTime(h.start, end), false
}
