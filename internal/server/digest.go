package server

import (
	"context"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// defaultWindow is the digest window when since is not given.
const defaultWindow = 24 * time.Hour

// parseSince reads a digest's since: an RFC 3339 time, a date (midnight
// UTC), or a duration back from now in Go's syntax plus whole days (7d)
// and weeks (2w). It returns a time or a window, never both.
func parseSince(s string) (time.Time, time.Duration, error) {
	if s == "" {
		return time.Time{}, defaultWindow, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, 0, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, 0, nil
	}
	bad := fmt.Errorf("%w: since %q must be a time (RFC 3339), a date (2006-01-02) or a duration such as 24h or 7d",
		store.ErrInvalid, s)
	d, err := proto.ParseDuration(s)
	if err != nil {
		return time.Time{}, 0, bad
	}
	return time.Time{}, d, nil
}

func digest(ctx context.Context, s *Server, _ store.Actor, in proto.DigestArgs) (any, *proto.Error) {
	since, window, err := parseSince(in.Since)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpDigest, "", 0, err)
	}
	d, err := s.cfg.Store.Digest(ctx, store.DigestFilter{Since: since, Window: window, By: in.By, Label: in.Label})
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpDigest, "", 0, err)
	}
	return wireDigest(d), nil
}

func wireDigest(d store.Digest) proto.DigestResult {
	items := func(sec store.DigestSection) []proto.DigestItem {
		var out []proto.DigestItem
		for _, it := range sec.Items {
			w := proto.DigestItem{ID: string(it.ID), Title: it.Title, Priority: int(it.Priority), By: it.By,
				At: it.At.UTC(), Note: it.Note, From: string(it.From)}
			for _, b := range it.BlockedBy {
				w.BlockedBy = append(w.BlockedBy, string(b))
			}
			out = append(out, w)
		}
		return out
	}
	return proto.DigestResult{
		Since: d.Since.UTC(), Until: d.Now.UTC(),
		Totals: proto.DigestTotals{Events: d.Events, Closed: d.Closed.Total, Started: d.Started.Total,
			InProgress: d.InProgress.Total, Stalled: d.Stalled.Total, Blocked: d.Blocked.Total,
			HandedOff: d.HandedOff.Total, Created: d.Created.Total, Discovered: d.Discovered.Total},
		Closed: items(d.Closed), Started: items(d.Started), InProgress: items(d.InProgress),
		Stalled: items(d.Stalled), Blocked: items(d.Blocked), HandedOff: items(d.HandedOff),
		Created: items(d.Created), Discovered: items(d.Discovered), Usage: wireDigestUsage(d.Usage), Truncated: d.Capped,
	}
}
