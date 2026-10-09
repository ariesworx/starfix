package server

import (
	"context"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// parseTime reads a price's from or a cost report's until: a date
// (midnight UTC) or an RFC 3339 time. what names the field in the error.
func parseTime(what, s string) (time.Time, error) {
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%w: %s %q must be a date (2006-01-02) or a time (RFC 3339)", store.ErrInvalid, what, s)
}

// priceSet sets a model's rates; the store refuses anyone but an admin.
func priceSet(ctx context.Context, s *Server, a store.Actor, in proto.PriceSetArgs) (any, *proto.Error) {
	from, err := parseTime("from", in.From)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpPriceSet, "", 0, err)
	}
	c, err := s.cfg.Store.SetPrice(ctx, a, in.Model, from, store.Rates(in.Rates))
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpPriceSet, "", 0, err)
	}
	return proto.PriceSetResult{Change: string(c)}, nil
}

func prices(ctx context.Context, s *Server, _ store.Actor, _ proto.PricesArgs) (any, *proto.Error) {
	ps, err := s.cfg.Store.Prices(ctx)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpPrices, "", 0, err)
	}
	out := proto.PricesResult{Prices: []proto.Price{}}
	for _, p := range ps {
		out.Prices = append(out.Prices, proto.Price{Model: p.Model, From: p.From, Rates: proto.Rates(p.Rates), SetBy: p.SetBy, SetAt: p.SetAt})
	}
	return out, nil
}

// cost reports the list-price equivalent of a window's tokens by group.
func cost(ctx context.Context, s *Server, _ store.Actor, in proto.CostArgs) (any, *proto.Error) {
	f, err := s.costFilter(in)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpCost, "", 0, err)
	}
	r, err := s.cfg.Store.CostReport(ctx, f)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpCost, "", 0, err)
	}
	out := proto.CostResult{By: in.By, Since: r.Since, Until: r.Until, Groups: []proto.CostGroup{}, Total: wireCostGroup(r.Total),
		Truncated: r.Capped}
	for _, g := range r.Groups {
		out.Groups = append(out.Groups, wireCostGroup(g))
	}
	out.Unpriced = r.Unpriced[:min(len(r.Unpriced), proto.MaxUsageModels)]
	return out, nil
}

// costFilter reads a cost report's arguments; since is required.
func (s *Server) costFilter(in proto.CostArgs) (store.CostFilter, error) {
	f := store.CostFilter{By: store.CostBy(in.By), Account: s.cfg.Account, Limit: in.Limit}
	if in.Since == "" {
		return f, fmt.Errorf("%w: a cost report needs since: a date, a time or a duration such as 7d", store.ErrInvalid)
	}
	since, window, err := parseSince(in.Since)
	if err != nil {
		return f, err
	}
	if f.Since = since; window > 0 {
		f.Since = s.cfg.Store.Now().Add(-window)
	}
	if in.Until != "" {
		if f.Until, err = parseTime("until", in.Until); err != nil {
			return f, err
		}
	}
	return f, nil
}

func wireCostGroup(g store.CostGroup) proto.CostGroup {
	return proto.CostGroup{Key: g.Key, Title: g.Title, Tokens: proto.Tokens(g.Tokens), CostUSD: proto.USD(g.Cost.Picodollars),
		Unpriced: g.Cost.Unpriced, Split: g.Split}
}
