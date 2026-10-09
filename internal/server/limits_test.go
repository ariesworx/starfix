package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

func TestResolveSettingsLimits(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		body string
		want func(Limits) bool
		err  string
	}{
		{name: "defaults", body: "", want: func(l Limits) bool { return l == DefaultLimits }},
		{name: "set", body: "limits:\n  labels_per_issue: 9\n  conns_per_principal: 4\n  idle_timeout: 90s\n  write_rate: 2.5\n  agent_keep: 14d\n",
			want: func(l Limits) bool {
				return l.Labels == 9 && l.ConnsPerPrincipal == 4 && l.IdleTimeout == Duration(90*time.Second) &&
					l.WriteRate == 2.5 && l.AgentKeep == Duration(14*24*time.Hour) && l.Conns == DefaultLimits.Conns
			}},
		{name: "zero takes the default", body: "limits:\n  labels_per_issue: 0\n  write_burst: 0\n  write_rate: 0\n",
			want: func(l Limits) bool { return l == DefaultLimits }},
		{name: "usage limits", body: "limits:\n  usage_records: 50\n  usage_per_day: 900\n",
			want: func(l Limits) bool {
				return l.UsageRecords == 50 && l.UsagePerDay == 900 && l.Labels == DefaultLimits.Labels
			}},
		{name: "negative store limit", body: "limits:\n  acceptance_items: -1\n", err: "acceptance_items"},
		{name: "negative usage limit", body: "limits:\n  usage_per_day: -1\n", err: "usage_per_day"},
		{name: "paths limit", body: "limits:\n  paths_per_issue: 50\n",
			want: func(l Limits) bool { return l.Paths == 50 && l.Labels == DefaultLimits.Labels }},
		{name: "negative paths limit", body: "limits:\n  paths_per_issue: -1\n", err: "paths_per_issue"},
		{name: "memory limits", body: "limits:\n  memory_body: 2048\n  memory_tags: 5\n  memory_tag_length: 32\n  memories_per_scope: 50\n  memory_key_length: 64\n",
			want: func(l Limits) bool {
				return l.MemoryBody == 2048 && l.MemoryTags == 5 && l.MemoryTagLength == 32 && l.Memories == 50 &&
					l.MemoryKeyLength == 64 && l.Labels == DefaultLimits.Labels
			}},
		{name: "memory limits default", body: "limits:\n  memory_body: 0\n",
			want: func(l Limits) bool {
				return l.MemoryBody == 4096 && l.MemoryTags == 20 && l.MemoryTagLength == 64 && l.Memories == 1000 && l.MemoryKeyLength == 128
			}},
		{name: "negative memory limit", body: "limits:\n  memories_per_scope: -1\n", err: "memories_per_scope"},
		{name: "memory body past the column", body: "limits:\n  memory_body: 70000\n", err: "memory_body"},
		{name: "negative server limit", body: "limits:\n  write_burst: -1\n", err: "limit write_burst must be zero or a positive number"},
		{name: "write rate not a number", body: "limits:\n  write_rate: .nan\n", err: "limit write_rate must be zero or a positive number"},
		{name: "infinite write rate", body: "limits:\n  write_rate: .inf\n", err: "limit write_rate must be zero or a positive number"},
		{name: "bad duration", body: "limits:\n  idle_timeout: soon\n", err: "soon"},
		{name: "unknown limit", body: "limits:\n  lables: 3\n", err: "lables"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := ResolveSettings(Settings{}, write(tc.name+".yaml", tc.body), func(string) string { return "" })
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ResolveSettings(%q) = %v, want an error naming %q", tc.body, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.want(s.Limits) {
				t.Errorf("ResolveSettings(%q).Limits = %+v", tc.body, s.Limits)
			}
		})
	}
}

func TestBuckets(t *testing.T) {
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	b := buckets{rate: 2, burst: 3}
	steps := []struct {
		at   time.Duration
		key  string
		ok   bool
		wait time.Duration
	}{
		{0, "alice", true, 0}, {0, "alice", true, 0}, {0, "alice", true, 0},
		{0, "alice", false, time.Second},
		{0, "bob", true, 0},
		{500 * time.Millisecond, "alice", true, 0},
		{500 * time.Millisecond, "alice", false, time.Second},
		{time.Hour, "alice", true, 0}, {time.Hour, "alice", true, 0}, {time.Hour, "alice", true, 0},
		{time.Hour, "alice", false, time.Second},
	}
	for i, st := range steps {
		ok, wait := b.take(st.key, t0.Add(st.at))
		if ok != st.ok || wait != st.wait {
			t.Errorf("step %d: take(%s, +%s) = %v, %s; want %v, %s", i, st.key, st.at, ok, wait, st.ok, st.wait)
		}
	}
}

