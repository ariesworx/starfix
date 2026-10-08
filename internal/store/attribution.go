package store

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
//     after the lease lapsed and before the reaper ran);
//   - claim.expire, whose before state holds the lease's expiry, when
//     the hold really ended;
//   - issue.close (close and finish end the claim);
//   - issue.update or issue.import that moves the issue out of
//     in_progress. A releasing handoff does that: a claimed issue is
//     always in_progress, because start sets it and update cannot change
//     it while the claim is live;
//   - issue.import whose after state has claim_released: an import that
//     reassigned the issue, or moved it out of in_progress, ended the
//     claim, at its lease's expiry if that came first.
//
// A hold still open ends now, or when its lease ran out if the reaper has
// not yet noticed. Renewals are not in the log, so a hold that lapsed
// before a takeover counts until the takeover, at most a reap interval
// late. A request record goes to the issues its session held at its
// time, shared evenly if it held several. A turn or session record is
// split over its span by time held, sharing each stretch evenly among the
// issues held then; stretches with nothing held are unattributed.

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
}

// ModelUsage is one model's tokens, rounded to whole tokens. A count is
// nil when no record that contributed reported it; otherwise it sums the
// records that did.
type ModelUsage struct {
	Model string
	Tokens
}

// usageScanRows bounds the usage rows one read takes.
const usageScanRows = 100_000

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
	key   sessionKey
	model string
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
	spans := map[sessionKey][2]time.Time{}
	for _, h := range own {
		out.Held += h.end.Sub(h.start)
		sp, ok := spans[h.key]
		if !ok {
			keys = append(keys, h.key)
			sp = [2]time.Time{h.start, h.end}
		}
		spans[h.key] = [2]time.Time{minTime(sp[0], h.start), maxTime(sp[1], h.end)}
	}
	// To split a record, every issue its session held matters.
	holds, err := sessionHolds(ctx, q, keys, now)
	if err != nil {
		return IssueUsage{}, err
	}
	var rows []usageRow
	for _, k := range keys {
		sp := spans[k]
		rs, capped, err := readUsage(ctx, q, `principal = ? AND session = ? AND at >= ? AND COALESCE(span_start, at) <= ?`,
			usageScanRows-len(rows), k.principal, k.session, sp[0], sp[1])
		if err != nil {
			return IssueUsage{}, err
		}
		rows = append(rows, rs...)
		out.Capped = out.Capped || capped
	}
	var sum usageSum
	for _, r := range rows {
		share := shares(r, holds[r.key])[id]
		if share > 0 {
			sum.add(r, share)
			out.Split = out.Split || share < 1-splitEpsilon
		}
	}
	out.Models = sum.models()
	return out, nil
}

// splitEpsilon absorbs float error in a share that is really whole.
const splitEpsilon = 1e-9

// claimEventOps are the events that open or may end a claim.
var claimEventOps = []Op{OpClaimTake, OpClaimExpire, OpIssueClose, OpIssueUpdate, OpIssueImport}

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
			end(id, e.At)
			open[id] = &hold{issue: id, key: sessionKey{e.Actor.Principal, e.Actor.Session}, start: e.At}
		case OpClaimExpire:
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

