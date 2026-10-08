package server

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// usage stores the usage records a client sends for its own session
// (design §12.1); the principal, session and machine are the
// connection's.
func usage(ctx context.Context, s *Server, a store.Actor, in proto.UsageArgs) (any, *proto.Error) {
	recs := make([]store.UsageRecord, len(in.Records))
	for i, r := range in.Records {
		recs[i] = store.UsageRecord{Harness: r.Harness, RequestID: r.RequestID, Model: r.Model, At: r.At,
			Granularity: store.Granularity(r.Granularity), SpanStart: r.SpanStart, Tokens: store.Tokens(r.Tokens)}
	}
	added, err := s.cfg.Store.AddUsage(ctx, a, recs)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpUsage, "", 0, err)
	}
	return proto.UsageResult{Added: added.Added, Duplicates: added.Duplicates}, nil
}

// wireIssueUsage is show's usage, with the server's default account when
// nothing in the issue's chain sets one.
func (s *Server) wireIssueUsage(u store.IssueUsage) *proto.IssueUsage {
	out := &proto.IssueUsage{Account: u.Account, AccountFrom: string(u.AccountFrom), HeldSeconds: seconds(u.Held),
		Split: u.Split, Models: wireModels(u.Models), Capped: u.Capped}
	if out.Account == "" {
		out.Account = s.cfg.Account
	}
	return out
}

// wireDigestUsage is digest's usage, or nil when the window has none.
func wireDigestUsage(u store.DigestUsage) *proto.DigestUsage {
	if u.Held == 0 && len(u.Models) == 0 {
		return nil
	}
	return &proto.DigestUsage{HeldSeconds: seconds(u.Held), Models: wireModels(u.Models), Unattributed: wireModels(u.Unattributed)}
}

// wireModels converts the store's sums, in model order. Past
// proto.MaxUsageModels, the models with the fewest tokens are summed into
// one proto.OtherModels entry, last.
func wireModels(ms []store.ModelUsage) []proto.ModelTokens {
	keep := map[string]bool{}
	if len(ms) > proto.MaxUsageModels {
		ranked := slices.Clone(ms)
		slices.SortStableFunc(ranked, func(a, b store.ModelUsage) int { return cmp.Compare(total(b.Tokens), total(a.Tokens)) })
		for _, m := range ranked[:proto.MaxUsageModels] {
			keep[m.Model] = true
		}
	}
	var out []proto.ModelTokens
	other := proto.ModelTokens{Model: proto.OtherModels}
	for _, m := range ms {
		if len(keep) == 0 || keep[m.Model] {
			out = append(out, proto.ModelTokens{Model: m.Model, Tokens: proto.Tokens(m.Tokens)})
			continue
		}
		o := &other.Tokens
		o.Input = addCount(o.Input, m.Input)
		o.Output = addCount(o.Output, m.Output)
		o.CacheWrite = addCount(o.CacheWrite, m.CacheWrite)
		o.CacheWrite1h = addCount(o.CacheWrite1h, m.CacheWrite1h)
		o.CacheRead = addCount(o.CacheRead, m.CacheRead)
	}
	if len(keep) > 0 {
		out = append(out, other)
	}
	return out
}

// addCount adds count v to sum; nil, unknown, adds nothing, and a sum
// stays nil until a known count arrives.
func addCount(sum, v *int64) *int64 {
	if v == nil {
		return sum
	}
	n := *v
	if sum != nil {
		n += *sum
	}
	return &n
}

// total is the tokens a model's known counts add up to.
func total(t store.Tokens) int64 {
	var n int64
	for _, c := range []*int64{t.Input, t.Output, t.CacheWrite, t.CacheRead} {
		if c != nil {
			n += *c
		}
	}
	return n
}

// seconds is d in whole seconds.
func seconds(d time.Duration) int64 { return int64(d / time.Second) }