func TestSampler(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	t0 := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := sampler{n: 2}
	for i := range 5 {
		s.log(log, "mallory", t0.Add(time.Duration(i)*time.Second), slog.LevelInfo, "request", "i", i)
	}
	s.log(log, "alice", t0, slog.LevelInfo, "request", "i", 9)
	s.log(log, "mallory", t0.Add(time.Minute), slog.LevelInfo, "request", "i", 5)
	got := buf.String()
	for _, w := range []string{"i=0", "i=1", "i=9", "lines=3", "i=5"} {
		if !strings.Contains(got, w) {
			t.Errorf("log lacks %q:\n%s", w, got)
		}
	}
	for _, w := range []string{"i=2", "i=3", "i=4"} {
		if strings.Contains(got, w) {
			t.Errorf("log has suppressed line %q:\n%s", w, got)
		}
	}
}

// The reproducers of the review (S-4, S-5, S-6, S-7), as regressions.

func TestSecWriterHold(t *testing.T) {
	s := newServer(t)
	labels := make([]string, 20000)
	for i := range labels {
		labels[i] = fmt.Sprintf("l%05d", i)
	}
	start := time.Now()
	_, perr := call[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "x", Labels: labels})
	if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "at most 50 labels") {
		t.Errorf("create with 20000 labels = %v, want invalid naming the cap of 50", perr)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("the refusal took %s; it should not touch the writer", d)
	}
	var b strings.Builder
	for i := range 201 {
		fmt.Fprintf(&b, "- i%d\n", i)
	}
	if _, perr := call[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "y", Acceptance: b.String()}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("create with 201 acceptance items = %v, want invalid", perr)
	}
	c := mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "z", Acceptance: "- one"})
	ticks := make([]int, 6000)
	for i := range ticks {
		ticks[i] = i + 1
	}
	if _, perr := call[proto.AcceptResult](t, s, bob, proto.OpAccept, proto.AcceptArgs{ID: c.ID, Tick: ticks}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("tick 6000 items = %v, want invalid", perr)
	}
}

// encodes checks that res fits one frame.
func encodes(t *testing.T, s *Server, a store.Actor, op string, args any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, perr := s.Dispatch(t.Context(), a, op, raw)
	if perr != nil {
		t.Fatalf("%s: %v", op, perr)
	}
	f, err := proto.Response(1, res, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.NewEncoder(&bytes.Buffer{}).Encode(f); err != nil {
		t.Fatalf("%s reply does not fit a frame: %v", op, err)
	}
	return f.OK
}

func TestSecSizes(t *testing.T) {
	s := newServerWith(t, Limits{Limits: store.Limits{Notices: 1000}})
	c := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "x"})
	big := strings.Repeat("A", 65535)
	u := mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: c.ID, Rev: c.Rev, Body: &big, Design: &big, Notes: &big})
	huge := strings.Repeat("C", 200000)
	if _, perr := call[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: c.ID, Rev: u.Rev, Body: &huge}); perr == nil || perr.Code != proto.CodeInvalid {
		t.Errorf("200 KB body = %v, want invalid", perr)
	}
	body := strings.Repeat("D", 65535)
	for i := range 70 {
		if _, perr := call[proto.CommentResult](t, s, bob, proto.OpComment, proto.CommentArgs{ID: c.ID, Body: body}); perr != nil {
			t.Fatalf("comment %d: %v", i, perr)
		}
	}
	n, before := 0, ""
	for range 10 {
		var r proto.CommentsResult
		if err := json.Unmarshal(encodes(t, s, alice, proto.OpComments, proto.PageArgs{ID: c.ID, Before: before}), &r); err != nil {
			t.Fatal(err)
		}
		n += len(r.Comments)
		if before = r.Earlier; before == "" {
			break
		}
	}
	if n != 70 {
		t.Errorf("paging comments read %d, want 70", n)
	}
	var h proto.HistoryResult
	if err := json.Unmarshal(encodes(t, s, alice, proto.OpHistory, proto.PageArgs{ID: c.ID}), &h); err != nil {
		t.Fatal(err)
	}
	if h.Total != 72 || len(h.Events) != 72 {
		t.Errorf("history = %d of %d events, want all 72 in one page now that events are compact", len(h.Events), h.Total)
	}
}

func TestSecRegistry(t *testing.T) {
	s := newServer(t)
	for i := range 300 {
		a := store.Actor{Principal: "mallory", Session: fmt.Sprintf("%04d", i) + strings.Repeat("s", 251), Machine: strings.Repeat("m", 255)}
		if err := s.cfg.Store.TouchAgent(t.Context(), a, "claude-code"); err != nil {
			t.Fatal(err)
		}
	}
	var w proto.WhoResult
	if err := json.Unmarshal(encodes(t, s, alice, proto.OpWho, proto.WhoArgs{Since: "7d"}), &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Agents) != 100 || w.More != 156 {
		t.Errorf("who = %d agents and %d more, want 100 and 156 (256 sessions kept)", len(w.Agents), w.More)
	}
}