// sessionHolds returns every hold of the sessions keys, on any issue, by
// session.
func sessionHolds(ctx context.Context, q querier, keys []sessionKey, now time.Time) (map[sessionKey][]hold, error) {
	var issues []IssueID
	seen := map[IssueID]bool{}
	for _, k := range keys {
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT target FROM events WHERE op = ? AND principal = ? AND session = ?`,
			string(OpClaimTake), k.principal, k.session)
		if err != nil {
			return nil, fmt.Errorf("issues taken: %w", err)
		}
		for rows.Next() {
			var id IssueID
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("issues taken: %w", err)
			}
			if !seen[id] {
				seen[id] = true
				issues = append(issues, id)
			}
		}
		if err := closeRows(rows); err != nil {
			return nil, fmt.Errorf("issues taken: %w", err)
		}
	}
	all, err := loadHolds(ctx, q, issues, now)
	if err != nil {
		return nil, err
	}
	want := map[sessionKey]bool{}
	for _, k := range keys {
		want[k] = true
	}
	out := map[sessionKey][]hold{}
	for _, h := range all {
		if want[h.key] {
			out[h.key] = append(out[h.key], h)
		}
	}
	return out, nil
}

// readUsage reads at most limit token_usage rows matching where, a
// constant condition with placeholders for args, oldest first, and
// reports whether more matched.
func readUsage(ctx context.Context, q querier, where string, limit int, args ...any) ([]usageRow, bool, error) {
	if limit <= 0 {
		return nil, true, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT principal, session, model, at, span_start,
  input, output, cache_write, cache_write_1h, cache_read
  FROM token_usage WHERE `+where+` ORDER BY at LIMIT ?`, append(args, limit+1)...) //nolint:gosec // where is a constant from the callers
	if err != nil {
		return nil, false, fmt.Errorf("usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []usageRow
	for rows.Next() {
		if len(out) == limit {
			return out, true, nil
		}
		var r usageRow
		var spanStart sql.NullTime
		var c [5]sql.NullInt64
		if err := rows.Scan(&r.key.principal, &r.key.session, &r.model, &r.to, &spanStart,
			&c[0], &c[1], &c[2], &c[3], &c[4]); err != nil {
			return nil, false, fmt.Errorf("usage: %w", err)
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
		return nil, false, fmt.Errorf("usage: %w", err)
	}
	return out, false, nil
}

// shares divides a record among the issues its session held: by issue,
// the fraction of the record that is the issue's. What is left of 1 is
// unattributed.
func shares(r usageRow, holds []hold) map[IssueID]float64 {
	out := map[IssueID]float64{}
	if !r.from.Before(r.to) {
		var held []IssueID
		for _, h := range holds {
			if !r.to.Before(h.start) && r.to.Before(h.end) {
				held = append(held, h.issue)
			}
		}
		for _, id := range held {
			out[id] += 1 / float64(len(held))
		}
		return out
	}
	// Cut the span at every hold boundary inside it; each piece is then
	// wholly held, or not, by each hold.
	cuts := []time.Time{r.from, r.to}
	for _, h := range holds {
		for _, t := range []time.Time{h.start, h.end} {
			if t.After(r.from) && t.Before(r.to) {
				cuts = append(cuts, t)
			}
		}
	}
	slices.SortFunc(cuts, time.Time.Compare)
	cuts = slices.CompactFunc(cuts, time.Time.Equal)
	whole := float64(r.to.Sub(r.from))
	for i := range len(cuts) - 1 {
		a, b := cuts[i], cuts[i+1]
		var held []IssueID
		for _, h := range holds {
			if !h.start.After(a) && !h.end.Before(b) {
				held = append(held, h.issue)
			}
		}
		for _, id := range held {
			out[id] += float64(b.Sub(a)) / whole / float64(len(held))
		}
	}
	return out
}

// usageSum adds up records, or parts of them, by model.
type usageSum map[string]*modelSum

// modelSum is one model's running counts, and which were ever known.
type modelSum struct {
	n     [5]float64
	known [5]bool
}

// add adds share of r.
func (u *usageSum) add(r usageRow, share float64) {
	if *u == nil {
		*u = usageSum{}
	}
	m := (*u)[r.model]
	if m == nil {
		m = &modelSum{}
		(*u)[r.model] = m
	}
	for i, c := range []*int64{r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead} {
		if c != nil {
			m.n[i] += float64(*c) * share
			m.known[i] = true
		}
	}
}

// models returns the sums by model, sorted, rounded to whole tokens.
func (u usageSum) models() []ModelUsage {
	var out []ModelUsage
	for model, m := range u {
		mu := ModelUsage{Model: model}
		for i, p := range []**int64{&mu.Input, &mu.Output, &mu.CacheWrite, &mu.CacheWrite1h, &mu.CacheRead} {
			if m.known[i] {
				v := int64(math.Round(m.n[i]))
				*p = &v
			}
		}
		out = append(out, mu)
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
	rows, capped, err := readUsage(ctx, q.tx, where, usageScanRows, args...)
	if err != nil {
		return err
	}
	u := &d.Usage
	u.Capped = capped
	q.capped = q.capped || capped

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
	var keys []sessionKey
	seen := map[sessionKey]bool{}
	for _, r := range rows {
		if !seen[r.key] {
			seen[r.key] = true
			keys = append(keys, r.key)
		}
	}
	bySession, err := sessionHolds(ctx, q.tx, keys, q.now)
	if err != nil {
		return err
	}
	counts := func(IssueID) bool { return true }
	if q.f.Label != "" {
		ids := slices.Clone(issues)
		for _, hs := range bySession {
			for _, h := range hs {
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
	var total, loose usageSum
	for _, r := range rows {
		attributed := 0.0
		for id, sh := range shares(r, bySession[r.key]) {
			if counts(id) {
				attributed += sh
			}
		}
		switch {
		case q.f.Label != "":
			if attributed > 0 {
				total.add(r, attributed)
				u.Split = u.Split || attributed < 1-splitEpsilon
			}
		default:
			total.add(r, 1)
			if rest := 1 - attributed; rest > splitEpsilon {
				loose.add(r, rest)
			}
		}
	}
	u.Models, u.Unattributed = total.models(), loose.models()
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
