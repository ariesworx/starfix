package e2e

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"maps"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
)

// TestSoak is stage 3's gate (design §13): many agent sessions, across
// several principals, work one backlog at once through the real stack,
// under churn and failure, and every stage 3 guarantee must hold
// throughout. Sessions vanish without finishing, connections drop between
// a request being applied and its answer arriving, and the daemon
// restarts mid-run. The store's clock runs fast, so leases lapse and the
// reaper ends them many times over.
//
// A short run is part of `go test`. Set STARFIX_SOAK to a duration, such
// as 10m, for a long one, and STARFIX_SOAK_SEED to replay a run's choices;
// the seed is printed at the start and with any failure. See
// CONTRIBUTING.md, "The soak test".
func TestSoak(t *testing.T) {
	cfg, err := soakConfigFrom(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("soak: seed %d, %s of work, %d principals × %d sessions, store clock ×%g; replay with STARFIX_SOAK_SEED=%d",
		cfg.seed, cfg.duration, len(cfg.principals), cfg.sessions, cfg.speed, cfg.seed)
	s := newSoak(t, cfg)
	s.run()
	s.verify()
	s.report()
	s.explain()
}

// soakConfig shapes one run.
type soakConfig struct {
	seed       uint64
	duration   time.Duration // how long the sessions work
	principals []string
	admin      string
	// sessions per principal; the first of each is an agent running `sfx
	// mcp`, the others use the client library directly.
	sessions int
	// speed is how many times faster than real time the store's clock
	// runs.
	speed float64
	// lease is the range of leases the client library sessions ask for,
	// on the store's clock; `sfx mcp` asks for its own 15m.
	leaseMin, leaseMax time.Duration
	// vanish is the chance, each step, that a session dies without a word.
	vanish float64
	// stall is the chance, each step, that a session stops past its
	// lease and then carries on with the claims it believes it holds.
	stall float64
	// think is the most a session pauses between steps, in real time.
	think time.Duration
	// restarts is how many times the daemon restarts, evenly spread.
	restarts int
	// limits are the daemon's limits, low enough that the workload meets
	// each cap.
	limits server.Limits
	// target is the backlog of unclosed issues the workload keeps near.
	target int
}

// soakConfigFrom reads STARFIX_SOAK (a duration; empty runs the short
// soak) and STARFIX_SOAK_SEED (empty picks one).
func soakConfigFrom(getenv func(string) string) (soakConfig, error) {
	cfg := soakConfig{
		seed:     rand.Uint64(), //nolint:gosec // a seed to print and replay, not a secret
		duration: 12 * time.Second, principals: []string{"alice", "bob", "carol", "dave"},
		admin: "dave", sessions: 5, speed: 120, leaseMin: time.Minute, leaseMax: 3 * time.Minute,
		vanish: 0.004, stall: 0.004, think: 20 * time.Millisecond, restarts: 1, target: 120,
		limits: server.Limits{
			Limits: store.Limits{Labels: 6, AcceptanceItems: 5, Deps: 6, Sessions: 10, InboxUnread: 40,
				UsageRecords: 20, UsagePerDay: 4000, Paths: 12},
			WriteRate: 100, WriteBurst: 100,
		},
	}
	if v := getenv("STARFIX_SOAK"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return soakConfig{}, fmt.Errorf("STARFIX_SOAK=%q: want a duration such as 10m", v)
		}
		cfg.duration = d
		cfg.restarts = max(1, int(d/(2*time.Minute)))
	}
	if v := getenv("STARFIX_SOAK_SEED"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return soakConfig{}, fmt.Errorf("STARFIX_SOAK_SEED=%q: want an unsigned integer", v)
		}
		cfg.seed = n
	}
	return cfg, nil
}

// vclock is the store's clock: it runs speed times faster than real time
// from when the run starts, so a one-minute lease lapses in half a second.
type vclock struct {
	base  time.Time
	start time.Time // real, with its monotonic reading
	speed float64
}

func (c *vclock) now() time.Time {
	return c.base.Add(time.Duration(float64(time.Since(c.start)) * c.speed)).UTC().Truncate(time.Microsecond)
}

// real is how long d on the store's clock takes in real time.
func (c *vclock) real(d time.Duration) time.Duration { return time.Duration(float64(d) / c.speed) }

