package store

import (
	"context"
	"database/sql"
	"maps"
	"math/big"
	"slices"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Amortized cost (design §12.1): what flat-rate plans actually cost,
// split across the work their principals did. For each calendar month
// (UTC) a report touches, each plan's monthly total, fee × seats, is
// split across the report's groups in proportion to the tokens, of all
// kinds, its principals' records gave each group that month; the tokens
// of the month outside the window take their share out of the report, so
// a month the window covers in part gets the part its tokens in the
// window are of the month's. A month whose principals reported no tokens
// is prorated by time instead, into (unattributed). Each split is exact
// (splitExact), so a whole month sums to the total to the picodollar.
//
// A fee accrues by time: the month that holds now has cost only the part
// of its total that has passed, and the months after it nothing, however
// far the window reaches.

// picosPerMicro is picodollars in a micro-dollar.
const picosPerMicro = 1_000_000

// planMonth is one plan's terms in one month a report touches.
type planMonth struct {
	month time.Time
	// fee is the month's total, fee × seats, in picodollars.
	fee *big.Int
	// principals are those whose usage the plan pays for.
	principals []string
	// tokens are all the month's tokens of the principals' records, in
	// the window or not.
	tokens int64
}

// monthPrincipal names one principal's records in one month.
type monthPrincipal struct {
	month     time.Time
	principal string
}

// amortizer splits plans' monthly totals across one report's groups.
// A nil amortizer is a server with no plans: it notes nothing and splits
// nothing.
type amortizer struct {
	// until is the window's end, or now if that is sooner.
	since, until, now time.Time
	months            []planMonth
	// covering lists, by month and principal, the months' plans that pay
	// for the principal's records, as indexes into months.
	covering map[monthPrincipal][]int
}

// monthOf is the first instant of t's month, UTC.
func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// newAmortizer reads the plans on q and the month totals of the tokens
// their principals reported in each month [since, until) touches, up to
// now. It is nil when there are no plans.
func newAmortizer(ctx context.Context, q querier, since, until, now time.Time) (*amortizer, error) {
	plans, err := loadPlans(ctx, q)
	if err != nil || len(plans) == 0 {
		return nil, err
	}
	z := &amortizer{since: since, until: minTime(until, now), now: now, covering: map[monthPrincipal][]int{}}
	for m := monthOf(since); m.Before(z.until); m = m.AddDate(0, 1, 0) {
		// The terms in effect: each name's latest row from m or before.
		// Plans are by name, then month.
		in := map[string]Plan{}
		for _, p := range plans {
			if !p.From.After(m) {
				in[p.Name] = p
			}
		}
		var who []string
		for _, name := range slices.Sorted(maps.Keys(in)) {
			p := in[name]
			if p.Fee == 0 || p.Seats == 0 {
				continue
			}
			fee := new(big.Int).Mul(big.NewInt(p.Fee), big.NewInt(int64(p.Seats)))
			z.months = append(z.months, planMonth{month: m, fee: fee.Mul(fee, big.NewInt(picosPerMicro)), principals: p.Principals})
			for _, pr := range p.Principals {
				k := monthPrincipal{m, pr}
				z.covering[k] = append(z.covering[k], len(z.months)-1)
				who = append(who, pr)
			}
		}
		slices.Sort(who)
		sums, err := monthTokens(ctx, q, m, slices.Compact(who))
		if err != nil {
			return nil, err
		}
		for i := range z.months {
			if z.months[i].month.Equal(m) {
				for _, pr := range z.months[i].principals {
					z.months[i].tokens += sums[pr]
				}
			}
		}
	}
	return z, nil
}

// monthTokens sums each principal's tokens, of all kinds, in the month
// from m. The one-hour cache writes are part of the cache writes.
func monthTokens(ctx context.Context, q querier, m time.Time, principals []string) (map[string]int64, error) {
	out := map[string]int64{}
	for chunk := range slices.Chunk(principals, 1000) {
		args := []any{m, m.AddDate(0, 1, 0)}
		for _, p := range chunk {
			args = append(args, p)
		}
		query := `SELECT principal, ` + sumInt(`COALESCE(input, 0) + COALESCE(output, 0) + COALESCE(cache_write, 0) + COALESCE(cache_read, 0)`) + `
  FROM token_usage WHERE at >= ? AND at < ? AND principal IN (` + placeholders(len(chunk)) + `) GROUP BY principal` //nolint:gosec // constant sum and placeholders only; values are arguments
		err := scanAll(ctx, q, "month tokens", query, args, func(rs *sql.Rows) error {
			var p string
			var n int64
			if err := rs.Scan(&p, &n); err != nil {
				return err
			}
			out[p] = n
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// note adds tokens t, of record r, to a's weight in each plan month
// that pays for r.
func (z *amortizer) note(a *costAcc, r usageRow, t Tokens) {
	if z == nil {
		return
	}
	in := z.covering[monthPrincipal{monthOf(r.to), r.key.principal}]
	if len(in) == 0 {
		return
	}
	if a.planTokens == nil {
		a.planTokens = map[int]int64{}
	}
	for _, i := range in {
		a.planTokens[i] += total64(t)
	}
}

// split sets each group's amortized cost: its part of every plan month,
// by its weight there. A month with no tokens is prorated by the time
// the window covers of it, up to now, into (unattributed), which acc
// makes when missing. A month with tokens that holds now is split for
// the part of its fee that has accrued.
func (z *amortizer) split(groups map[string]*costAcc, acc func(string) *costAcc) {
	if z == nil {
		return
	}
	keys := slices.Sorted(maps.Keys(groups))
	for i, pm := range z.months {
		end := pm.month.AddDate(0, 1, 0)
		if pm.tokens == 0 {
			if covered := minTime(end, z.until).Sub(maxTime(pm.month, z.since)); covered > 0 {
				acc(proto.CostUnattributed).addAmortized(prorate(pm.fee, covered, end.Sub(pm.month)))
			}
			continue
		}
		fee := pm.fee
		if z.now.Before(end) {
			fee = prorate(fee, z.now.Sub(pm.month), end.Sub(pm.month))
		}
		weights := make([]int64, len(keys), len(keys)+1)
		outside := pm.tokens
		for j, k := range keys {
			weights[j] = groups[k].planTokens[i]
			outside -= weights[j]
		}
		parts := splitExact(fee, append(weights, max(outside, 0)))
		for j, k := range keys {
			groups[k].addAmortized(parts[j])
		}
	}
	for _, a := range groups {
		a.addAmortized(new(big.Int))
	}
}

// prorate is the part d of length is of fee, rounded down.
func prorate(fee *big.Int, d, length time.Duration) *big.Int {
	part := new(big.Int).Mul(fee, big.NewInt(int64(d)))
	return part.Quo(part, big.NewInt(int64(length)))
}

// total sums the groups' amortized costs, nil when there are no plans.
func (z *amortizer) total(groups map[string]*costAcc) *big.Int {
	if z == nil {
		return nil
	}
	out := new(big.Int)
	for _, a := range groups {
		out.Add(out, a.amortized)
	}
	return out
}
