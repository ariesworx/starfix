package server

import (
	"context"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// hoursLog logs the caller's own time on an issue; the principal is the
// connection's.
func hoursLog(ctx context.Context, s *Server, a store.Actor, in proto.HoursLogArgs) (any, *proto.Error) {
	fail := func(err error) (any, *proto.Error) { return nil, s.mapErr(ctx, proto.OpHoursLog, in.ID, 0, err) }
	// A count past a day would overflow a Duration on its way to the store.
	if most := int64(store.MaxHoursEntry / time.Second); in.Seconds < 0 || in.Seconds > most {
		return fail(fmt.Errorf("%w: seconds must be from 60 to %d", store.ErrInvalid, most))
	}
	nh := store.NewHours{Issue: store.IssueID(in.ID), Duration: time.Duration(in.Seconds) * time.Second, Note: in.Note, Idem: in.Idem}
	if in.On != "" {
		on, err := time.Parse(time.DateOnly, in.On)
		if err != nil {
			return fail(fmt.Errorf("%w: on %q must be a date (2006-01-02)", store.ErrInvalid, in.On))
		}
		nh.On = on
	}
	e, err := s.cfg.Store.LogHours(ctx, a, nh)
	if err != nil {
		return fail(err)
	}
	return proto.HoursLogResult{ID: e.ID, On: e.On.Format(time.DateOnly), Undone: e.Undone}, nil
}

// hoursDelete undoes an entry: the caller's own, or anyone's for an
// admin; the store decides.
func hoursDelete(ctx context.Context, s *Server, a store.Actor, in proto.HoursDeleteArgs) (any, *proto.Error) {
	e, err := s.cfg.Store.DeleteHours(ctx, a, in.ID, in.Idem)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpHoursDelete, in.ID, 0, err)
	}
	return proto.HoursDeleteResult{ID: e.ID}, nil
}

func hours(ctx context.Context, s *Server, _ store.Actor, in proto.HoursArgs) (any, *proto.Error) {
	l, err := s.cfg.Store.Hours(ctx, store.HoursFilter{Issue: store.IssueID(in.Issue), Principal: in.By, Limit: in.Limit})
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpHours, in.Issue, 0, err)
	}
	out := proto.HoursResult{Entries: []proto.HoursEntry{}, More: l.More}
	for _, e := range l.Entries {
		out.Entries = append(out.Entries, proto.HoursEntry{ID: e.ID, Issue: string(e.Issue), Principal: e.Principal,
			On: e.On.Format(time.DateOnly), Seconds: seconds(e.Duration), Note: e.Note, At: e.At})
	}
	return out, nil
}

// planSet sets a plan's terms; the store refuses anyone but an admin.
func planSet(ctx context.Context, s *Server, a store.Actor, in proto.PlanSetArgs) (any, *proto.Error) {
	from, err := time.Parse("2006-01", in.From)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpPlanSet, "", 0, fmt.Errorf("%w: from %q must be a month (2006-01)", store.ErrInvalid, in.From))
	}
	c, err := s.cfg.Store.SetPlan(ctx, a, store.NewPlan{Name: in.Name, From: from, Fee: in.Fee, Seats: in.Seats, Principals: in.Principals})
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpPlanSet, "", 0, err)
	}
	return proto.PlanSetResult{Change: string(c)}, nil
}

func plans(ctx context.Context, s *Server, _ store.Actor, _ proto.PlansArgs) (any, *proto.Error) {
	ps, err := s.cfg.Store.Plans(ctx)
	if err != nil {
		return nil, s.mapErr(ctx, proto.OpPlans, "", 0, err)
	}
	out := proto.PlansResult{Plans: []proto.Plan{}}
	for _, p := range ps {
		out.Plans = append(out.Plans, proto.Plan{Name: p.Name, From: p.From.Format("2006-01"), Fee: p.Fee, Seats: p.Seats,
			Principals: p.Principals, SetBy: p.SetBy, SetAt: p.SetAt})
	}
	return out, nil
}
