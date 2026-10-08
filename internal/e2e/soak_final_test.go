package e2e

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// verify runs the checks that need the whole run, once it is at rest: the
// journal against the event log, the inbox, usage, ready and blocked,
// then closes every session and checks for leaks.
func (s *soak) verify() {
	calls := s.mon.calls()
	r := s.rep
	r.mu.Lock()
	s.checkMarkers(calls)
	s.checkFencing(calls)
	s.checkGuards()
	s.checkTakeovers(calls)
	s.checkRenewals()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s.checkNotices(ctx)
	s.checkPushes(ctx)
	eve, err := mcpserver.DialRepo(ctx, s.eve.repo, s.eveOptions("eve-check"))
	if err != nil {
		r.mu.Unlock()
		s.t.Errorf("dial as eve: %v", err)
		return
	}
	s.checkReady(ctx, eve)
	s.checkUsage(ctx, eve, calls)
	r.mu.Unlock()
	_ = eve.Close()
	for _, a := range s.all() {
		a.close()
	}
	s.checkLeaks(15 * time.Second)
}

// attempts gathers the requests that carried one marker, or one batch.
type attempts struct {
	acked, refused, lost bool
	who                  sessKey
	op                   string
}

// result is the marker's outcome: applied once if any attempt was
// acknowledged, never if every attempt was refused, and at most once if
// any was lost without an answer.
func (a attempts) want() (lo, hi int) {
	switch {
	case a.acked:
		return 1, 1
	case a.lost:
		return 0, 1
	}
	return 0, 0
}

// markersOf lists the markers a request carried: text unique to it that
// the event log shows wherever the request took effect.
func markersOf(args any) []string {
	switch a := args.(type) {
	case proto.CreateArgs:
		return []string{a.Title}
	case proto.UpdateArgs:
		if a.Title != nil {
			return []string{*a.Title}
		}
	case proto.CloseArgs:
		return []string{a.Reason}
	case proto.CommentArgs:
		return []string{a.Body}
	case proto.HandoffArgs:
		return []string{a.Note}
	case proto.FinishArgs:
		out := []string{a.Reason}
		for _, d := range a.Discovered {
			out = append(out, d.Title)
		}
		if a.Handoff != "" {
			out = append(out, a.Handoff)
		}
		return out
	}
	return nil
}

// checkMarkers: every acknowledged write is in the log exactly once,
// every refused one not at all, every one whose answer was lost at most
// once, and nothing else is there. Retries carry the same marker, so a
// retry applied twice shows here.
func (s *soak) checkMarkers(calls []call) {
	byMarker := map[string]*attempts{}
	for _, c := range calls {
		for _, m := range markersOf(c.args) {
			a := byMarker[m]
			if a == nil {
				a = &attempts{who: c.who, op: c.op}
				byMarker[m] = a
			}
			switch c.out {
			case acked:
				a.acked = true
			case refused:
				a.refused = true
			case lost:
				a.lost = true
			}
		}
	}
	for m, a := range byMarker {
		n := s.rep.markers[m]
		if lo, hi := a.want(); n < lo || n > hi {
			s.mon.fail(s.issueOf(m), "%s by %s %q is in the event log %d times; acked %t, refused %t, lost %t",
				a.op, a.who, m, n, a.acked, a.refused, a.lost)
		}
	}
	for m, n := range s.rep.markers {
		if _, ok := byMarker[m]; !ok && n > 0 {
			s.mon.fail(s.issueOf(m), "%q is in the event log %d times, but no session sent it", m, n)
		}
	}
}

// issueOf finds the issue a marker landed on, for the failure's story.
func (s *soak) issueOf(marker string) string {
	for id, is := range s.rep.issues {
		if is.title == marker || is.closeReason == marker {
			return id
		}
	}
	if e, ok := s.rep.closedAt[marker]; ok {
		return e.issue
	}
	if e, ok := s.rep.handedAt[marker]; ok {
		return e.issue
	}
	return ""
}