// soak is one run.
type soak struct {
	t       *testing.T
	cfg     soakConfig
	clock   *vclock
	w       *world
	db      *sql.DB // the checker's own read-only use of the store's database
	mon     *monitor
	rep     *replay
	pool    pool
	holding holding
	logs    *logCounter
	users   map[string]*user
	eve     *user // reads for the checks, and does nothing else

	mu    sync.Mutex // guards live and every
	live  []worker   // the sessions still at work when the run ended
	every []worker   // every session the run started
	stats struct {
		drops, dropAlls, restarts, stalls, rewatches     atomic.Int64
		heapPeak                                         atomic.Uint64
		goroutinePeak                                    atomic.Int64
		snapshots                                        atomic.Int64
		windowsChecked, windowsResynced                  atomic.Int64
		eventWindowsChecked, eventsChecked, eventsPushed atomic.Int64
		claimsChecked                                    atomic.Int64
	}
	baseline int // goroutines before any session started
	started  time.Time
	ended    time.Duration // real length of the work
}

func newSoak(t *testing.T, cfg soakConfig) *soak {
	clock := &vclock{base: time.Now().UTC().Truncate(time.Second), start: time.Now(), speed: cfg.speed}
	s := &soak{t: t, cfg: cfg, clock: clock, mon: newMonitor(), logs: &logCounter{}, users: map[string]*user{}}
	s.rep = newReplay(s.mon.fail, []string{cfg.admin})
	s.w = newWorld(t, daemonOpts{now: clock.now, reap: 100 * time.Millisecond, commit: 500 * time.Millisecond,
		admins: []string{cfg.admin}, limits: cfg.limits, logger: slog.New(s.logs)})
	for _, p := range cfg.principals {
		s.users[p] = s.w.newUser(p, "")
	}
	s.eve = s.w.newUser("eve", "")
	dc, err := mysql.ParseDSN(s.w.dsn)
	if err != nil {
		t.Fatal(err)
	}
	dc.ParseTime, dc.Loc = true, time.UTC
	conn, err := mysql.NewConnector(dc)
	if err != nil {
		t.Fatal(err)
	}
	s.db = sql.OpenDB(conn)
	s.db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = s.db.Close() })
	s.baseline = runtime.NumGoroutine()
	return s
}

// run starts every session, the chaos and the sampler, lets them work for
// the configured time, then brings the run to rest: sessions stop, their
// claims lapse and are reaped, and every live watch is checked.
func (s *soak) run() {
	t := s.t
	ctx, stop := context.WithTimeout(context.Background(), s.cfg.duration)
	defer stop()
	s.started = time.Now()
	var wg sync.WaitGroup
	for i, p := range s.cfg.principals {
		for slot := range s.cfg.sessions {
			seed := s.cfg.seed + uint64(i*100+slot)
			wg.Go(func() { s.slot(ctx, p, slot, seed) })
		}
	}
	var side sync.WaitGroup
	side.Go(func() { s.chaos(ctx) })
	side.Go(func() { s.sample(ctx) })
	wg.Wait()
	side.Wait()
	s.ended = time.Since(s.started)
	s.w.dropReplyTo("")

	settle, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, a := range s.agents() {
		a.settle(settle)
	}
	s.checkClaimsOp(settle)
	s.awaitReaped()
	for _, a := range s.agents() {
		if ln, c := a.liveLine(); ln != nil {
			s.awaitWindow(a.base(), ln, c, bound{})
		}
	}
	if err := s.snapshot(); err != nil {
		t.Errorf("final snapshot: %v", err)
	}
}

