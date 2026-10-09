package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"
)

// Attribution (design §12.1): which issue a usage record's tokens belong
// to, worked out when read and never stored, from the claim history in
// the event log. A session holds an issue from its claim.take until the
// claim ends, which the log records as one of:
//
//   - claim.take of the issue by another session (a takeover, or a take
//     after the lease lapsed and before the reaper ran). Its before state
//     holds the replaced claim's expiry, and a hold that lapsed first
//     ends there; a take recorded before token capture has none, and
//     ends the hold at the take;
//   - claim.expire, whose before state holds the lease's expiry, when
//     the hold really ended;
//   - claim.release (a releasing handoff, a close or finish, or an
//     import that took the issue from its holder), whose before state
//     also holds the lease's expiry, so a hold released after its lease
//     ran out ends at the expiry;
//   - for logs written before claim.release existed: issue.close (close
//     and finish end the claim); issue.update or issue.import that moves
//     the issue out of in_progress, as a releasing handoff of an issue in
//     progress did; and issue.import whose after state has
//     claim_released, at its lease's expiry if that came first. In a log
//     with claim.release these follow it and find the hold already
//     ended.
//
// A hold still open ends now, or when its lease ran out if the reaper has
// not yet noticed. A request record goes to the issues its session held
// at its time, shared evenly if it held several. A turn or session record
// is split over its span by time held, sharing each stretch evenly among
// the issues held then; stretches with nothing held are unattributed.
// Each count is divided in whole tokens that sum to it (apportion).
//
// Reads are bounded: an issue's records are read stretch by stretch of
// its holds, a span is at most MaxUsageSpan, one read keeps at most
// usageScanRows records, and only the holds of the records' sessions
// that overlap them are loaded and indexed (sessionHolds, holdIndex).

// IssueUsage is what is attributed to one issue: the time it was held,
// its tokens by model and the account they report against.
type IssueUsage struct {
	UsageSummary
	// Account is the resolved account, empty when neither the issue nor
	// an ancestor sets one; AccountFrom is the issue that sets it.
	Account     string
	AccountFrom IssueID
}

// UsageSummary totals time and tokens.
type UsageSummary struct {
	// Held is the time under claims.
	Held time.Duration
	// Split is set when a record was shared with other issues, or with
	// time no issue was held, so its part here is an estimate.
	Split bool
	// Models are the tokens by model, sorted by model.
	Models []ModelUsage
	// Capped is set when more records matched than a read takes, so the
	// tokens are a lower bound.
	Capped bool
	// Cost is the tokens' list-price equivalent.
	Cost Cost
}

// ModelUsage is one model's tokens. A count is nil when no record that
// contributed reported it; otherwise it sums the records that did, or
// their parts. A record is divided in whole tokens whose parts sum to it,
// so the issues' parts and the unattributed part add up to the total.
type ModelUsage struct {
	Model string
	Tokens
}

// usageScanRows bounds the usage rows one read takes. Tests lower it.
var usageScanRows = 100_000

// ctxEvery is how many rows a long loop takes between checks that its
// context is still live.
const ctxEvery = 1024

// sessionKey names a session: the principal and the client's session id.
type sessionKey struct{ principal, session string }

// hold is a session's claim on an issue from start to end.
type hold struct {
	issue      IssueID
	key        sessionKey
	start, end time.Time
}

// usageRow is a token_usage row as attribution reads it.
type usageRow struct {
	key       sessionKey
	requestID string
	model     string
	// from and to bound the span; equal for an instant.
	from, to time.Time
	Tokens
}

