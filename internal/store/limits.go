package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Limits bound what one request, or one principal, can make the store
// hold (S-4, S-6, S-7). Without them a single create with 20,000 labels
// would hold the one writer for 17 s, and a principal could grow the
// registry and another's inbox without end. Zero fields take
// [DefaultLimits]. A request past a limit is refused with ErrInvalid,
// except UsagePerDay, refused with ErrBusy, and Sessions, InboxUnread
// and Notices, which drop rows or notices as their fields say.
type Limits struct {
	// Labels caps the labels on one issue, and so in one create.
	Labels int `yaml:"labels_per_issue"`
	// AcceptanceItems caps the items an issue's acceptance text may hold,
	// and the item numbers one accept or finish may name.
	AcceptanceItems int `yaml:"acceptance_items"`
	// Deps caps the edges out of one issue.
	Deps int `yaml:"deps_per_issue"`
	// Sessions caps one principal's rows in the agents registry; a new
	// session past it drops the least recently seen.
	Sessions int `yaml:"sessions_per_principal"`
	// InboxUnread caps one principal's unread items; past it the oldest
	// are marked read, so they stay under `inbox --all` until purged.
	InboxUnread int `yaml:"inbox_unread"`
	// Notices caps the mentions, assignments and handoffs one principal
	// can send another in a minute; the rest are not delivered. Lost
	// claims are not counted.
	Notices int `yaml:"notices_per_minute"`
	// UsageRecords caps the records one AddUsage call may send.
	UsageRecords int `yaml:"usage_records"`
	// UsagePerDay caps the usage records one principal may add in 24
	// hours, counted by when the server stored them. Per principal, not
	// per session: sessions are the client's to name, so a cap per
	// session would bound nothing.
	UsagePerDay int `yaml:"usage_per_day"`
	// Paths caps the paths one issue keeps, declared and from commits;
	// declared paths past it are refused, and commit paths past it, less
	// the declared ones, drop the least recently recorded. It also bounds
	// the commit paths one request records.
	Paths int `yaml:"paths_per_issue"`
	// MemoryBody caps a memory's body, in bytes; at most 65,535.
	MemoryBody int `yaml:"memory_body"`
	// MemoryTags caps the distinct tags on one memory.
	MemoryTags int `yaml:"memory_tags"`
	// MemoryTagLength caps one tag, in bytes; at most 255.
	MemoryTagLength int `yaml:"memory_tag_length"`
	// Memories caps the memories one principal has created in one scope;
	// replacing one of them is no new memory.
	Memories int `yaml:"memories_per_scope"`
	// MemoryKeyLength caps a memory's key, in bytes; at most 255.
	MemoryKeyLength int `yaml:"memory_key_length"`
	// Prices caps the rows of the prices table: a model's rates from one
	// effective time are one row, and replacing them is no new row; at
	// most 10,000.
	Prices int `yaml:"prices"`
	// HoursPerDay caps one principal's hours entries on one day.
	HoursPerDay int `yaml:"hours_per_day"`
	// HoursNote caps an hours entry's note, in bytes; at most 65,535.
	HoursNote int `yaml:"hours_note"`
	// Plans caps the rows of the plans table: a plan's terms from one
	// month are one row, and replacing them is no new row; at most
	// 10,000.
	Plans int `yaml:"plans"`
	// PlanPrincipals caps the principals one plan row names; at most
	// 1,000.
	PlanPrincipals int `yaml:"plan_principals"`
}

// DefaultLimits are the limits a zero field takes.
var DefaultLimits = Limits{Labels: 50, AcceptanceItems: 200, Deps: 200, Sessions: 256, InboxUnread: 1000, Notices: 10,
	UsageRecords: 500, UsagePerDay: 50000, Paths: proto.MaxPaths,
	MemoryBody: 4096, MemoryTags: 20, MemoryTagLength: 64, Memories: 1000, MemoryKeyLength: 128,
	Prices: 1000, HoursPerDay: 50, HoursNote: 500, Plans: 1000, PlanPrincipals: 100}

// Column sizes that bound the memory limits: a body is TEXT, and a key
// and a tag VARCHAR(255).
const (
	maxMemoryBody = maxText
	maxMemoryName = 255
)

// maxPrices bounds the prices limit: every cost report, show and digest
// reads the whole prices table into memory.
const maxPrices = 10000

// maxPlans and maxPlanPrincipals bound the plans limits: every cost
// report reads every plan and its principals into memory.
const (
	maxPlans          = 10000
	maxPlanPrincipals = 1000
)