// slot keeps one session of principal p at work: when it vanishes, a new
// session, with a new id, takes its place.
func (s *soak) slot(ctx context.Context, p string, slot int, seed uint64) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // a reproducible workload, not security
	for gen := 0; ctx.Err() == nil; gen++ {
		tag := fmt.Sprintf("%s-%dg%d", p, slot, gen)
		sub := rand.New(rand.NewPCG(rng.Uint64(), rng.Uint64())) //nolint:gosec // a reproducible workload, not security
		base := &session{s: s, tag: tag, u: s.users[p], rng: sub,
			lease: s.cfg.leaseMin + time.Duration(rng.Int64N(int64(s.cfg.leaseMax-s.cfg.leaseMin)+1)).Truncate(time.Minute)}
		var a interface {
			worker
			run(context.Context) bool
		}
		if slot == 0 {
			base.key = sessKey{p, "mcp-" + tag}
			base.lease = 15 * time.Minute
			base.renewEvery = s.clock.real(base.lease) / 8
			ms := &mcpSession{session: base, held: map[string]int{}}
			if err := ms.open(); err != nil {
				s.mon.fail("", "%s: start sfx mcp: %v", base.key, err)
				return
			}
			a = ms
		} else {
			base.key = sessKey{p, "lib-" + tag}
			base.renewEvery = s.clock.real(base.lease) / 4
			base.events = slot == 1
			a = &libSession{session: base, held: map[string]heldIssue{}, revs: map[string]int64{}}
		}
		s.mu.Lock()
		s.every = append(s.every, a)
		s.mu.Unlock()
		if a.run(ctx) {
			s.mu.Lock()
			s.live = append(s.live, a)
			s.mu.Unlock()
			return
		}
	}
}

func (s *soak) agents() []worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.live)
}

// createWeight steers creates so the backlog stays near its target.
func (s *soak) createWeight() int {
	open := 0
	s.rep.mu.Lock()
	open = s.rep.openCount
	s.rep.mu.Unlock()
	switch {
	case open < s.cfg.target/2:
		return 16
	case open < s.cfg.target:
		return 8
	}
	return 1
}

// chaos drops answers after the server applied the request, drops every
// connection now and then, and restarts the daemon at even intervals.
func (s *soak) chaos(ctx context.Context) {
	rng := rand.New(rand.NewPCG(s.cfg.seed, 7)) //nolint:gosec // reproducible chaos, not security
	ops := []string{proto.OpCreate, proto.OpComment, proto.OpFinish, proto.OpHandoff, proto.OpStart, proto.OpRenew,
		proto.OpUsage, proto.OpUpdate, proto.OpLabelAdd, proto.OpDepAdd, proto.OpAccept, proto.OpClose,
		proto.OpWatch, proto.OpInbox, proto.OpAck, proto.OpReady, proto.OpShow}
	every := s.cfg.duration / time.Duration(s.cfg.restarts+1)
	nextRestart := time.Now().Add(every)
	nextDropAll := time.Now().Add(time.Duration(1000+rng.IntN(2000)) * time.Millisecond)
	for ctx.Err() == nil {
		pause(ctx, time.Duration(100+rng.IntN(300))*time.Millisecond)
		if ctx.Err() != nil {
			return
		}
		s.w.dropReplyTo(ops[rng.IntN(len(ops))])
		s.stats.drops.Add(1)
		if time.Now().After(nextDropAll) {
			s.w.dropAll()
			s.stats.dropAlls.Add(1)
			nextDropAll = time.Now().Add(time.Duration(1000+rng.IntN(2000)) * time.Millisecond)
		}
		if int(s.stats.restarts.Load()) < s.cfg.restarts && time.Now().After(nextRestart) {
			if err := s.w.restartDaemon(); err != nil {
				s.mon.fail("", "restart the daemon: %v", err)
				return
			}
			s.stats.restarts.Add(1)
			nextRestart = time.Now().Add(every)
		}
	}
}

// sample watches memory and goroutines, and every few seconds compares
// the replayed event log with the tables in one snapshot.
func (s *soak) sample(ctx context.Context) {
	last := time.Now()
	for ctx.Err() == nil {
		pause(ctx, 500*time.Millisecond)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > s.stats.heapPeak.Load() {
			s.stats.heapPeak.Store(ms.HeapAlloc)
		}
		if n := int64(runtime.NumGoroutine()); n > s.stats.goroutinePeak.Load() {
			s.stats.goroutinePeak.Store(n)
		}
		if time.Since(last) >= 3*time.Second && ctx.Err() == nil {
			last = time.Now()
			if err := s.snapshot(); err != nil {
				s.t.Logf("soak: snapshot: %v", err)
			}
		}
	}
}

// logCounter is a slog handler that counts the daemon's warnings and
// errors by line, for the report.
type logCounter struct {
	mu    sync.Mutex
	n     int
	lines map[string]int
}

func (l *logCounter) Enabled(_ context.Context, lv slog.Level) bool { return lv >= slog.LevelWarn }
func (l *logCounter) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n++
	if l.lines == nil {
		l.lines = map[string]int{}
	}
	l.lines[b.String()]++
	return nil
}

func (l *logCounter) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n
}