// IssueUsage returns the time the issue was held, the tokens attributed
// to it and its account, from one snapshot. An issue that does not exist
// is ErrNotFound.
func (s *Store) IssueUsage(ctx context.Context, id IssueID) (IssueUsage, error) {
	if err := id.Validate(); err != nil {
		return IssueUsage{}, err
	}
	q, end, err := s.beginRead(ctx)
	if err != nil {
		return IssueUsage{}, err
	}
	defer end()
	var out IssueUsage
	if out.Account, out.AccountFrom, err = resolveAccount(ctx, q, id); err != nil {
		return IssueUsage{}, err
	}
	now := s.now()
	own, err := loadHolds(ctx, q, []IssueID{id}, now)
	if err != nil {
		return IssueUsage{}, err
	}
	if len(own) == 0 {
		return out, nil
	}
	var keys []sessionKey
	held := map[sessionKey][]window{}
	for _, h := range own {
		out.Held += h.end.Sub(h.start)
		if _, ok := held[h.key]; !ok {
			keys = append(keys, h.key)
		}
		held[h.key] = append(held[h.key], window{h.start, h.end})
	}
	// Read the records of each stretch the issue was held, by session:
	// those whose time is in it, and the spans that start in it and end
	// later. A span across two stretches is read twice and kept once.
	ur := usageReader{limit: usageScanRows}
	for _, k := range keys {
		for _, w := range mergeWindows(held[k]) {
			if err := ur.read(ctx, q, usageIn, k.principal, k.session, w.from, w.to, w.from); err != nil {
				return IssueUsage{}, err
			}
			if err := ur.read(ctx, q, usageSpansPast, k.principal, k.session,
				string(GranularityTurn), string(GranularitySession), w.to, w.to.Add(MaxUsageSpan), w.to); err != nil {
				return IssueUsage{}, err
			}
		}
	}
	rows := ur.rows
	out.Capped = ur.capped
	// To split a record, every issue its session held matters.
	holds, err := sessionHolds(ctx, q, rows, now)
	if err != nil {
		return IssueUsage{}, err
	}
	prices, err := loadPrices(ctx, q)
	if err != nil {
		return IssueUsage{}, err
	}
	book := newPriceBook(prices)
	var sum usageSum
	var cost costSum
	for i, r := range rows {
		if i%ctxEvery == 0 {
			if err := ctx.Err(); err != nil {
				return IssueUsage{}, err
			}
		}
		d := divide(r, holds[r.key])
		p := d.part(id)
		if p < 0 {
			continue
		}
		part := d.share(r.Tokens, p)
		sum.add(r.model, part)
		cost.add(book, r.model, r.to, part)
		out.Split = out.Split || d.split()
	}
	out.Models, out.Cost = sum.models(), cost.cost()
	return out, nil
}

// claimEventOps are the events that open or may end a claim.
var claimEventOps = []Op{OpClaimTake, OpClaimExpire, OpClaimRelease, OpIssueClose, OpIssueUpdate, OpIssueImport}

// loadHolds reconstructs the holds on issues from their claim events, as
// of now, sorted by issue and then start.
func loadHolds(ctx context.Context, q querier, issues []IssueID, now time.Time) ([]hold, error) {
	var out []hold
	// Holds are per issue, so a long list is read in chunks.
	for chunk := range slices.Chunk(issues, 1000) {
		hs, err := loadHoldsOf(ctx, q, chunk, now)
		if err != nil {
			return nil, err
		}
		out = append(out, hs...)
	}
	slices.SortFunc(out, func(a, b hold) int {
		return cmp.Or(strings.Compare(string(a.issue), string(b.issue)), a.start.Compare(b.start))
	})
	return out, nil
}

