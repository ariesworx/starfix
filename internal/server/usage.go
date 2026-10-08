package server

import (
	"context"
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

func wireModels(ms []store.ModelUsage) []proto.ModelTokens {
	var out []proto.ModelTokens
	for _, m := range ms {
		out = append(out, proto.ModelTokens{Model: m.Model, Tokens: proto.Tokens(m.Tokens)})
	}
	return out
}

// seconds is d in whole seconds.
func seconds(d time.Duration) int64 { return int64(d / time.Second) }
