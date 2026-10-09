package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Cost (design §12.1): the list-price equivalent of tokens, computed when
// read from the prices in effect at each record's time, never stored. A
// part of a record is priced as its own tokens, so the issues' parts of
// a record cost exactly what the whole does.

// Cost is a list-price equivalent: Picodollars (10⁻¹² USD) of the tokens
// that had a price, exactly, and Unpriced set when some tokens did not.
// A nil Picodollars is zero.
type Cost struct {
	Picodollars *big.Int
	Unpriced    bool
}

// priceBook finds the price in effect for a model at a time.
type priceBook map[string][]Price // by model, oldest first

func newPriceBook(ps []Price) priceBook {
	b := priceBook{}
	for _, p := range ps {
		b[p.Model] = append(b[p.Model], p)
	}
	for _, ps := range b {
		slices.SortFunc(ps, func(a, b Price) int { return a.From.Compare(b.From) })
	}
	return b
}

// at returns the rates of the newest price for model whose effective time
// is no later than t, or nil when there is none.
func (b priceBook) at(model string, t time.Time) *Rates {
	ps := b[model]
	n, _ := slices.BinarySearchFunc(ps, t, func(p Price, t time.Time) int {
		if p.From.After(t) {
			return 1
		}
		return -1
	})
	if n == 0 {
		return nil
	}
	return &ps[n-1].Rates
}

// costSum adds up tokens by the rates that price them, so each rate
// multiplies a sum once, and notes tokens that had no price.
type costSum struct {
	byRates map[*Rates]*[5]int64
	// unpriced are the models with tokens and no price.
	unpriced map[string]bool
}

// add prices t, tokens of model at time at, by book. A count is priced
// at its own rate; the five-minute part of a cache write is CacheWrite
// less CacheWrite1h, and all of it when the one-hour part is unknown.
// Unknown counts add nothing.
func (c *costSum) add(book priceBook, model string, at time.Time, t Tokens) {
	var n [5]int64
	for i, v := range []*int64{t.Input, t.Output, t.CacheWrite, t.CacheWrite1h, t.CacheRead} {
		if v != nil {
			n[i] = *v
		}
	}
	n[2] -= n[3]
	r := book.at(model, at)
	if r == nil {
		if n != [5]int64{} {
			if c.unpriced == nil {
				c.unpriced = map[string]bool{}
			}
			c.unpriced[model] = true
		}
		return
	}
	c.addAt(r, n)
}

// addAt adds n to the sums priced by r.
func (c *costSum) addAt(r *Rates, n [5]int64) {
	if c.byRates == nil {
		c.byRates = map[*Rates]*[5]int64{}
	}
	sum := c.byRates[r]
	if sum == nil {
		sum = &[5]int64{}
		c.byRates[r] = sum
	}
	for i := range n {
		sum[i] += n[i]
	}
}

// merge adds b's sums to c's.
func (c *costSum) merge(b costSum) {
	for r, n := range b.byRates {
		c.addAt(r, *n)
	}
	for m := range b.unpriced {
		if c.unpriced == nil {
			c.unpriced = map[string]bool{}
		}
		c.unpriced[m] = true
	}
}

// cost is the sum's cost, exactly.
func (c costSum) cost() Cost {
	out := Cost{Picodollars: new(big.Int), Unpriced: len(c.unpriced) > 0}
	var term big.Int
	for r, n := range c.byRates {
		for i, rate := range []int64{r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead} {
			out.Picodollars.Add(out.Picodollars, term.Mul(big.NewInt(n[i]), big.NewInt(rate)))
		}
	}
	return out
}

// CostBy is what a cost report groups by.
type CostBy string

// The groupings. Account, issue and epic divide each record among the
// issues its session held, as show does; person (the principal that
// reported it) and model take each record whole.
const (
	CostByAccount CostBy = "account"
	CostByIssue   CostBy = "issue"
	CostByEpic    CostBy = "epic"
	CostByPerson  CostBy = "person"
	CostByModel   CostBy = "model"
)

// Valid reports whether b is a grouping.
func (b CostBy) Valid() bool {
	switch b {
	case CostByAccount, CostByIssue, CostByEpic, CostByPerson, CostByModel:
		return true
	}
	return false
}

// Cost report bounds.
const (
	// DefaultCostGroups is how many groups a report lists when not told.
	DefaultCostGroups = 50
	// MaxCostGroups is the most a report lists; the rest are summed.
	MaxCostGroups = 500
	// MaxCostWindow bounds a report's window, as a digest's.
	MaxCostWindow = MaxDigestWindow
)

// CostFilter selects a cost report: records whose time is in [Since,
// Until), Until zero meaning now, grouped By: Limit groups (0 takes
// DefaultCostGroups), then the rest summed into one. Account is the project's default account, for
// issues whose chain sets none; grouping by account needs it.
type CostFilter struct {
	By           CostBy
	Since, Until time.Time
	Account      string
	Limit        int
}

