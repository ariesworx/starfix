package server

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

// Limits bound what one principal can make the daemon do: how many
// connections it holds, how long they idle, how fast it writes, and how
// many log lines its refusals cost (S-11, S-20, the write rate limit);
// the store's limits on what a request may hold come with them (S-4,
// S-6, S-7). They are set under limits: in starfixd's config file; a
// field left out, or zero, takes DefaultLimits.
type Limits struct {
	store.Limits `yaml:",inline"`

	// Conns caps the daemon's connections past the handshake; twice it
	// caps sockets still in the handshake.
	Conns int `yaml:"conns"`
	// ConnsPerPrincipal caps one principal's connections.
	ConnsPerPrincipal int `yaml:"conns_per_principal"`
	// IdleTimeout closes a connection that sends nothing for this long,
	// unless it watches its inbox.
	IdleTimeout Duration `yaml:"idle_timeout"`
	// WriteRate is how many writes a second a principal's token bucket
	// refills; WriteBurst how many it holds.
	WriteRate  float64 `yaml:"write_rate"`
	WriteBurst int     `yaml:"write_burst"`
	// RefusalLogs caps the refusal lines logged per principal a minute;
	// the rest are counted in one line when the minute ends.
	RefusalLogs int `yaml:"refusal_logs"`
	// AgentKeep is how long a registry row not seen is kept (a
	// principal's latest is always kept); InboxKeep how long a read inbox
	// item is.
	AgentKeep Duration `yaml:"agent_keep"`
	InboxKeep Duration `yaml:"inbox_keep"`
}

// DefaultLimits are the limits a zero field takes.
var DefaultLimits = Limits{
	Limits: store.DefaultLimits, Conns: 1024, ConnsPerPrincipal: 32, IdleTimeout: Duration(10 * time.Minute),
	WriteRate: 10, WriteBurst: 100, RefusalLogs: 20,
	AgentKeep: Duration(store.MaxAgentWindow), InboxKeep: Duration(30 * 24 * time.Hour),
}

// Duration is a YAML duration such as 90s, 10m, 8h or 7d.
type Duration time.Duration

// UnmarshalText reads a duration in proto.ParseDuration's form.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := proto.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("%q: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

// WithDefaults fills each zero field from DefaultLimits.
func (l Limits) WithDefaults() Limits {
	d := DefaultLimits
	for _, f := range []struct{ v, d *int }{
		{&l.Labels, &d.Labels}, {&l.AcceptanceItems, &d.AcceptanceItems}, {&l.Deps, &d.Deps},
		{&l.Sessions, &d.Sessions}, {&l.InboxUnread, &d.InboxUnread}, {&l.Notices, &d.Notices},
		{&l.UsageRecords, &d.UsageRecords}, {&l.UsagePerDay, &d.UsagePerDay}, {&l.Paths, &d.Paths},
		{&l.Conns, &d.Conns}, {&l.ConnsPerPrincipal, &d.ConnsPerPrincipal}, {&l.WriteBurst, &d.WriteBurst},
		{&l.RefusalLogs, &d.RefusalLogs},
	} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	for _, f := range []struct{ v, d *Duration }{{&l.IdleTimeout, &d.IdleTimeout}, {&l.AgentKeep, &d.AgentKeep}, {&l.InboxKeep, &d.InboxKeep}} {
		if *f.v == 0 {
			*f.v = *f.d
		}
	}
	if l.WriteRate == 0 {
		l.WriteRate = d.WriteRate
	}
	return l
}

// Validate refuses a negative limit, or a write rate that is NaN or
// infinite. Zero is valid: it takes the default.
func (l Limits) Validate() error {
	if err := l.Limits.Validate(); err != nil {
		return fmt.Errorf("%w; fix: correct limits: in the config file", err)
	}
	for _, f := range []struct {
		name string
		bad  bool
	}{
		{"conns", l.Conns < 0}, {"conns_per_principal", l.ConnsPerPrincipal < 0}, {"idle_timeout", l.IdleTimeout < 0},
		{"write_rate", l.WriteRate < 0 || math.IsNaN(l.WriteRate) || math.IsInf(l.WriteRate, 0)},
		{"write_burst", l.WriteBurst < 0}, {"refusal_logs", l.RefusalLogs < 0},
		{"agent_keep", l.AgentKeep < 0}, {"inbox_keep", l.InboxKeep < 0},
	} {
		if f.bad {
			return fmt.Errorf("limit %s must be zero or a positive number; fix: correct limits: in the config file, or leave it out for the default", f.name)
		}
	}
	return nil
}