// loadHoldsOf is loadHolds for at most a chunk of issues.
func loadHoldsOf(ctx context.Context, q querier, issues []IssueID, now time.Time) ([]hold, error) {
	ids := make([]any, len(issues))
	for i, id := range issues {
		ids[i] = string(id)
	}
	ops := make([]any, len(claimEventOps))
	for i, op := range claimEventOps {
		ops[i] = string(op)
	}
	// Leases of holds still open: renewals move them without an event.
	expires := map[IssueID]claimRow{}
	rows, err := q.QueryContext(ctx, `SELECT issue_id, principal, session, expires_at FROM claims
  WHERE principal IS NOT NULL AND issue_id IN (`+placeholders(len(ids))+`)`, ids...)
	if err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}
	for rows.Next() {
		var c claimRow
		if err := rows.Scan(&c.Issue, &c.Holder.Principal, &c.Holder.Session, &c.ExpiresAt); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("claims: %w", err)
		}
		expires[c.Issue] = c
	}
	if err := closeRows(rows); err != nil {
		return nil, fmt.Errorf("claims: %w", err)
	}

	evs, err := queryEvents(ctx, q, `WHERE target IN (`+placeholders(len(ids))+`) AND op IN (`+
		placeholders(len(ops))+`) ORDER BY seq`, append(ids, ops...)...)
	if err != nil {
		return nil, err
	}
	var out []hold
	open := map[IssueID]*hold{}
	end := func(id IssueID, at time.Time) {
		if h := open[id]; h != nil {
			h.end = maxTime(h.start, at)
			out = append(out, *h)
			delete(open, id)
		}
	}
	for _, e := range evs {
		id := IssueID(e.Target)
		switch e.Op {
		case OpClaimTake:
			var b struct {
				ExpiresAt time.Time `json:"expires_at"`
			}
			_ = json.Unmarshal(e.Before, &b) // none before token capture: the take ends the hold
			end(id, lapsedAt(e.At, b.ExpiresAt))
			open[id] = &hold{issue: id, key: sessionKey{e.Actor.Principal, e.Actor.Session}, start: e.At}
		case OpClaimExpire, OpClaimRelease:
			var b struct {
				ExpiresAt time.Time `json:"expires_at"`
			}
			_ = json.Unmarshal(e.Before, &b) // a malformed state ends the hold at the event
			end(id, lapsedAt(e.At, b.ExpiresAt))
		case OpIssueClose:
			end(id, e.At)
		default: // issue.update, issue.import
			var a struct {
				Status   *Status `json:"status"`
				Released *struct {
					ExpiresAt time.Time `json:"expires_at"`
				} `json:"claim_released"`
			}
			if json.Unmarshal(e.After, &a) != nil {
				break
			}
			switch {
			case a.Released != nil:
				end(id, lapsedAt(e.At, a.Released.ExpiresAt))
			case a.Status != nil && *a.Status != StatusInProgress:
				end(id, e.At)
			}
		}
	}
	for id, h := range open {
		at := now
		if c, ok := expires[id]; ok && c.Holder.Principal == h.key.principal && c.Holder.Session == h.key.session {
			at = minTime(at, c.ExpiresAt.UTC())
		}
		h.end = maxTime(h.start, at)
		out = append(out, *h)
	}
	return out, nil
}

// lapsedAt is when a hold ended by an event at at really ended: at, or
// the lease's expiry if that came first. A zero expiry, from an event
// that does not record one, is at.
func lapsedAt(at, expires time.Time) time.Time {
	if !expires.IsZero() && expires.Before(at) {
		return expires.UTC()
	}
	return at
}