// CostGroup is one group of a cost report: its tokens, summed over
// models, and their cost. Key is the account, issue id, epic id,
// principal or model, or one of proto's CostUnattributed, CostNoEpic and
// OtherModels; Title is the issue's or epic's. Split is set when part of
// the group's tokens came from records shared by time with another group,
// so that part is an estimate.
type CostGroup struct {
	Key   string
	Title string
	Tokens
	Cost  Cost
	Split bool
}

// CostReport is the list-price equivalent of a window's tokens. Groups
// are by cost, largest first, then by tokens and key, and the groups
// past the limit are summed into one more, last. Total covers every record read.
// Unpriced names the models with tokens and no price in effect at their
// time. Capped is set when the window held more records than a report
// reads, so every figure is a lower bound.
type CostReport struct {
	Since, Until time.Time
	Groups       []CostGroup
	Total        CostGroup
	Unpriced     []string
	Capped       bool
}

// costAcc accumulates one group.
type costAcc struct {
	tokens modelSum
	cost   costSum
	split  bool
}

func (a *costAcc) add(book priceBook, model string, at time.Time, t Tokens) {
	a.tokens.add(t)
	a.cost.add(book, model, at, t)
}

func (a *costAcc) merge(b *costAcc) {
	a.tokens.add(b.tokens.tokens())
	a.cost.merge(b.cost)
	a.split = a.split || b.split
}

func (a *costAcc) group(key, title string) CostGroup {
	return CostGroup{Key: key, Title: title, Tokens: a.tokens.tokens(), Cost: a.cost.cost(), Split: a.split}
}

// CostReport prices the tokens reported in f's window, grouped as f
// asks, in one snapshot. It refuses with ErrInvalid an unknown grouping,
// a window with no start, an end before its start or a window longer than
// MaxCostWindow, a limit out of range, and grouping by account without a
// valid default account.
func (s *Store) CostReport(ctx context.Context, f CostFilter) (CostReport, error) {
	now := s.now()
	until := f.Until.UTC()
	if f.Until.IsZero() {
		until = now
	}
	since := f.Since.UTC()
	switch {
	case !f.By.Valid():
		return CostReport{}, fmt.Errorf("%w: by %q must be account, issue, epic, person or model", ErrInvalid, f.By)
	case f.Since.IsZero():
		return CostReport{}, fmt.Errorf("%w: a cost report needs since", ErrInvalid)
	case until.Before(since):
		return CostReport{}, fmt.Errorf("%w: until %s is before since %s", ErrInvalid, until.Format(time.RFC3339), since.Format(time.RFC3339))
	case until.Sub(since) > MaxCostWindow:
		return CostReport{}, fmt.Errorf("%w: a cost report covers at most %d days", ErrInvalid, MaxCostWindow/(24*time.Hour))
	case f.Limit < 0 || f.Limit > MaxCostGroups:
		return CostReport{}, fmt.Errorf("%w: limit must be from 1 to %d, or 0 for %d", ErrInvalid, MaxCostGroups, DefaultCostGroups)
	case f.By == CostByAccount && !ValidAccount(f.Account):
		return CostReport{}, fmt.Errorf("%w: grouping by account needs the project's default account", ErrInvalid)
	}
	limit := cmp.Or(f.Limit, DefaultCostGroups)
	q, end, err := s.beginRead(ctx)
	if err != nil {
		return CostReport{}, err
	}
	defer end()
	ur := usageReader{limit: usageScanRows}
	if err := ur.read(ctx, q, `at >= ? AND at < ?`, since, until); err != nil {
		return CostReport{}, err
	}
	prices, err := loadPrices(ctx, q)
	if err != nil {
		return CostReport{}, err
	}
	book := newPriceBook(prices)
	out := CostReport{Since: since, Until: until, Capped: ur.capped}

	var total costAcc
	for _, r := range ur.rows {
		total.add(book, r.model, r.to, r.Tokens)
	}
	out.Total = total.group("", "")
	out.Unpriced = slices.Sorted(maps.Keys(total.cost.unpriced))

	groups, titles, err := costGroups(ctx, q, f, ur.rows, book, now)
	if err != nil {
		return CostReport{}, err
	}
	for k, a := range groups {
		out.Groups = append(out.Groups, a.group(k, titles[k]))
	}
	slices.SortFunc(out.Groups, func(a, b CostGroup) int {
		return cmp.Or(b.Cost.Picodollars.Cmp(a.Cost.Picodollars), cmp.Compare(total64(b.Tokens), total64(a.Tokens)),
			strings.Compare(a.Key, b.Key))
	})
	if len(out.Groups) > limit {
		var other costAcc
		for _, g := range out.Groups[limit:] {
			other.merge(groups[g.Key])
		}
		out.Groups = append(out.Groups[:limit], other.group(proto.OtherModels, ""))
	}
	return out, nil
}