// checkFencing: a finish or releasing handoff that named an epoch and
// took effect acted under that epoch, and was the holder's. It looks at
// every call, not only those acknowledged: a call whose answer was lost
// may have been applied, and its marker says whether it was.
func (s *soak) checkFencing(calls []call) {
	for _, c := range calls {
		var at atEpoch
		var ok bool
		var epoch int64
		var id string
		switch a := c.args.(type) {
		case proto.FinishArgs:
			at, ok = s.rep.closedAt[a.Reason]
			epoch, id = a.Epoch, a.ID
		case proto.HandoffArgs:
			if !a.Release {
				continue
			}
			at, ok = s.rep.handedAt[a.Note]
			epoch, id = a.Epoch, a.ID
		default:
			continue
		}
		if !ok || epoch == 0 {
			continue // checkMarkers reports a missing event
		}
		if at.epoch != epoch {
			s.mon.fail(id, "%s's %s fenced to epoch %d was applied under epoch %d (event %d)", c.who, c.op, epoch, at.epoch, at.seq)
		}
		if at.holder != c.who && at.holder != (sessKey{}) {
			s.mon.fail(id, "%s's %s under epoch %d was applied while %s held it (event %d)", c.who, c.op, epoch, at.holder, at.seq)
		}
	}
}

// checkGuards: a change by another principal than a claim's holder's,
// not an admin's, came after the lease ran out. The lease's end is known
// from the event that ended the claim, or bounded below by the last
// expiry a client was told.
func (s *soak) checkGuards() {
	for _, g := range s.rep.guards {
		k := issueEpoch{g.issue, g.epoch}
		exp := s.rep.ends[k].exp
		if exp.IsZero() {
			exp = s.mon.maxExp(k)
		}
		if exp.After(g.at) {
			s.mon.fail(g.issue, "%s made %s (event %d) at %s, while %s held epoch %d until %s",
				g.actor, g.op, g.seq, stamp(g.at), s.rep.takers[k], g.epoch, stamp(exp))
		}
	}
}

// checkTakeovers: a session took a live claim from another session of
// its principal only when its start asked to.
func (s *soak) checkTakeovers(calls []call) {
	type key struct {
		who   sessKey
		issue string
	}
	took := map[key]bool{}
	for _, c := range calls {
		if a, ok := c.args.(proto.StartArgs); ok && a.Take && a.ID != "" {
			took[key{c.who, a.ID}] = true
		}
	}
	for _, o := range s.rep.overs {
		if !took[key{o.actor, o.issue}] {
			s.mon.fail(o.issue, "%s took epoch %d of %s from another session of its principal (event %d) without asking to take it",
				o.actor, o.epoch, o.issue, o.seq)
		}
	}
}

// checkRenewals: every claim a client was told of is the one the log
// records, and a lease the client was told of was never cut short: the
// claim's recorded end is no earlier.
func (s *soak) checkRenewals() {
	s.mon.mu.Lock()
	defer s.mon.mu.Unlock()
	for k, rec := range s.mon.takes {
		who, ok := s.rep.takers[k]
		switch {
		case !ok:
			s.mon.failLocked(k.issue, "%s was told it holds epoch %d of %s, which no claim.take records", rec.who, k.epoch, k.issue)
			continue
		case who != rec.who:
			s.mon.failLocked(k.issue, "%s was told it holds epoch %d of %s, which %s took", rec.who, k.epoch, k.issue, who)
		}
		if end := s.rep.ends[k]; !end.exp.IsZero() && end.exp.Before(rec.maxExp) {
			s.mon.failLocked(k.issue, "%s was told epoch %d of %s runs to %s, but the claim ended (%s, event at %s) recording %s: a renewal was lost",
				rec.who, k.epoch, k.issue, stamp(rec.maxExp), end.op, stamp(end.at), stamp(end.exp))
		}
	}
}

// inboxRow is one inbox row.
type inboxRow struct {
	proto.InboxItem
	to string
}