// readOps are the operations that write nothing; every other op counts
// against the principal's write bucket.
var readOps = map[string]bool{
	proto.OpShow: true, proto.OpList: true, proto.OpReady: true, proto.OpBlocked: true, proto.OpComments: true,
	proto.OpHistory: true, proto.OpDigest: true, proto.OpWho: true, proto.OpInbox: true, proto.OpWatch: true,
}

// buckets is a token bucket per principal: each holds up to burst tokens,
// refilled at rate a second, and a write takes one.
type buckets struct {
	mu    sync.Mutex
	rate  float64
	burst float64
	m     map[string]*bucket // by principal, guarded by mu
}

// bucket is one principal's tokens, as of at.
type bucket struct {
	tokens float64
	at     time.Time
}

// take takes a token from key's bucket at now, or reports how long until
// one is there.
func (b *buckets) take(key string, now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]*bucket{}
	}
	k := b.m[key]
	if k == nil {
		k = &bucket{tokens: b.burst, at: now}
		b.m[key] = k
	}
	if el := now.Sub(k.at).Seconds(); el > 0 {
		k.tokens = math.Min(b.burst, k.tokens+el*b.rate)
	}
	k.at = now
	if k.tokens >= 1 {
		k.tokens--
		return true, 0
	}
	return false, time.Duration(math.Ceil((1-k.tokens)/b.rate)) * time.Second
}

// conns counts connections past the handshake, in all and per principal.
type conns struct {
	mu    sync.Mutex // guards total and by
	total int
	by    map[string]int
}

// acquire takes a slot for principal, or says which cap is full.
func (c *conns) acquire(principal string, total, per int) (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.total >= total:
		return false, fmt.Sprintf("the server is at its connection limit (%d)", total)
	case c.by[principal] >= per:
		return false, fmt.Sprintf("%s is at its connection limit (%d)", principal, per)
	}
	if c.by == nil {
		c.by = map[string]int{}
	}
	c.total++
	c.by[principal]++
	return true, ""
}

// release frees a slot acquire took for principal.
func (c *conns) release(principal string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total--
	if c.by[principal]--; c.by[principal] <= 0 {
		delete(c.by, principal)
	}
}

// sampler logs at most n refusal lines per principal a minute (S-20), so
// a principal looping refused requests cannot flood the journal; when a
// minute with suppressed lines ends, its next line says how many.
type sampler struct {
	mu sync.Mutex
	n  int
	m  map[string]*window // by principal ("" before one is known), guarded by mu
}

// window counts one principal's refusal lines in the minute from start.
type window struct {
	start      time.Time
	logged     int
	suppressed int
}

// log writes the line through log unless key is over its allowance.
func (s *sampler) log(log *slog.Logger, key string, now time.Time, level slog.Level, msg string, attrs ...any) {
	s.mu.Lock()
	if s.m == nil {
		s.m = map[string]*window{}
	}
	w := s.m[key]
	if w == nil || now.Sub(w.start) >= time.Minute || now.Before(w.start) {
		if w != nil && w.suppressed > 0 {
			log.Warn("refusal lines suppressed", "key", key, "lines", w.suppressed, "per_minute", s.n)
		}
		w = &window{start: now}
		s.m[key] = w
		if len(s.m) > 4096 { // forget idle principals
			for k, v := range s.m {
				if now.Sub(v.start) >= time.Minute {
					delete(s.m, k)
				}
			}
			s.m[key] = w
		}
	}
	ok := w.logged < s.n
	if ok {
		w.logged++
	} else {
		w.suppressed++
	}
	s.mu.Unlock()
	if ok {
		log.Log(context.Background(), level, msg, attrs...)
	}
}