// withDefaults returns l with each zero field set from DefaultLimits.
func (l Limits) withDefaults() Limits {
	for _, f := range []struct{ v, d *int }{
		{&l.Labels, &DefaultLimits.Labels}, {&l.AcceptanceItems, &DefaultLimits.AcceptanceItems},
		{&l.Deps, &DefaultLimits.Deps}, {&l.Sessions, &DefaultLimits.Sessions},
		{&l.InboxUnread, &DefaultLimits.InboxUnread}, {&l.Notices, &DefaultLimits.Notices},
		{&l.UsageRecords, &DefaultLimits.UsageRecords}, {&l.UsagePerDay, &DefaultLimits.UsagePerDay},
		{&l.Paths, &DefaultLimits.Paths}, {&l.MemoryBody, &DefaultLimits.MemoryBody},
		{&l.MemoryTags, &DefaultLimits.MemoryTags}, {&l.MemoryTagLength, &DefaultLimits.MemoryTagLength},
		{&l.Memories, &DefaultLimits.Memories}, {&l.MemoryKeyLength, &DefaultLimits.MemoryKeyLength},
		{&l.Prices, &DefaultLimits.Prices}, {&l.HoursPerDay, &DefaultLimits.HoursPerDay},
		{&l.HoursNote, &DefaultLimits.HoursNote}, {&l.Plans, &DefaultLimits.Plans},
		{&l.PlanPrincipals, &DefaultLimits.PlanPrincipals},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	return l
}

// Validate refuses, with ErrInvalid, a negative limit, and a memory limit
// larger than its column holds. Zero is valid: it means the default.
func (l Limits) Validate() error {
	for _, f := range []struct {
		name string
		v    int
	}{
		{"labels_per_issue", l.Labels}, {"acceptance_items", l.AcceptanceItems}, {"deps_per_issue", l.Deps},
		{"sessions_per_principal", l.Sessions}, {"inbox_unread", l.InboxUnread}, {"notices_per_minute", l.Notices},
		{"usage_records", l.UsageRecords}, {"usage_per_day", l.UsagePerDay}, {"paths_per_issue", l.Paths},
		{"memory_body", l.MemoryBody}, {"memory_tags", l.MemoryTags}, {"memory_tag_length", l.MemoryTagLength},
		{"memories_per_scope", l.Memories}, {"memory_key_length", l.MemoryKeyLength}, {"prices", l.Prices},
		{"hours_per_day", l.HoursPerDay}, {"hours_note", l.HoursNote}, {"plans", l.Plans},
		{"plan_principals", l.PlanPrincipals},
	} {
		if f.v < 0 {
			return fmt.Errorf("%w: limit %s is %d; give a positive number, or leave it out for the default", ErrInvalid, f.name, f.v)
		}
	}
	for _, f := range []struct {
		name    string
		v, most int
	}{
		{"memory_body", l.MemoryBody, maxMemoryBody}, {"memory_tag_length", l.MemoryTagLength, maxMemoryName},
		{"memory_key_length", l.MemoryKeyLength, maxMemoryName}, {"prices", l.Prices, maxPrices},
		{"hours_note", l.HoursNote, maxText}, {"plans", l.Plans, maxPlans},
		{"plan_principals", l.PlanPrincipals, maxPlanPrincipals},
	} {
		if f.v > f.most {
			return fmt.Errorf("%w: limit %s is %d; it can be at most %d", ErrInvalid, f.name, f.v, f.most)
		}
	}
	return nil
}

// Limits returns the limits in force.
func (s *Store) Limits() Limits { return s.opts.Limits }

// Pruned counts what one Prune removed.
type Pruned struct {
	Agents int64
	Inbox  int64
}

// pruneBatch bounds the rows one Prune deletes from each table, so it
// never holds the writer long; the next run takes the rest.
const pruneBatch = 1000

// Prune deletes registry rows not seen within agentKeep, except each
// principal's most recent row (which keeps the principal known to
// mentions), and inbox items read more than inboxKeep ago, at most 1,000
// of each per call. Neither is history, so it records no event. A window
// that is not positive is refused with ErrInvalid. The server's reaper
// runs it.
func (s *Store) Prune(ctx context.Context, agentKeep, inboxKeep time.Duration) (Pruned, error) {
	if agentKeep <= 0 || inboxKeep <= 0 {
		return Pruned{}, fmt.Errorf("%w: prune windows must be positive", ErrInvalid)
	}
	var out Pruned
	err := s.write(ctx, ReaperActor, func(w *wtx) error {
		out = Pruned{}
		w.quiet = true
		rows, err := w.tx.QueryContext(ctx, `SELECT a.principal, a.session FROM agents a
  JOIN (SELECT principal, MAX(last_seen) AS latest FROM agents GROUP BY principal) m ON m.principal = a.principal
  WHERE a.last_seen < ? AND a.last_seen < m.latest
  ORDER BY a.last_seen LIMIT ?`, w.now.Add(-agentKeep), pruneBatch)
		if err != nil {
			return fmt.Errorf("prune agents: %w", err)
		}
		var stale [][2]string
		for rows.Next() {
			var p, sess string
			if err := rows.Scan(&p, &sess); err != nil {
				_ = rows.Close()
				return fmt.Errorf("prune agents: %w", err)
			}
			stale = append(stale, [2]string{p, sess})
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("prune agents: %w", err)
		}
		for _, k := range stale {
			n, err := w.exec(ctx, `DELETE FROM agents WHERE principal = ? AND session = ?`, k[0], k[1])
			if err != nil {
				return fmt.Errorf("prune agents: %w", err)
			}
			out.Agents += n
		}
		n, err := w.exec(ctx, `DELETE FROM inbox WHERE read_at IS NOT NULL AND read_at < ? ORDER BY id LIMIT ?`,
			w.now.Add(-inboxKeep), pruneBatch)
		if err != nil {
			return fmt.Errorf("prune inbox: %w", err)
		}
		out.Inbox = n
		return nil
	})
	return out, err
}