func (s *soak) inboxRows(ctx context.Context) (map[int64]inboxRow, error) {
	out := map[int64]inboxRow{}
	err := scan(ctx, s.db, `SELECT id, to_principal, COALESCE(to_session, ''), kind, COALESCE(issue_id, ''), body, from_principal, at
  FROM inbox`, nil, func(rows *sql.Rows) error {
		var r inboxRow
		if err := rows.Scan(&r.ID, &r.to, &r.Session, &r.Kind, &r.Issue, &r.Body, &r.From, &r.At); err != nil {
			return err
		}
		r.At = r.At.UTC()
		out[r.ID] = r
		return nil
	})
	return out, err
}

// checkNotices: every claim a session lost to a takeover or the reaper
// put a claim.lost item in its inbox, in the same transaction, and every
// claim.lost item has a cause in the log.
func (s *soak) checkNotices(ctx context.Context) {
	rows, err := s.inboxRows(ctx)
	if err != nil {
		s.t.Errorf("read inbox: %v", err)
		return
	}
	type key struct {
		issue string
		to    sessKey
		at    time.Time
	}
	have := map[key]bool{}
	for _, r := range rows {
		if r.Kind != "claim.lost" {
			continue
		}
		have[key{r.Issue, sessKey{r.to, r.Session}, r.At}] = true
		if !s.rep.causes[causeKey(r.Issue, r.At)] {
			s.mon.fail(r.Issue, "claim.lost item %d to %s/%s at %s has no event on %s at that time", r.ID, r.to, r.Session, stamp(r.At), r.Issue)
		}
	}
	for _, n := range s.rep.notices {
		if !have[key{n.issue, n.to, n.at}] {
			s.mon.fail(n.issue, "%s lost its claim on %s (event %d) and was not told: no claim.lost item", n.to, n.issue, n.seq)
		}
	}
}

// all returns every session the run started, vanished or not.
func (s *soak) all() []worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.every)
}

// checkPushes: every item pushed to a session is in the inbox table as it
// was pushed, and addressed to that session or its principal.
func (s *soak) checkPushes(ctx context.Context) {
	rows, err := s.inboxRows(ctx)
	if err != nil {
		s.t.Errorf("read inbox: %v", err)
		return
	}
	for _, a := range s.all() {
		ss := a.base()
		for id, it := range ss.inbox.pushed() {
			r, ok := rows[id]
			switch {
			case !ok:
				s.mon.fail(it.Issue, "%s was pushed inbox item %d, which is not in the inbox", ss.key, id)
			case r.to != ss.key.principal || (r.Session != "" && r.Session != ss.key.session):
				s.mon.fail(it.Issue, "%s was pushed inbox item %d, addressed to %s/%s", ss.key, id, r.to, r.Session)
			case !it.At.Equal(r.At) || it.Kind != r.Kind || it.Issue != r.Issue || it.Body != r.Body || it.From != r.From || it.Session != r.Session:
				s.mon.fail(it.Issue, "%s was pushed item %d as %+v; the inbox holds %+v", ss.key, id, it, r.InboxItem)
			}
		}
	}
	s.checkPushedEvents(ctx)
}

// checkPushedEvents: every issue event pushed to a watch is in the event
// log as it was pushed, and is an issue's.
func (s *soak) checkPushedEvents(ctx context.Context) {
	evs, err := readEvents(ctx, s.db, 0)
	if err != nil {
		s.t.Errorf("read events: %v", err)
		return
	}
	bySeq := make(map[int64]sevent, len(evs))
	for _, e := range evs {
		bySeq[e.seq] = e
	}
	for _, a := range s.all() {
		ss := a.base()
		for _, got := range ss.inbox.events() {
			for seq, p := range got {
				s.stats.eventsPushed.Add(1)
				e, ok := bySeq[seq]
				switch {
				case !ok:
					s.mon.fail(p.Issue, "%s was pushed event %d, which is not in the event log", ss.key, seq)
				case store.IssueID(e.target).Validate() != nil:
					s.mon.fail(p.Issue, "%s was pushed event %d (%s on %s), which is no issue's", ss.key, seq, e.op, e.target)
				case !p.At.Equal(e.at) || p.Op != e.op || p.Issue != e.target || p.Principal != e.actor.principal || p.Session != e.actor.session:
					s.mon.fail(p.Issue, "%s was pushed event %d as %s %s by %s/%s at %s; the log holds %s %s by %s at %s", ss.key, seq,
						p.Op, p.Issue, p.Principal, p.Session, stamp(p.At), e.op, e.target, e.actor, stamp(e.at))
				}
			}
		}
	}
}

