package store

import (
	"math/big"
	"slices"
	"time"
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
	byRates  map[*Rates]*[5]int64
	unpriced bool
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
			c.unpriced = true
		}
		return
	}
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

// cost is the sum's cost, exactly.
func (c costSum) cost() Cost {
	out := Cost{Picodollars: new(big.Int), Unpriced: c.unpriced}
	var term big.Int
	for r, n := range c.byRates {
		for i, rate := range []int64{r.Input, r.Output, r.CacheWrite, r.CacheWrite1h, r.CacheRead} {
			out.Picodollars.Add(out.Picodollars, term.Mul(big.NewInt(n[i]), big.NewInt(rate)))
		}
	}
	return out
}