// top returns the most frequent lines, at most n, with their counts.
func (l *logCounter) top(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := slices.SortedFunc(maps.Keys(l.lines), func(a, b string) int {
		return cmp.Or(cmp.Compare(l.lines[b], l.lines[a]), cmp.Compare(a, b))
	})
	var out []string
	for _, k := range keys[:min(n, len(keys))] {
		out = append(out, fmt.Sprintf("%5d× %s", l.lines[k], k))
	}
	return out
}
func (l *logCounter) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logCounter) WithGroup(string) slog.Handler      { return l }

// report logs throughput and latency per op, and what the run did. They
// are reported, not asserted: only a hang fails the run.
func (s *soak) report() {
	calls := s.mon.calls()
	type opStat struct {
		n, acked, refused, lost int
		took                    []time.Duration
	}
	byOp := map[string]*opStat{}
	for _, c := range calls {
		st := byOp[c.op]
		if st == nil {
			st = &opStat{}
			byOp[c.op] = st
		}
		st.n++
		st.took = append(st.took, c.took)
		switch c.out {
		case acked:
			st.acked++
		case refused:
			st.refused++
		case lost:
			st.lost++
		}
	}
	var b bytes.Buffer
	secs := s.ended.Seconds()
	fmt.Fprintf(&b, "soak: seed %d: %d requests in %.1fs (%.0f/s)\n", s.cfg.seed, len(calls), secs, float64(len(calls))/secs)
	fmt.Fprintf(&b, "  %-10s %7s %7s %7s %6s %8s %8s %8s\n", "op", "count", "acked", "refused", "lost", "p50", "p99", "max")
	for _, op := range slices.Sorted(maps.Keys(byOp)) {
		st := byOp[op]
		slices.Sort(st.took)
		q := func(f float64) time.Duration { return st.took[min(len(st.took)-1, int(f*float64(len(st.took))))] }
		fmt.Fprintf(&b, "  %-10s %7d %7d %7d %6d %8s %8s %8s\n", op, st.n, st.acked, st.refused, st.lost,
			q(0.5).Round(100*time.Microsecond), q(0.99).Round(100*time.Microsecond), st.took[len(st.took)-1].Round(time.Millisecond))
	}
	codes := map[string]int{}
	for _, c := range calls {
		if c.out == refused && c.err != nil {
			codes[c.op+" "+string(c.err.Code)]++
		}
	}
	b.WriteString("  refusals:")
	for _, k := range slices.Sorted(maps.Keys(codes)) {
		fmt.Fprintf(&b, " %s %d;", k, codes[k])
	}
	b.WriteString("\n")
	r := s.rep
	r.mu.Lock()
	fmt.Fprintf(&b, "  events %d in %d transactions; issues %d; claims taken %d, reaped %d; takeovers of a live claim %d; handoffs that may have released a claim unlogged %d, did %d\n",
		r.seq, r.txs, len(r.issues), r.ops["claim.take"], r.ops["claim.expire"], len(r.overs), r.ambiguous, r.unlogged)
	r.mu.Unlock()
	m := s.mon
	m.mu.Lock()
	fmt.Fprintf(&b, "  chaos: %d dropped answers, %d connection drops, %d daemon restarts; %d sessions vanished, %d stalled; %d dial failures; retries %v\n",
		s.stats.drops.Load(), s.stats.dropAlls.Load(), s.stats.restarts.Load(), m.vanishes, s.stats.stalls.Load(), m.dialFails, m.retries)
	m.mu.Unlock()
	fmt.Fprintf(&b, "  watches checked %d (%d for events, %d events owed; %d events pushed in all), %d ended by a resync, %d watched again; claims checked %d; snapshots compared %d; peak heap %d MiB, peak goroutines %d; daemon warnings %d\n",
		s.stats.windowsChecked.Load(), s.stats.eventWindowsChecked.Load(), s.stats.eventsChecked.Load(), s.stats.eventsPushed.Load(), s.stats.windowsResynced.Load(),
		s.stats.rewatches.Load(), s.stats.claimsChecked.Load(), s.stats.snapshots.Load(), s.stats.heapPeak.Load()>>20, s.stats.goroutinePeak.Load(), s.logs.count())
	for _, l := range s.logs.top(12) {
		fmt.Fprintf(&b, "    %s\n", l)
	}
	s.t.Log(b.String())
}