// checkReady: ready and blocked, as the server answers them, are what the
// event log says they are. At rest no claim is held, so ready's order is
// priority, then age, then id.
func (s *soak) checkReady(ctx context.Context, eve *mcpserver.RepoConn) {
	ready, blocked := s.rep.derive()
	var got proto.ListResult
	if err := eve.Call(ctx, proto.OpReady, proto.LimitArgs{Limit: 500}, &got); err != nil {
		s.t.Errorf("ready: %v", err)
		return
	}
	var ids []string
	for _, is := range got.Issues {
		ids = append(ids, is.ID)
	}
	if want := ready[:min(len(ready), 500)]; !slices.Equal(ids, want) {
		s.mon.fail(firstDiff(ids, want), "ready is %d issues %v…; the event log says %d issues %v…", len(ids), head(ids), len(want), head(want))
	}
	var gotB proto.BlockedResult
	if err := eve.Call(ctx, proto.OpBlocked, proto.LimitArgs{Limit: 500}, &gotB); err != nil {
		s.t.Errorf("blocked: %v", err)
		return
	}
	var bids []string
	for _, b := range gotB.Issues {
		bids = append(bids, b.ID)
		want := blocked.by[b.ID]
		if b.Via != "" {
			if !blocked.blockedSet[b.Via] {
				s.mon.fail(b.ID, "blocked says %s is blocked via %s, which the event log says is not blocked", b.ID, b.Via)
			}
			want = blocked.direct[b.Via]
		}
		if !slices.Equal(b.BlockedBy, want) {
			s.mon.fail(b.ID, "blocked says %s is blocked by %v; the event log says %v", b.ID, b.BlockedBy, want)
		}
	}
	if want := blocked.order[:min(len(blocked.order), 500)]; !slices.Equal(bids, want) {
		s.mon.fail(firstDiff(bids, want), "blocked is %d issues %v…; the event log says %d issues %v…", len(bids), head(bids), len(want), head(want))
	}
}

func head(ids []string) []string { return ids[:min(len(ids), 5)] }

// firstDiff is the first id where two lists differ.
func firstDiff(a, b []string) string {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return a[i]
		}
	}
	if len(a) > len(b) {
		return a[len(b)]
	}
	if len(b) > len(a) {
		return b[len(a)]
	}
	return ""
}

// derived blocked state.
type blockedState struct {
	direct     map[string][]string // an issue's own unclosed blockers, sorted
	by         map[string][]string // as direct, for blocked issues that have their own
	blockedSet map[string]bool     // blocked directly or through an ancestor
	order      []string            // unclosed blocked issues, by priority, age and id
}