func TestSecMentions(t *testing.T) {
	s := newServer(t)
	for _, p := range []string{"v0", "v1", "v2", "v3", "v4", "v5", "v6", "v7", "v8", "v9"} {
		if err := s.cfg.Store.TouchAgent(t.Context(), store.Actor{Principal: p, Session: "x", Machine: "m"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	c := mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "x"})
	for range 50 {
		mustCall[proto.CommentResult](t, s, bob, proto.OpComment, proto.CommentArgs{ID: c.ID, Body: "@v0 @v1 @v2 @v3 @v4 @v5 @v6 @v7 @v8 @v9"})
	}
	for range 50 {
		mustCall[proto.CreateResult](t, s, bob, proto.OpCreate, proto.CreateArgs{Title: "spam", Assignee: "v0"})
	}
	pg, err := s.cfg.Store.Inbox(t.Context(), store.Actor{Principal: "v0", Session: "x", Machine: "m"}, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := store.DefaultLimits.Notices; pg.Unread != want {
		t.Errorf("v0 unread after 100 requests from bob = %d, want %d", pg.Unread, want)
	}
}

// S-5: show and blocked cap the edges they list and count the rest.
func TestShowAndBlockedCapEdges(t *testing.T) {
	s := newServerWith(t, Limits{Limits: store.Limits{Deps: 1000}})
	hub := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "hub"})
	for range proto.MaxShowDeps + 3 {
		b := mustCall[proto.CreateResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "blocker"})
		mustCall[proto.Empty](t, s, alice, proto.OpDepAdd, proto.DepArgs{From: hub.ID, To: b.ID})
	}
	sh := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: hub.ID})
	if len(sh.Deps) != proto.MaxShowDeps || sh.DepsMore != 3 {
		t.Errorf("show = %d deps and %d more, want %d and 3", len(sh.Deps), sh.DepsMore, proto.MaxShowDeps)
	}
	bl := mustCall[proto.BlockedResult](t, s, alice, proto.OpBlocked, proto.LimitArgs{})
	if len(bl.Issues) != 1 || len(bl.Issues[0].BlockedBy) != proto.MaxBlockers || bl.Issues[0].More != proto.MaxShowDeps+3-proto.MaxBlockers {
		t.Errorf("blocked = %+v, want hub with %d blockers listed", bl.Issues, proto.MaxBlockers)
	}
}

// handshakeAs connects a over a pipe and returns the welcome, which may be
// a refusal.
func handshakeAs(t *testing.T, s *Server, a store.Actor) (*proto.Frame, *conn) {
	t.Helper()
	return handshakeProto(t, s, a, proto.Proto)
}

// handshakeProto is handshakeAs with a client speaking protocol p.
func handshakeProto(t *testing.T, s *Server, a store.Actor, p int) (*proto.Frame, *conn) {
	t.Helper()
	srv, cli := net.Pipe()
	c := &conn{t: t, enc: proto.NewEncoder(cli), dec: proto.NewDecoder(cli), nc: cli}
	t.Cleanup(func() { _ = cli.Close(); c.wg.Wait() })
	c.wg.Go(func() { s.handle(t.Context(), srv) })
	c.wg.Go(func() {
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: a.Principal})
		_ = c.enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: p, Project: project, Session: a.Session, Machine: a.Machine})
	})
	return c.read(), c
}

// S-11: connections are capped per principal and in all, refused with
// busy in the welcome, and a slot frees when its connection ends.
func TestConnectionCaps(t *testing.T) {
	s := newServerWith(t, Limits{ConnsPerPrincipal: 2, Conns: 3})
	w1, c1 := handshakeAs(t, s, alice)
	w2, _ := handshakeAs(t, s, alice)
	w3, _ := handshakeAs(t, s, alice)
	if w1.Err != nil || w2.Err != nil {
		t.Fatalf("first two of alice's connections refused: %v, %v", w1.Err, w2.Err)
	}
	if w3.Err == nil || w3.Err.Code != proto.CodeBusy || !strings.Contains(w3.Err.Message, "alice is at its connection limit (2)") {
		t.Errorf("alice's third connection: %+v, want busy naming her limit", w3.Err)
	}
	if w, _ := handshakeAs(t, s, bob); w.Err != nil {
		t.Errorf("bob's connection refused: %v", w.Err)
	}
	if w, _ := handshakeAs(t, s, dana); w.Err == nil || w.Err.Code != proto.CodeBusy || !strings.Contains(w.Err.Message, "server is at its connection limit (3)") {
		t.Errorf("a fourth connection in all: %+v, want busy naming the server's limit", w.Err)
	}
	// Closing one of alice's frees its slot.
	_ = c1.nc.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		w, c := handshakeAs(t, s, alice)
		if w.Err == nil {
			break
		}
		_ = c.nc.Close()
		if time.Now().After(deadline) {
			t.Fatalf("alice's slot was never freed: %v", w.Err)
		}
		runtime.Gosched()
	}
}