// sessionHolds returns the holds that divide rows: those of the rows'
// sessions, on any issue, that overlap the rows of their session,
// indexed by session. It reads a constant number of queries a chunk of
// sessions or issues, and loads the claim history only of issues whose
// history reaches a session's rows.
func sessionHolds(ctx context.Context, q querier, rows []usageRow, now time.Time) (map[sessionKey]*holdIndex, error) {
	spans := map[sessionKey]window{} // each session's rows, first to last
	for _, r := range rows {
		w, ok := spans[r.key]
		if !ok {
			w = window{r.from, r.to}
		}
		spans[r.key] = window{minTime(w.from, r.from), maxTime(w.to, r.to)}
	}
	keys := slices.SortedFunc(maps.Keys(spans), func(a, b sessionKey) int {
		return cmp.Or(strings.Compare(a.principal, b.principal), strings.Compare(a.session, b.session))
	})
	// The issues the sessions took before their rows ended, each with the
	// earliest start of the rows of a session that took it.
	from := map[IssueID]time.Time{}
	for chunk := range slices.Chunk(keys, 300) {
		terms := make([]string, len(chunk))
		args := []any{string(OpClaimTake)}
		for i, k := range chunk {
			terms[i] = `(principal = ? AND session = ? AND at <= ?)`
			args = append(args, k.principal, k.session, spans[k].to)
		}
		query := `SELECT DISTINCT target, principal, session FROM events WHERE op = ? AND (` + strings.Join(terms, " OR ") + `)` //nolint:gosec // constant terms; values are arguments
		err := scanAll(ctx, q, "issues taken", query, args, func(rs *sql.Rows) error {
			var id IssueID
			var k sessionKey
			if err := rs.Scan(&id, &k.principal, &k.session); err != nil {
				return err
			}
			if t, ok := from[id]; !ok || spans[k].from.Before(t) {
				from[id] = spans[k].from
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Of those, the issues still claimed, or with a claim event since
	// then: a hold that ended before has its last event before.
	ops := make([]any, len(claimEventOps))
	for i, op := range claimEventOps {
		ops[i] = string(op)
	}
	var keep []IssueID
	for chunk := range slices.Chunk(slices.Sorted(maps.Keys(from)), 1000) {
		ids := make([]any, len(chunk))
		for i, id := range chunk {
			ids[i] = string(id)
		}
		reaches := map[IssueID]bool{}
		mark := func(rs *sql.Rows) error {
			var id IssueID
			var last sql.NullTime
			if err := rs.Scan(&id, &last); err != nil {
				return err
			}
			if !last.Valid || !last.Time.Before(from[id]) {
				reaches[id] = true
			}
			return nil
		}
		query := `SELECT target, MAX(at) FROM events WHERE target IN (` + placeholders(len(ids)) + `) AND op IN (` + //nolint:gosec // placeholders only; values are arguments
			placeholders(len(ops)) + `) GROUP BY target`
		if err := scanAll(ctx, q, "claim history", query, append(slices.Clone(ids), ops...), mark); err != nil {
			return nil, err
		}
		query = `SELECT issue_id, NULL FROM claims WHERE principal IS NOT NULL AND issue_id IN (` + placeholders(len(ids)) + `)` //nolint:gosec // placeholders only; values are arguments
		if err := scanAll(ctx, q, "claims", query, ids, mark); err != nil {
			return nil, err
		}
		for _, id := range chunk {
			if reaches[id] {
				keep = append(keep, id)
			}
		}
	}
	all, err := loadHolds(ctx, q, keep, now)
	if err != nil {
		return nil, err
	}
	byKey := map[sessionKey][]hold{}
	for _, h := range all {
		if w, ok := spans[h.key]; ok && !h.start.After(w.to) && h.end.After(w.from) {
			byKey[h.key] = append(byKey[h.key], h)
		}
	}
	out := map[sessionKey]*holdIndex{}
	for k, hs := range byKey {
		out[k] = newHoldIndex(hs)
	}
	return out, nil
}

// scanAll runs query and calls scan for each row; what names the read in
// an error.
func scanAll(ctx context.Context, q querier, what, query string, args []any, scan func(*sql.Rows) error) error {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	for rows.Next() {
		if err := scan(rows); err != nil {
			_ = rows.Close()
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	if err := closeRows(rows); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return nil
}

// usageIn matches a session's records whose time is in [from, to) and
// that overlap it: all but a span that ends at from. Its arguments are
// principal, session, from, to and from again. The session index serves
// it.
const usageIn = `principal = ? AND session = ? AND at >= ? AND at < ?
  AND (at > ? OR span_start IS NULL OR span_start >= at)`

// usageSpansPast matches a session's turn and session records that end
// at or after to and start before it. Spans are at most MaxUsageSpan
// long, which bounds the range. Its arguments are principal, session,
// the two granularities, to, to plus MaxUsageSpan and to again. The span
// index serves it.
const usageSpansPast = `principal = ? AND session = ? AND granularity IN (?, ?)
  AND at >= ? AND at < ? AND span_start < ?`

// usageKey names a record: its session and the harness's request id.
type usageKey struct {
	key       sessionKey
	requestID string
}

// usageReader reads the token_usage rows of one attribution, keeping
// each record once and at most limit of them.
type usageReader struct {
	limit int
	rows  []usageRow
	seen  map[usageKey]bool
	// capped is set once a record was left out for the limit.
	capped bool
}

// read keeps the rows matching where, a constant condition with
// placeholders for args, oldest first. A record kept already does not
// count against the limit again.
func (u *usageReader) read(ctx context.Context, q querier, where string, args ...any) error {
	if u.capped {
		return nil
	}
	// At most len(u.rows) of the rows are repeats, so this many hold one
	// more new record than the limit has room for, if there is one.
	rows, err := readUsage(ctx, q, where, u.limit+1, args...)
	if err != nil {
		return err
	}
	if u.seen == nil {
		u.seen = map[usageKey]bool{}
	}
	for _, r := range rows {
		k := usageKey{r.key, r.requestID}
		if u.seen[k] {
			continue
		}
		if len(u.rows) == u.limit {
			u.capped = true
			return nil
		}
		u.seen[k] = true
		u.rows = append(u.rows, r)
	}
	return nil
}

// readUsage reads at most limit token_usage rows matching where, oldest
// first.
func readUsage(ctx context.Context, q querier, where string, limit int, args ...any) ([]usageRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT principal, session, request_id, model, at, span_start,
  input, output, cache_write, cache_write_1h, cache_read
  FROM token_usage WHERE `+where+` ORDER BY at, request_id LIMIT ?`, append(args, limit)...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []usageRow
	for rows.Next() {
		var r usageRow
		var spanStart sql.NullTime
		var c [5]sql.NullInt64
		if err := rows.Scan(&r.key.principal, &r.key.session, &r.requestID, &r.model, &r.to, &spanStart,
			&c[0], &c[1], &c[2], &c[3], &c[4]); err != nil {
			return nil, fmt.Errorf("usage: %w", err)
		}
		r.to = r.to.UTC()
		r.from = r.to
		if spanStart.Valid {
			r.from = spanStart.Time.UTC()
		}
		for i, p := range []**int64{&r.Input, &r.Output, &r.CacheWrite, &r.CacheWrite1h, &r.CacheRead} {
			if c[i].Valid {
				v := c[i].Int64
				*p = &v
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: %w", err)
	}
	return out, nil
}

// window is a stretch of time, [from, to).
type window struct{ from, to time.Time }

// mergeWindows sorts ws and joins those that touch or overlap, dropping
// empty ones.
func mergeWindows(ws []window) []window {
	slices.SortFunc(ws, func(a, b window) int { return a.from.Compare(b.from) })
	var out []window
	for _, w := range ws {
		switch {
		case !w.from.Before(w.to):
		case len(out) > 0 && !w.from.After(out[len(out)-1].to):
			out[len(out)-1].to = maxTime(out[len(out)-1].to, w.to)
		default:
			out = append(out, w)
		}
	}
	return out
}

// division is how one record divides: the issues its session held over
// it, sorted, each with a weight, then the weight of the time no issue
// was held, last. A weight is time held in nanoseconds, or 1 each for an
// instant; time held by several issues at once counts for each in equal
// shares.
type division struct {
	issues  []IssueID
	weights []float64
}

// holdIndex finds which of a session's holds overlap a time, in time
// logarithmic in the holds plus linear in those found.
type holdIndex struct {
	holds []hold // by start
	// last is a segment tree over holds: last[n] is the latest end in
	// node n's range, with node 1 the root and node n's children 2n and
	// 2n+1.
	last []time.Time
}

// newHoldIndex indexes holds, which it sorts.
func newHoldIndex(holds []hold) *holdIndex {
	slices.SortFunc(holds, func(a, b hold) int {
		return cmp.Or(a.start.Compare(b.start), strings.Compare(string(a.issue), string(b.issue)))
	})
	x := &holdIndex{holds: holds, last: make([]time.Time, 4*len(holds))}
	if len(holds) > 0 {
		x.build(1, 0, len(holds))
	}
	return x
}

func (x *holdIndex) build(node, lo, hi int) time.Time {
	if hi-lo == 1 {
		x.last[node] = x.holds[lo].end
		return x.last[node]
	}
	mid := (lo + hi) / 2
	x.last[node] = maxTime(x.build(2*node, lo, mid), x.build(2*node+1, mid, hi))
	return x.last[node]
}

// overlapping calls f with each hold that starts at or before to and
// ends after from. A nil index has no holds.
func (x *holdIndex) overlapping(from, to time.Time, f func(hold)) {
	if x == nil || len(x.holds) == 0 {
		return
	}
	n, _ := slices.BinarySearchFunc(x.holds, to, func(h hold, t time.Time) int {
		if h.start.After(t) {
			return 1
		}
		return -1
	})
	x.visit(1, 0, len(x.holds), n, from, f)
}

// visit calls f with the holds in node's range [lo, hi), and before n,
// that end after from, skipping every subtree that ends no later.
func (x *holdIndex) visit(node, lo, hi, n int, from time.Time, f func(hold)) {
	if lo >= n || !x.last[node].After(from) {
		return
	}
	if hi-lo == 1 {
		f(x.holds[lo])
		return
	}
	mid := (lo + hi) / 2
	x.visit(2*node, lo, mid, n, from, f)
	x.visit(2*node+1, mid, hi, n, from, f)
}

// divide divides r among its session's holds, x.
//
// A span is swept once over the start and end of each hold inside it.
// S(t) accumulates, over the time from the span's start to t, each
// stretch's length divided by the number of holds then, so a hold's
// share of the span is S at its end less S at its start, and stretches
// held by none are unheld. That is O(k log k) for k holds overlapping
// the span.
func divide(r usageRow, x *holdIndex) division {
	w := map[IssueID]float64{}
	var unheld float64
	if !r.from.Before(r.to) {
		x.overlapping(r.to, r.to, func(h hold) { w[h.issue] = 1 })
		if len(w) == 0 {
			unheld = 1
		}
		return newDivision(w, unheld)
	}
	// pieces are the holds clipped to the span, with their weights.
	type piece struct {
		issue IssueID
		w     float64
	}
	type edge struct {
		at    time.Duration // from the span's start
		piece int
		open  bool
	}
	var pieces []piece
	var edges []edge
	x.overlapping(r.from, r.to, func(h hold) {
		a, b := maxTime(h.start, r.from).Sub(r.from), minTime(h.end, r.to).Sub(r.from)
		if a < b {
			edges = append(edges, edge{a, len(pieces), true}, edge{b, len(pieces), false})
			pieces = append(pieces, piece{issue: h.issue})
		}
	})
	slices.SortFunc(edges, func(a, b edge) int { return cmp.Compare(a.at, b.at) })
	var sum float64 // S at prev
	var prev time.Duration
	active := 0
	for _, e := range edges {
		if d := float64(e.at - prev); active > 0 {
			sum += d / float64(active)
		} else {
			unheld += d
		}
		prev = e.at
		if e.open {
			pieces[e.piece].w -= sum
			active++
		} else {
			pieces[e.piece].w += sum
			active--
		}
	}
	unheld += float64(r.to.Sub(r.from) - prev)
	// An issue's weight is its pieces'.
	slices.SortStableFunc(pieces, func(a, b piece) int { return strings.Compare(string(a.issue), string(b.issue)) })
	var d division
	for i, p := range pieces {
		if i > 0 && p.issue == pieces[i-1].issue {
			d.weights[len(d.weights)-1] += p.w
			continue
		}
		d.issues = append(d.issues, p.issue)
		d.weights = append(d.weights, p.w)
	}
	d.weights = append(d.weights, unheld)
	return d
}

// newDivision orders weights by issue and puts unheld last.
func newDivision(w map[IssueID]float64, unheld float64) division {
	d := division{issues: slices.Sorted(maps.Keys(w))}
	for _, id := range d.issues {
		d.weights = append(d.weights, w[id])
	}
	d.weights = append(d.weights, unheld)
	return d
}

// part is the bucket of issue id, or -1 if the record has none for it.
func (d division) part(id IssueID) int {
	if i, ok := slices.BinarySearch(d.issues, id); ok && d.weights[i] > 0 {
		return i
	}
	return -1
}

// unheld is the bucket of the time no issue was held.
func (d division) unheld() int { return len(d.issues) }

// split reports whether the record divides into more than one part.
func (d division) split() bool { return d.positive() > 1 }

// positive counts the parts with weight.
func (d division) positive() int {
	n := 0
	for _, w := range d.weights {
		if w > 0 {
			n++
		}
	}
	return n
}

// parts divides a count n by the division's weights, in whole tokens
// that sum to n.
func (d division) parts(n int64) []int64 { return apportion(n, d.weights) }

// share is the part of t in the buckets given: for each count t knows,
// the sum of those buckets' parts of it. Unknown counts stay unknown.
// The one-hour cache writes are part of the cache writes, so the
// five-minute rest and the one-hour part divide separately and each
// bucket's cache writes are the sum of its two: divided independently,
// a bucket could get more one-hour writes than writes.
func (d division) share(t Tokens, buckets ...int) Tokens {
	sum := func(n int64) *int64 {
		p, s := d.parts(n), int64(0)
		for _, b := range buckets {
			s += p[b]
		}
		return &s
	}
	var out Tokens
	for _, c := range []struct{ in, out **int64 }{
		{&t.Input, &out.Input}, {&t.Output, &out.Output}, {&t.CacheWrite, &out.CacheWrite}, {&t.CacheRead, &out.CacheRead},
	} {
		if *c.in != nil {
			*c.out = sum(**c.in)
		}
	}
	if t.CacheWrite1h != nil { // and so CacheWrite too, and not less
		out.CacheWrite1h = sum(*t.CacheWrite1h)
		*out.CacheWrite = *sum(*t.CacheWrite - *t.CacheWrite1h) + *out.CacheWrite1h
	}
	return out
}

// apportion divides n into parts in proportion to weights, which are not
// negative and not all zero, by the largest-remainder method: each part
// is its quota rounded down, and what that leaves goes one each to the
// parts with the largest remainders, ties to the earlier part. The parts
// sum to n, and a part with no weight gets nothing.
func apportion(n int64, weights []float64) []int64 {
	var total float64
	for _, w := range weights {
		total += w
	}
	parts := make([]int64, len(weights))
	rems := make([]float64, len(weights))
	var order []int // the parts with weight, by remainder, largest first
	left := n
	for i, w := range weights {
		if w <= 0 {
			continue
		}
		q := float64(n) * (w / total)
		f := math.Floor(q)
		parts[i], rems[i] = int64(f), q-f
		left -= parts[i]
		order = append(order, i)
	}
	if len(order) == 0 {
		return parts
	}
	slices.SortStableFunc(order, func(a, b int) int { return cmp.Compare(rems[b], rems[a]) })
	// Exact quotas leave fewer tokens than there are parts. Float error
	// can leave one more, or take one too many; the loops settle either.
	for i := 0; left > 0; i, left = i+1, left-1 {
		parts[order[i%len(order)]]++
	}
	for left < 0 {
		for _, j := range slices.Backward(order) {
			if left < 0 && parts[j] > 0 {
				parts[j]--
				left++
			}
		}
	}
	return parts
}

// usageSum adds up records, or parts of them, by model.
type usageSum map[string]*modelSum

// modelSum is one model's running counts, and which were ever known.
type modelSum struct {
	n     [5]int64
	known [5]bool
}

// add adds the counts t knows to model's.
func (u *usageSum) add(model string, t Tokens) {
	if *u == nil {
		*u = usageSum{}
	}
	m := (*u)[model]
	if m == nil {
		m = &modelSum{}
		(*u)[model] = m
	}
	m.add(t)
}

// add adds the counts t knows.
func (m *modelSum) add(t Tokens) {
	for i, c := range []*int64{t.Input, t.Output, t.CacheWrite, t.CacheWrite1h, t.CacheRead} {
		if c != nil {
			m.n[i] += *c
			m.known[i] = true
		}
	}
}

// tokens returns the sums, nil where no count was known.
func (m *modelSum) tokens() Tokens {
	var t Tokens
	for i, p := range []**int64{&t.Input, &t.Output, &t.CacheWrite, &t.CacheWrite1h, &t.CacheRead} {
		if m.known[i] {
			*p = &m.n[i]
		}
	}
	return t
}

// models returns the sums by model, sorted.
func (u usageSum) models() []ModelUsage {
	var out []ModelUsage
	for model, m := range u {
		out = append(out, ModelUsage{Model: model, Tokens: m.tokens()})
	}
	slices.SortFunc(out, func(a, b ModelUsage) int { return strings.Compare(a.Model, b.Model) })
	return out
}

// closeRows ends rows, returning the error the iteration or the close
// met.
func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}

// DigestUsage totals a digest window's time and tokens. Held is the time
// issues were held within the window; Models are the tokens of the
// records whose time is in it. Unattributed is the part of Models no
// issue was held for. With the digest's By, only that principal's holds
// and records count; with its Label, only holds of and tokens attributed
// to issues with the label, so nothing is unattributed.
type DigestUsage struct {
	UsageSummary
	Unattributed []ModelUsage
}

// usage fills in d.Usage.
func (q *digestQuery) usage(ctx context.Context, d *Digest) error {
	where, args := `at >= ? AND at <= ?`, []any{q.f.Since, q.now}
	if q.f.By != "" {
		where, args = where+` AND principal = ?`, append(args, q.f.By)
	}
	ur := usageReader{limit: usageScanRows}
	if err := ur.read(ctx, q.tx, where, args...); err != nil {
		return err
	}
	rows, u := ur.rows, &d.Usage
	u.Capped = ur.capped
	q.capped = q.capped || ur.capped

	// The issues held in the window: those with a claim event in it, and
	// those held now.
	issues, err := q.heldIssues(ctx)
	if err != nil {
		return err
	}
	inWindow, err := loadHolds(ctx, q.tx, issues, q.now)
	if err != nil {
		return err
	}
	bySession, err := sessionHolds(ctx, q.tx, rows, q.now)
	if err != nil {
		return err
	}
	counts := func(IssueID) bool { return true }
	if q.f.Label != "" {
		ids := slices.Clone(issues)
		for _, x := range bySession {
			for _, h := range x.holds {
				ids = append(ids, h.issue)
			}
		}
		labeled, err := q.labeled(ctx, ids)
		if err != nil {
			return err
		}
		counts = func(id IssueID) bool { return labeled[id] }
	}

	for _, h := range inWindow {
		if q.f.By != "" && h.key.principal != q.f.By || !counts(h.issue) {
			continue
		}
		if from, to := maxTime(h.start, q.f.Since), minTime(h.end, q.now); from.Before(to) {
			u.Held += to.Sub(from)
		}
	}
	prices, err := loadPrices(ctx, q.tx)
	if err != nil {
		return err
	}
	book := newPriceBook(prices)
	var total, loose usageSum
	var cost costSum
	for i, r := range rows {
		if i%ctxEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		d := divide(r, bySession[r.key])
		if q.f.Label == "" {
			total.add(r.model, r.Tokens)
			cost.add(book, r.model, r.to, r.Tokens)
			if un := d.unheld(); d.weights[un] > 0 {
				loose.add(r.model, d.share(r.Tokens, un))
			}
			u.Split = u.Split || d.split()
			continue
		}
		var in []int // the labeled issues' parts
		for i, id := range d.issues {
			if d.weights[i] > 0 && counts(id) {
				in = append(in, i)
			}
		}
		if len(in) == 0 {
			continue
		}
		part := d.share(r.Tokens, in...)
		total.add(r.model, part)
		cost.add(book, r.model, r.to, part)
		u.Split = u.Split || d.positive() > len(in)
	}
	u.Models, u.Unattributed, u.Cost = total.models(), loose.models(), cost.cost()
	return nil
}

// heldIssues lists the issues with a claim event in the digest window,
// or held now, at most digestRows of them.
func (q *digestQuery) heldIssues(ctx context.Context) ([]IssueID, error) {
	ops := make([]any, len(claimEventOps))
	for i, op := range claimEventOps {
		ops[i] = string(op)
	}
	var out []IssueID
	seen := map[IssueID]bool{}
	for _, sel := range []struct {
		query string
		args  []any
	}{
		{`SELECT DISTINCT target FROM events WHERE at >= ? AND op IN (` + placeholders(len(ops)) + `)`, append([]any{q.f.Since}, ops...)},
		{`SELECT issue_id FROM claims WHERE principal IS NOT NULL`, nil},
	} {
		rows, err := q.tx.QueryContext(ctx, sel.query, sel.args...)
		if err != nil {
			return nil, fmt.Errorf("held issues: %w", err)
		}
		for rows.Next() {
			var id IssueID
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("held issues: %w", err)
			}
			if seen[id] {
				continue
			}
			if len(out) == digestRows {
				q.capped = true
				break
			}
			seen[id] = true
			out = append(out, id)
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("held issues: %w", err)
		}
	}
	return out, nil
}

// labeled returns which of ids have the digest's label.
func (q *digestQuery) labeled(ctx context.Context, ids []IssueID) (map[IssueID]bool, error) {
	out := map[IssueID]bool{}
	for chunk := range slices.Chunk(ids, 1000) {
		args := []any{q.f.Label}
		for _, id := range chunk {
			args = append(args, string(id))
		}
		query := `SELECT issue_id FROM labels WHERE label = ? AND issue_id IN (` + placeholders(len(chunk)) + `)` //nolint:gosec // placeholders only; values are arguments
		rows, err := q.tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("labels: %w", err)
		}
		for rows.Next() {
			var id IssueID
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("labels: %w", err)
			}
			out[id] = true
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("labels: %w", err)
		}
	}
	return out, nil
}