// derive computes ready and blocked from the replayed issues and edges,
// as design §4 defines them: an issue is blocked while it or an ancestor
// has an unclosed blocking target and is itself unclosed, deferred while
// it or an ancestor is deferred, and ready when open, neither blocked nor
// deferred.
func (r *replay) derive() ([]string, blockedState) {
	b := blockedState{direct: map[string][]string{}, by: map[string][]string{}, blockedSet: map[string]bool{}}
	for k := range r.deps {
		if k.typ != "blocks" && k.typ != "conditional-blocks" {
			continue
		}
		f, t := r.issues[k.from], r.issues[k.to]
		if f != nil && t != nil && f.status != "closed" && t.status != "closed" {
			b.direct[k.from] = append(b.direct[k.from], k.to)
		}
	}
	for id := range b.direct {
		slices.Sort(b.direct[id])
		b.by[id] = b.direct[id]
	}
	children := map[string][]string{}
	for id, is := range r.issues {
		if is.parent != "" {
			children[is.parent] = append(children[is.parent], id)
		}
	}
	descend := func(seeds []string, into map[string]bool) {
		for len(seeds) > 0 {
			id := seeds[len(seeds)-1]
			seeds = seeds[:len(seeds)-1]
			if into[id] {
				continue
			}
			into[id] = true
			seeds = append(seeds, children[id]...)
		}
	}
	descend(slices.Collect(maps.Keys(b.direct)), b.blockedSet)
	deferred := map[string]bool{}
	var seeds []string
	for id, is := range r.issues {
		if is.status == "deferred" {
			seeds = append(seeds, id)
		}
	}
	descend(seeds, deferred)
	byRank := func(a, c string) int {
		x, y := r.issues[a], r.issues[c]
		return cmp.Or(cmp.Compare(x.priority, y.priority), x.createdAt.Compare(y.createdAt), strings.Compare(a, c))
	}
	var ready []string
	for id, is := range r.issues {
		if is.status == "open" && !b.blockedSet[id] && !deferred[id] {
			ready = append(ready, id)
		}
		if b.blockedSet[id] && is.status != "closed" {
			b.order = append(b.order, id)
		}
	}
	slices.SortFunc(ready, byRank)
	slices.SortFunc(b.order, byRank)
	return ready, b
}

// usageRow is one token_usage row.
type usageRow struct {
	who       sessKey
	requestID string
	model     string
	at, added time.Time
	counts    [5]*int64 // input, output, cache write, cache write 1h, cache read
}

func countsOf(t proto.Tokens) [5]*int64 {
	return [5]*int64{t.Input, t.Output, t.CacheWrite, t.CacheWrite1h, t.CacheRead}
}

// modelSum is one model's tokens: each count's sum, and whether any
// record that contributed reported it.
type modelSum struct {
	n     [5]int64
	known [5]bool
}

type usageSums map[string]*modelSum

func (u usageSums) add(model string, counts [5]*int64, part func(int64) int64) {
	m := u[model]
	if m == nil {
		m = &modelSum{}
		u[model] = m
	}
	for i, c := range counts {
		if c != nil {
			m.n[i] += part(*c)
			m.known[i] = true
		}
	}
}

// equal compares the sums with a server's answer.
func (u usageSums) equal(got []proto.ModelTokens) bool {
	if len(got) != len(u) {
		return false
	}
	for _, g := range got {
		m := u[g.Model]
		if m == nil {
			return false
		}
		for i, c := range countsOf(g.Tokens) {
			if (c != nil) != m.known[i] || (c != nil && *c != m.n[i]) {
				return false
			}
		}
	}
	return true
}

func (u usageSums) String() string {
	var b strings.Builder
	for _, model := range slices.Sorted(maps.Keys(u)) {
		fmt.Fprintf(&b, "%s%v ", model, u[model].n)
	}
	return b.String()
}

func modelsString(ms []proto.ModelTokens) string {
	var b strings.Builder
	for _, m := range ms {
		var n [5]int64
		for i, c := range countsOf(m.Tokens) {
			n[i] = deref(c)
		}
		fmt.Fprintf(&b, "%s%v ", m.Model, n)
	}
	return b.String()
}