// A client that leaves before reading its welcome gives back the slot the
// handshake took for it; otherwise enough of them would lock its
// principal out until a restart.
func TestWelcomeWriteFailureFreesSlot(t *testing.T) {
	s := newServer(t)
	srv, cli := net.Pipe()
	var wg sync.WaitGroup
	wg.Go(func() { s.handle(t.Context(), srv); _ = srv.Close() })
	_ = cli.SetDeadline(time.Now().Add(10 * time.Second))
	// net.Pipe has no buffer: once the hello is written the server has
	// read it, and all it does with the connection next is write the
	// welcome, which the close makes fail.
	enc := proto.NewEncoder(cli)
	if err := enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: alice.Principal}); err != nil {
		t.Fatal(err)
	}
	if err := enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: proto.Proto, Project: project, Session: alice.Session,
		Machine: alice.Machine}); err != nil {
		t.Fatal(err)
	}
	_ = cli.Close()
	wg.Wait()
	s.slots.mu.Lock()
	n, total := s.slots.by[alice.Principal], s.slots.total
	s.slots.mu.Unlock()
	if n != 0 || total != 0 {
		t.Errorf("after a client closed before its welcome, alice holds %d connection slots, %d in all; want 0", n, total)
	}
}

// S-11: a connection that sends nothing for IdleTimeout is closed with a
// note, unless it watches its inbox.
func TestIdleTimeout(t *testing.T) {
	s := newServer(t)
	var past atomic.Bool
	s.now = func() time.Time {
		if past.Load() {
			return time.Now().Add(-time.Hour) // every deadline is already gone
		}
		return time.Now()
	}
	past.Store(true)
	w, c := handshakeAs(t, s, alice)
	if w.Err != nil {
		t.Fatal(w.Err)
	}
	f := c.read()
	if f.Err == nil || f.Err.Code != proto.CodeUnavailable || !strings.Contains(f.Err.Message, "idle") {
		t.Fatalf("idle connection got %+v, want an unavailable note about idling", f)
	}

	past.Store(false)
	w, c = handshakeAs(t, s, bob)
	if w.Err != nil {
		t.Fatal(w.Err)
	}
	c.until(c.send(proto.OpWatch, proto.WatchArgs{}))
	past.Store(true)
	c.until(c.send(proto.OpReady, proto.LimitArgs{}))
	c.until(c.send(proto.OpReady, proto.LimitArgs{})) // read with the old clock's deadline, had it one
}

// The write rate limit: a principal's writes past its bucket are refused
// with busy and a wait; its reads, and others' writes, go on.
func TestWriteRateLimit(t *testing.T) {
	s := newServerWith(t, Limits{WriteBurst: 3, WriteRate: 0.001})
	_, a := handshakeAs(t, s, alice)
	for i := range 3 {
		a.until(a.send(proto.OpCreate, proto.CreateArgs{Title: fmt.Sprintf("w%d", i)}))
	}
	id := a.send(proto.OpCreate, proto.CreateArgs{Title: "one too many"})
	f := a.read()
	if f.ID != id || f.Err == nil || f.Err.Code != proto.CodeBusy || !strings.HasPrefix(f.Err.Fix, "wait ") {
		t.Fatalf("fourth write = %+v, want busy with a wait", f)
	}
	a.until(a.send(proto.OpList, proto.ListArgs{}))
	_, b := handshakeAs(t, s, bob)
	b.until(b.send(proto.OpCreate, proto.CreateArgs{Title: "bob's"}))
}

// A reply that would pass the frame limit is refused with invalid; the
// connection stays up.
func TestSendTooLarge(t *testing.T) {
	s := newServer(t)
	srv, cli := net.Pipe()
	defer func() { _ = srv.Close(); _ = cli.Close() }()
	sess := &session{enc: proto.NewEncoder(srv)}
	go func() { _ = s.send(sess, 7, map[string]string{"x": strings.Repeat("y", proto.MaxFrame)}, nil) }()
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	f, err := proto.NewDecoder(cli).Decode()
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != 7 || f.Err == nil || f.Err.Code != proto.CodeInvalid || !strings.Contains(f.Err.Message, "larger than 4 MiB") {
		t.Errorf("oversized reply = %+v, want invalid for request 7", f)
	}
}

// Prune runs the store's pruning with the configured windows.
func TestServerPrune(t *testing.T) {
	s := newServer(t)
	s.Prune(t.Context()) // nothing to prune, and no error logged
}