// costGroups accumulates rows by f.By, with the titles of issue and epic
// groups.
func costGroups(ctx context.Context, q querier, f CostFilter, rows []usageRow, book priceBook, now time.Time) (map[string]*costAcc, map[string]string, error) {
	groups := map[string]*costAcc{}
	acc := func(key string) *costAcc {
		a := groups[key]
		if a == nil {
			a = &costAcc{}
			groups[key] = a
		}
		return a
	}
	if f.By == CostByPerson || f.By == CostByModel {
		for _, r := range rows {
			key := r.key.principal
			if f.By == CostByModel {
				key = r.model
			}
			acc(key).add(book, r.model, r.to, r.Tokens)
		}
		return groups, nil, nil
	}
	holds, err := sessionHolds(ctx, q, rows, now)
	if err != nil {
		return nil, nil, err
	}
	divs := make([]division, len(rows))
	seen := map[IssueID]bool{}
	for i, r := range rows {
		if i%ctxEvery == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		divs[i] = divide(r, holds[r.key])
		for j, id := range divs[i].issues {
			if divs[i].weights[j] > 0 {
				seen[id] = true
			}
		}
	}
	chains, err := loadChains(ctx, q, slices.Sorted(maps.Keys(seen)))
	if err != nil {
		return nil, nil, err
	}
	titles := map[string]string{}
	keys := map[IssueID]string{}
	keyOf := func(id IssueID) string {
		k, ok := keys[id]
		if !ok {
			k = groupKey(f, chains, id, titles)
			keys[id] = k
		}
		return k
	}
	for i, r := range rows {
		d := divs[i]
		// The parts by group, so a record whose parts all fall in one
		// group counts whole there, not split.
		buckets := map[string][]int{}
		for j, id := range d.issues {
			if d.weights[j] > 0 {
				k := keyOf(id)
				buckets[k] = append(buckets[k], j)
			}
		}
		if un := d.unheld(); d.weights[un] > 0 {
			buckets[proto.CostUnattributed] = append(buckets[proto.CostUnattributed], un)
		}
		for k, b := range buckets {
			a := acc(k)
			a.add(book, r.model, r.to, d.share(r.Tokens, b...))
			a.split = a.split || len(buckets) > 1
		}
	}
	return groups, titles, nil
}

// groupKey is the key of issue id's group in a report by f.By, noting
// the title of an issue or epic group in titles.
func groupKey(f CostFilter, c chains, id IssueID, titles map[string]string) string {
	switch f.By {
	case CostByAccount:
		return cmp.Or(c.account(id), f.Account)
	case CostByEpic:
		e := c.epic(id)
		if e == "" {
			return proto.CostNoEpic
		}
		titles[string(e)] = c[e].title
		return string(e)
	}
	titles[string(id)] = c[id].title
	return string(id)
}

// total64 is the sum of the counts t knows, to rank groups.
func total64(t Tokens) int64 {
	var n int64
	for _, c := range []*int64{t.Input, t.Output, t.CacheWrite, t.CacheRead} {
		if c != nil {
			n += *c
		}
	}
	return n
}

// chainRow is what an issue's account and epic need of it.
type chainRow struct {
	parent  IssueID
	typ     IssueType
	account string
	title   string
}

// chains are issues and their ancestors, by id.
type chains map[IssueID]chainRow

// loadChains reads ids and all their ancestors, a generation at a time,
// in chunks.
func loadChains(ctx context.Context, q querier, ids []IssueID) (chains, error) {
	out := chains{}
	for depth := 0; len(ids) > 0 && depth < maxParentDepth; depth++ {
		var next []IssueID
		for chunk := range slices.Chunk(ids, 1000) {
			args := make([]any, len(chunk))
			for i, id := range chunk {
				args[i] = string(id)
			}
			query := `SELECT id, parent_id, type, account, title FROM issues WHERE id IN (` + placeholders(len(args)) + `)` //nolint:gosec // placeholders only; values are arguments
			err := scanAll(ctx, q, "issue chains", query, args, func(rs *sql.Rows) error {
				var id IssueID
				var parent, account sql.NullString
				var c chainRow
				if err := rs.Scan(&id, &parent, &c.typ, &account, &c.title); err != nil {
					return err
				}
				c.parent, c.account = IssueID(parent.String), account.String
				out[id] = c
				if _, ok := out[c.parent]; c.parent != "" && !ok {
					next = append(next, c.parent)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		slices.Sort(next)
		ids = slices.Compact(next)
	}
	return out, nil
}

// account is the account id's chain sets, "" when none does.
func (c chains) account(id IssueID) string {
	for range maxParentDepth {
		r, ok := c[id]
		if !ok {
			return ""
		}
		if r.account != "" {
			return r.account
		}
		id = r.parent
	}
	return ""
}

// epic is id, or its nearest ancestor, that is an epic; "" when none is.
func (c chains) epic(id IssueID) IssueID {
	for range maxParentDepth {
		r, ok := c[id]
		if !ok {
			return ""
		}
		if r.typ == TypeEpic {
			return id
		}
		id = r.parent
	}
	return ""
}