// checkUsage: the usage table holds exactly the records sent and
// acknowledged, each batch whole or not at all; no principal stored more
// in a day than its cap; and each issue's tokens, and the digest's, are
// the records divided among the issues their sessions held at the
// records' times (design §12.1), recomputed here from the event log.
func (s *soak) checkUsage(ctx context.Context, eve *mcpserver.RepoConn, calls []call) {
	var rows []usageRow
	err := scan(ctx, s.db, `SELECT principal, session, request_id, model, at, added_at, input, output, cache_write, cache_write_1h, cache_read
  FROM token_usage`, nil, func(rs *sql.Rows) error {
		var u usageRow
		var c [5]sql.NullInt64
		if err := rs.Scan(&u.who.principal, &u.who.session, &u.requestID, &u.model, &u.at, &u.added, &c[0], &c[1], &c[2], &c[3], &c[4]); err != nil {
			return err
		}
		u.at, u.added = u.at.UTC(), u.added.UTC()
		for i := range c {
			if c[i].Valid {
				v := c[i].Int64
				u.counts[i] = &v
			}
		}
		rows = append(rows, u)
		return nil
	})
	if err != nil {
		s.t.Errorf("read usage: %v", err)
		return
	}
	type rkey struct {
		who sessKey
		id  string
	}
	stored := map[rkey]usageRow{}
	for _, u := range rows {
		stored[rkey{u.who, u.requestID}] = u
	}
	// Batches, by their first record: a retry resends the same batch.
	batches := map[rkey]*attempts{}
	sent := map[rkey]proto.UsageRecord{}
	var order []rkey
	recs := map[rkey][]proto.UsageRecord{}
	for _, c := range calls {
		a, ok := c.args.(proto.UsageArgs)
		if !ok || len(a.Records) == 0 {
			continue
		}
		k := rkey{c.who, a.Records[0].RequestID}
		b := batches[k]
		if b == nil {
			b = &attempts{who: c.who, op: c.op}
			batches[k] = b
			order = append(order, k)
			recs[k] = a.Records
		}
		switch c.out {
		case acked:
			b.acked = true
		case refused:
			b.refused = true
		case lost:
			b.lost = true
		}
		for _, r := range a.Records {
			sent[rkey{c.who, r.RequestID}] = r
		}
	}
	for _, k := range order {
		n := 0
		for _, r := range recs[k] {
			if _, ok := stored[rkey{k.who, r.RequestID}]; ok {
				n++
			}
		}
		b := batches[k]
		lo, hi := b.want()
		switch {
		case n != 0 && n != len(recs[k]):
			s.mon.fail("", "%s's usage batch from %s stored %d of its %d records", k.who, k.id, n, len(recs[k]))
		case n == 0 && lo == 1, n > 0 && hi == 0:
			s.mon.fail("", "%s's usage batch from %s stored %d records; acked %t, refused %t, lost %t", k.who, k.id, n, b.acked, b.refused, b.lost)
		}
	}
	for k, u := range stored {
		r, ok := sent[k]
		if !ok {
			s.mon.fail("", "usage record %s of %s is stored, but no session sent it", k.id, k.who)
			continue
		}
		if !r.At.Truncate(time.Microsecond).Equal(u.at) || r.Model != u.model || !equalCounts(countsOf(r.Tokens), u.counts) {
			s.mon.fail("", "usage record %s of %s is stored as %s %v at %s, sent as %s %v at %s", k.id, k.who,
				u.model, countsString(u.counts), stamp(u.at), r.Model, countsString(countsOf(r.Tokens)), stamp(r.At))
		}
	}
	s.checkUsageCap(rows)
	s.checkAttribution(ctx, eve, rows)
}

// countsString prints counts, with "-" for a count not reported.
func countsString(c [5]*int64) string {
	var b strings.Builder
	for i, n := range c {
		if i > 0 {
			b.WriteByte('/')
		}
		if n == nil {
			b.WriteByte('-')
			continue
		}
		fmt.Fprint(&b, *n)
	}
	return b.String()
}

func equalCounts(a, b [5]*int64) bool {
	for i := range a {
		if (a[i] == nil) != (b[i] == nil) || (a[i] != nil && *a[i] != *b[i]) {
			return false
		}
	}
	return true
}

// checkUsageCap: no principal stored more records in any 24 hours, by
// the server's receipt time, than usage_per_day.
func (s *soak) checkUsageCap(rows []usageRow) {
	byP := map[string][]time.Time{}
	for _, u := range rows {
		byP[u.who.principal] = append(byP[u.who.principal], u.added)
	}
	for p, ts := range byP {
		slices.SortFunc(ts, time.Time.Compare)
		lo := 0
		for hi, t := range ts {
			for !ts[lo].After(t.Add(-24 * time.Hour)) {
				lo++
			}
			if n := hi - lo + 1; n > s.cfg.limits.UsagePerDay {
				s.mon.fail("", "%s stored %d usage records in 24 hours, past the limit of %d", p, n, s.cfg.limits.UsagePerDay)
				break
			}
		}
	}
}

// checkAttribution recomputes each issue's tokens and time from the
// replayed holds and compares show's answer, and the digest's totals.
func (s *soak) checkAttribution(ctx context.Context, eve *mcpserver.RepoConn, rows []usageRow) {
	bySession := map[sessKey][]*hold{}
	held := map[string]time.Duration{}
	for id, hs := range s.rep.holds {
		for _, h := range hs {
			if h.open {
				s.mon.fail(id, "%s's hold on %s from %s never ended, with every claim reaped", h.who, id, stamp(h.start))
				continue
			}
			bySession[h.who] = append(bySession[h.who], h)
			held[id] += h.end.Sub(h.start)
		}
	}
	perIssue := map[string]usageSums{}
	total, loose := usageSums{}, usageSums{}
	for _, u := range rows {
		total.add(u.model, u.counts, func(n int64) int64 { return n })
		var on []string
		for _, h := range bySession[u.who] {
			if !h.start.After(u.at) && h.end.After(u.at) {
				on = append(on, h.issue)
			}
		}
		slices.Sort(on)
		on = slices.Compact(on)
		if len(on) == 0 {
			loose.add(u.model, u.counts, func(n int64) int64 { return n })
			continue
		}
		// Shared evenly in whole tokens: the remainder goes one each to
		// the first issues by id, so the parts sum to the record.
		for i, id := range on {
			if perIssue[id] == nil {
				perIssue[id] = usageSums{}
			}
			k := int64(len(on))
			perIssue[id].add(u.model, u.counts, func(n int64) int64 {
				part := n / k
				if int64(i) < n%k {
					part++
				}
				return part
			})
		}
	}
	ids := slices.Sorted(maps.Keys(held))
	for _, id := range ids {
		var r proto.ShowResult
		if err := eve.Call(ctx, proto.OpShow, proto.ShowArgs{ID: id}, &r); err != nil {
			s.t.Errorf("show %s: %v", id, err)
			return
		}
		want := perIssue[id]
		if want == nil {
			want = usageSums{}
		}
		switch {
		case r.Usage == nil:
			s.mon.fail(id, "show %s has no usage", id)
		case r.Usage.Capped:
			s.mon.fail(id, "show %s read too many records to total them", id)
		case !want.equal(r.Usage.Models):
			s.mon.fail(id, "show %s attributes %s; its holds and the records say %s", id, modelsString(r.Usage.Models), want)
		case r.Usage.HeldSeconds != int64(held[id]/time.Second):
			s.mon.fail(id, "show %s says it was held %ds; its holds say %s", id, r.Usage.HeldSeconds, held[id])
		}
	}
	var d proto.DigestResult
	if err := eve.Call(ctx, proto.OpDigest, proto.DigestArgs{Since: "366d"}, &d); err != nil {
		s.t.Errorf("digest: %v", err)
		return
	}
	switch {
	case len(rows) == 0:
	case d.Usage == nil:
		s.mon.fail("", "digest has no usage, with %d records stored", len(rows))
	case !total.equal(d.Usage.Models):
		s.mon.fail("", "digest totals %s; the stored records sum to %s", modelsString(d.Usage.Models), total)
	case !loose.equal(d.Usage.Unattributed):
		s.mon.fail("", "digest leaves %s unattributed; the holds leave %s", modelsString(d.Usage.Unattributed), loose)
	}
}

// eveOptions are the options of a connection of eve, who only reads.
func (s *soak) eveOptions(session string) client.Options {
	return client.Options{Version: "v0.2.0", Session: session, Machine: "checker", Getenv: func(string) string { return "" }}
}

// leakPatterns name the goroutines that serve or hold a connection: the
// daemon's handler and watch pushes, the bridge, the client's reader and
// the SSH server's.
var leakPatterns = []string{"server.(*Server).handle(", "server.(*Server).watch.func", "server.Bridge(",
	"client.(*Conn).read(", "e2e.(*world).serveSSH("}

// connGoroutines counts the running goroutines that match each of
// leakPatterns.
func connGoroutines() map[string]int {
	buf := make([]byte, 16<<20)
	buf = buf[:runtime.Stack(buf, true)]
	counts := map[string]int{}
	for g := range bytes.SplitSeq(buf, []byte("\n\n")) {
		for _, p := range leakPatterns {
			if bytes.Contains(g, []byte(p)) {
				counts[p]++
			}
		}
	}
	return counts
}

// checkLeaks: once every session has closed, within wait, the daemon
// serves no connection, pushes to no watch, and the SSH server and
// clients hold none beyond those running before the run, as another
// test's may be; the goroutines are back near where they started.
func (s *soak) checkLeaks(wait time.Duration) {
	deadline := time.Now().Add(wait)
	for {
		grown := map[string]int{}
		for p, n := range connGoroutines() {
			if d := n - s.connBase[p]; d > 0 {
				grown[p] = d
			}
		}
		s.w.connsMu.Lock()
		conns := len(s.w.conns)
		s.w.connsMu.Unlock()
		n := runtime.NumGoroutine()
		if len(grown) == 0 && conns == 0 && n <= s.baseline+24 {
			return
		}
		if time.Now().After(deadline) {
			s.mon.fail("", "after every session closed: goroutines %v more than before the run, %d SSH connections open, %d goroutines (%d before the run)",
				grown, conns, n, s.baseline)
			return
		}
		pause(context.Background(), 50*time.Millisecond)
	}
}

// explain fails the test with every violation, the seed, and the story of
// the first issues involved: their events, in order.
func (s *soak) explain() {
	vs := s.mon.violations()
	if len(vs) == 0 {
		return
	}
	seen := map[string]bool{}
	var issues []string
	shown := 0
	for _, v := range vs {
		if seen[v.msg] {
			continue
		}
		seen[v.msg] = true
		if shown++; shown <= 30 {
			s.t.Errorf("soak: %s", v.msg)
		}
		if v.issue != "" && !slices.Contains(issues, v.issue) {
			issues = append(issues, v.issue)
		}
	}
	if shown > 30 {
		s.t.Errorf("soak: … and %d more", shown-30)
	}
	s.t.Errorf("soak: replay this run with STARFIX_SOAK_SEED=%d (the interleaving differs; the choices do not)", s.cfg.seed)
	for _, id := range issues[:min(len(issues), 3)] {
		s.t.Errorf("soak: the story of %s:\n%s", id, s.story(id))
	}
}

// story is an issue's events, one a line.
func (s *soak) story(id string) string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var b strings.Builder
	err := scan(ctx, s.db, `SELECT seq, at, principal, session, op, COALESCE(before_state, ''), COALESCE(after_state, '')
  FROM events WHERE target = ? ORDER BY seq`, []any{id}, func(rows *sql.Rows) error {
		var seq int64
		var at time.Time
		var p, sess, op, before, after string
		if err := rows.Scan(&seq, &at, &p, &sess, &op, &before, &after); err != nil {
			return err
		}
		fmt.Fprintf(&b, "  %6d %s %s/%s %s %s → %s\n", seq, stamp(at), p, sess, op, clip(before), clip(after))
		return nil
	})
	if err != nil {
		return err.Error()
	}
	return b.String()
}

func clip(s string) string {
	if len(s) > 240 {
		return s[:240] + "…"
	}
	if s == "" {
		return "-"
	}
	return s
}

var _ = json.Valid
