package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

var (
	dolt    *dolttest.Server
	doltErr error
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "starfix-server-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	dolt, doltErr = dolttest.Start(ctx, dir)
	cancel()
	if dolt != nil {
		defer func() { _ = dolt.Stop() }()
	}
	return m.Run()
}

const project = "00000000-0000-4000-8000-000000000001"

func newServer(t *testing.T) *Server {
	t.Helper()
	if errors.Is(doltErr, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the server tests")
	}
	if doltErr != nil {
		t.Fatalf("dolt sql-server: %v", doltErr)
	}
	dsn, err := dolt.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(Config{Store: st, Project: project, Version: "v0.2.0", PeerCheck: func(net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var (
	alice = store.Actor{Principal: "alice", Session: "s-a", Machine: "laptop-a"}
	bob   = store.Actor{Principal: "bob", Session: "s-b", Machine: "laptop-b"}
)

func call[R any](t *testing.T, s *Server, a store.Actor, op string, args any) (R, *proto.Error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var out R
	res, perr := s.Dispatch(t.Context(), a, op, raw)
	if perr != nil {
		return out, perr
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func mustCall[R any](t *testing.T, s *Server, a store.Actor, op string, args any) R {
	t.Helper()
	out, perr := call[R](t, s, a, op, args)
	if perr != nil {
		t.Fatalf("%s: %v", op, perr)
	}
	return out
}

func TestDispatchRoundTrip(t *testing.T) {
	s := newServer(t)
	p1 := 1
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "parent", Priority: &p1, Labels: []string{"x"}})
	b := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "blocker", Body: strings.Repeat("é", 400)})
	if a.Rev != 1 || !strings.HasPrefix(a.ID, "sf-") {
		t.Fatalf("create: %+v", a)
	}
	mustCall[proto.Empty](t, s, alice, proto.OpDepAdd, proto.DepArgs{From: a.ID, To: b.ID})

	ready := mustCall[proto.ListResult](t, s, alice, proto.OpReady, proto.LimitArgs{})
	if len(ready.Issues) != 1 || ready.Issues[0].ID != b.ID {
		t.Fatalf("ready: %+v", ready)
	}
	blocked := mustCall[proto.BlockedResult](t, s, alice, proto.OpBlocked, proto.LimitArgs{})
	if len(blocked.Issues) != 1 || blocked.Issues[0].ID != a.ID || blocked.Issues[0].BlockedBy[0] != b.ID {
		t.Fatalf("blocked: %+v", blocked)
	}

	compact := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: b.ID})
	if !compact.Issue.Truncated || len(compact.Issue.Body) > compactText+len("…") || !strings.HasSuffix(compact.Issue.Body, "…") {
		t.Fatalf("compact show: truncated=%v len=%d", compact.Issue.Truncated, len(compact.Issue.Body))
	}
	full := mustCall[proto.ShowResult](t, s, alice, proto.OpShow, proto.ShowArgs{ID: a.ID, Full: true})
	if full.Issue.Priority != 1 || len(full.Deps) != 1 || full.Issue.Labels[0] != "x" {
		t.Fatalf("show: %+v", full)
	}

	title := "renamed"
	u := mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: a.ID, Rev: 1, Title: &title})
	if u.Rev != 2 {
		t.Fatalf("update: %+v", u)
	}
	mustCall[proto.Empty](t, s, alice, proto.OpLabelAdd, proto.LabelArgs{ID: a.ID, Label: "y"})
	mustCall[proto.Empty](t, s, alice, proto.OpLabelRm, proto.LabelArgs{ID: a.ID, Label: "x"})
	c := mustCall[proto.CommentResult](t, s, bob, proto.OpComment, proto.CommentArgs{ID: a.ID, Body: "hello"})
	cs := mustCall[proto.CommentsResult](t, s, alice, proto.OpComments, proto.IDArgs{ID: a.ID})
	if len(cs.Comments) != 1 || cs.Comments[0].ID != c.ID || cs.Comments[0].Author != "bob" || cs.Comments[0].Session != "s-b" {
		t.Fatalf("comments: %+v", cs)
	}
	cl := mustCall[proto.WriteResult](t, s, alice, proto.OpClose, proto.CloseArgs{ID: b.ID, Reason: "done"})
	if cl.Rev != 2 {
		t.Fatalf("close: %+v", cl)
	}
	mustCall[proto.WriteResult](t, s, alice, proto.OpReopen, proto.ReopenArgs{ID: b.ID})
	mustCall[proto.Empty](t, s, alice, proto.OpDepRm, proto.DepArgs{From: a.ID, To: b.ID})

	list := mustCall[proto.ListResult](t, s, alice, proto.OpList, proto.ListArgs{Labels: []string{"y"}})
	if len(list.Issues) != 1 || list.Issues[0].Title != "renamed" {
		t.Fatalf("list: %+v", list)
	}
	h := mustCall[proto.HistoryResult](t, s, alice, proto.OpHistory, proto.IDArgs{ID: a.ID})
	var ops []string
	for _, e := range h.Events {
		ops = append(ops, e.Op+"/"+e.Principal)
	}
	want := "issue.create/alice dep.add/alice issue.update/bob label.add/alice label.remove/alice comment.add/bob dep.remove/alice"
	if got := strings.Join(ops, " "); got != want {
		t.Fatalf("history:\n got %s\nwant %s", got, want)
	}
}

func TestDispatchErrors(t *testing.T) {
	s := newServer(t)
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "a"})
	b := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "b"})
	title := "moved on"
	mustCall[proto.WriteResult](t, s, bob, proto.OpUpdate, proto.UpdateArgs{ID: a.ID, Rev: 1, Title: &title})
	mustCall[proto.Empty](t, s, alice, proto.OpDepAdd, proto.DepArgs{From: a.ID, To: b.ID})
	mustCall[proto.WriteResult](t, s, alice, proto.OpClose, proto.CloseArgs{ID: b.ID})
	stale := "stale"
	open := "in_progress"

	tests := []struct {
		name    string
		op      string
		args    any
		code    proto.Code
		msg     string
		fix     string
		rawArgs string
	}{
		{name: "conflict names who and which rev", op: proto.OpUpdate, args: proto.UpdateArgs{ID: a.ID, Rev: 1, Title: &stale},
			code: proto.CodeConflict, msg: a.ID + " changed since rev 1 (now rev 2 by bob)", fix: "re-read with `sfx show " + a.ID + "`"},
		{name: "close with stale rev", op: proto.OpClose, args: proto.CloseArgs{ID: a.ID, Rev: 1},
			code: proto.CodeConflict, msg: "changed since rev 1 (now rev 2 by bob)"},
		{name: "not found", op: proto.OpShow, args: proto.ShowArgs{ID: "sf-zzzzzzzz"},
			code: proto.CodeNotFound, msg: "issue sf-zzzzzzzz not found", fix: "sfx list"},
		{name: "comments of a missing issue", op: proto.OpComments, args: proto.IDArgs{ID: "sf-zzzzzzzz"}, code: proto.CodeNotFound},
		{name: "history of a missing issue", op: proto.OpHistory, args: proto.IDArgs{ID: "sf-zzzzzzzz"}, code: proto.CodeNotFound},
		{name: "exists", op: proto.OpCreate, args: proto.CreateArgs{ID: b.ID, Title: "dup"},
			code: proto.CodeExists, msg: "issue " + b.ID + " already exists", fix: "omit --id"},
		{name: "cycle", op: proto.OpDepAdd, args: proto.DepArgs{From: b.ID, To: a.ID},
			code: proto.CodeCycle, msg: a.ID + " already depends on " + b.ID + ", so this would make a cycle", fix: "dep rm"},
		{name: "invalid title", op: proto.OpCreate, args: proto.CreateArgs{Title: " "},
			code: proto.CodeInvalid, msg: "title must be 1-500 characters", fix: "`sfx create -h`"},
		{name: "invalid dep type", op: proto.OpDepAdd, args: proto.DepArgs{From: a.ID, To: b.ID, Type: "likes"},
			code: proto.CodeInvalid, fix: "`sfx dep add -h`"},
		{name: "status on a closed issue", op: proto.OpUpdate, args: proto.UpdateArgs{ID: b.ID, Rev: 2, Status: &open},
			code: proto.CodeInvalid, msg: "is closed", fix: "sfx reopen " + b.ID},
		{name: "close twice", op: proto.OpClose, args: proto.CloseArgs{ID: b.ID}, code: proto.CodeInvalid, fix: "nothing to do"},
		{name: "unknown op", op: "explode", args: struct{}{}, code: proto.CodeInvalid, msg: `unknown operation "explode"`},
		{name: "unknown field", op: proto.OpShow, rawArgs: `{"id":"sf-aaaa","colour":1}`, code: proto.CodeInvalid, msg: "colour"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := json.RawMessage(tc.rawArgs)
			if tc.rawArgs == "" {
				var err error
				if raw, err = json.Marshal(tc.args); err != nil {
					t.Fatal(err)
				}
			}
			_, perr := s.Dispatch(t.Context(), alice, tc.op, raw)
			if perr == nil {
				t.Fatal("succeeded")
			}
			if perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) {
				t.Fatalf("got %+v\nwant code %s, message containing %q, fix containing %q", perr, tc.code, tc.msg, tc.fix)
			}
			if perr.Fix == "" {
				t.Fatal("no fix line")
			}
		})
	}
}

// pipeConn wires a handshake through net.Pipe, playing the bridge and the
// client.
func handshake(t *testing.T, s *Server, frames ...*proto.Frame) (*proto.Frame, error) {
	t.Helper()
	srv, cli := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() { _ = srv.Close() }()
		s.handle(t.Context(), srv)
	}()
	defer func() { _ = cli.Close(); <-done }()
	enc, dec := proto.NewEncoder(cli), proto.NewDecoder(cli)
	go func() {
		for _, f := range frames {
			if enc.Encode(f) != nil {
				return
			}
		}
	}()
	_ = cli.SetReadDeadline(time.Now().Add(10 * time.Second))
	return dec.Decode()
}

func TestHandshake(t *testing.T) {
	s := newServer(t)
	bridge := &proto.Frame{T: proto.FrameBridge, Principal: "alice"}
	hello := func(p int, proj, session string) *proto.Frame {
		return &proto.Frame{T: proto.FrameHello, Version: "v0.1.0", Proto: p, Project: proj, Session: session, Machine: "m"}
	}
	tests := []struct {
		name    string
		frames  []*proto.Frame
		code    proto.Code // "" means welcomed
		closed  bool       // no reply at all
		session string
	}{
		{name: "welcome with client session", frames: []*proto.Frame{bridge, hello(2, project, "s-mine")}, session: "s-mine"},
		{name: "protocol 1 still welcome", frames: []*proto.Frame{bridge, hello(1, project, "s-old")}, session: "s-old"},
		{name: "welcome assigns a session", frames: []*proto.Frame{bridge, hello(1, project, "")}, session: "s-"},
		{name: "protocol too new", frames: []*proto.Frame{bridge, hello(3, project, "")}, code: proto.CodeVersion},
		{name: "protocol too old", frames: []*proto.Frame{bridge, hello(0, project, "")}, code: proto.CodeVersion},
		{name: "other project", frames: []*proto.Frame{bridge, hello(1, "00000000-0000-4000-8000-000000000002", "")}, code: proto.CodeNotFound},
		{name: "request before hello", frames: []*proto.Frame{bridge, {T: proto.FrameReq, ID: 1, Op: "show"}}, code: proto.CodeInvalid},
		{name: "no bridge frame", frames: []*proto.Frame{hello(1, project, "")}, closed: true},
		{name: "client forges principal", frames: []*proto.Frame{{T: proto.FrameBridge, Principal: "Robert'); DROP"}}, closed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := handshake(t, s, tc.frames...)
			if tc.closed {
				if err == nil {
					t.Fatalf("got a reply: %+v", f)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.T != proto.FrameWelcome || f.Version != "v0.2.0" || f.Min != 1 || f.Max != 2 {
				t.Fatalf("welcome: %+v", f)
			}
			if tc.code == "" {
				if f.Err != nil || !strings.HasPrefix(f.Session, tc.session) || f.Principal != "alice" {
					t.Fatalf("welcome: %+v (err %v)", f, f.Err)
				}
				return
			}
			if f.Err == nil || f.Err.Code != tc.code || f.Err.Fix == "" {
				t.Fatalf("got %+v, want refusal %s with a fix", f.Err, tc.code)
			}
		})
	}
}

func TestBridgeFrameOnlyFirst(t *testing.T) {
	s := newServer(t)
	srv, cli := net.Pipe()
	go func() { s.handle(t.Context(), srv); _ = srv.Close() }()
	defer func() { _ = cli.Close() }()
	enc, dec := proto.NewEncoder(cli), proto.NewDecoder(cli)
	go func() {
		_ = enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: "alice"})
		_ = enc.Encode(&proto.Frame{T: proto.FrameHello, Proto: 1, Project: project})
	}()
	if f, err := dec.Decode(); err != nil || f.Err != nil {
		t.Fatalf("welcome: %v %+v", err, f)
	}
	go func() { _ = enc.Encode(&proto.Frame{T: proto.FrameBridge, Principal: "root"}) }()
	f, err := dec.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if f.T != proto.FrameRes || f.Err == nil || f.Err.Code != proto.CodeInvalid {
		t.Fatalf("second bridge frame: %+v", f)
	}
	if _, err := dec.Decode(); err == nil {
		t.Fatal("connection stayed open")
	}
}

func TestListen(t *testing.T) {
	base, err := os.MkdirTemp("", "sfl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })

	sock := filepath.Join(base, "run", "d.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Dir(sock))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v %v", fi.Mode(), err)
	}
	if _, err := Listen(sock); err == nil || !strings.Contains(err.Error(), "another starfixd") {
		t.Fatalf("second listener: %v", err)
	}
	if err := CheckPeer(mustDial(t, sock, l)); err != nil {
		t.Fatalf("own uid refused: %v", err)
	}
	_ = l.Close()

	// A stale socket file (no listener) is replaced.
	stale := filepath.Join(base, "run", "stale.sock")
	ul, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ul.SetUnlinkOnClose(false)
	_ = ul.Close()
	l2, err := Listen(stale)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	_ = l2.Close()

	open := filepath.Join(base, "open")
	if err := os.Mkdir(open, 0o755); err != nil { //nolint:gosec // deliberately too open
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil { //nolint:gosec // deliberately too open
		t.Fatal(err)
	}
	if _, err := Listen(filepath.Join(open, "d.sock")); err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("open directory: %v", err)
	}
}

func mustDial(t *testing.T, path string, l net.Listener) net.Conn {
	t.Helper()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	sc := <-accepted
	if sc == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

func TestBridgeUnavailable(t *testing.T) {
	var out strings.Builder
	err := Bridge(t.Context(), filepath.Join(t.TempDir(), "none.sock"), "alice", strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("bridged to nothing")
	}
	f, derr := proto.NewDecoder(strings.NewReader(out.String())).Decode()
	if derr != nil || f.T != proto.FrameWelcome || f.Err == nil || f.Err.Code != proto.CodeUnavailable ||
		!strings.Contains(f.Err.Fix, "systemctl start starfixd") {
		t.Fatalf("refusal: %+v %v", f, derr)
	}
	out.Reset()
	if err := Bridge(t.Context(), "/nonexistent", "Bad Name", strings.NewReader(""), &out); err == nil {
		t.Fatal("bad principal accepted")
	}
	if !strings.Contains(out.String(), `"c":"auth"`) {
		t.Fatalf("refusal: %s", out.String())
	}
}

func TestResolveSettings(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := write("good.yaml", "dsn: starfix:pw@tcp(127.0.0.1:3306)/sf\nsocket: /run/x/s.sock\nproject: "+project+"\n", 0o600)
	loose := write("loose.yaml", "dsn: starfix:pw@tcp(127.0.0.1:3306)/sf\n", 0o644)
	nopw := write("nopw.yaml", "dsn: starfix@tcp(127.0.0.1:3306)/sf\n", 0o644)
	unknown := write("unknown.yaml", "dns: typo\n", 0o600)
	unit := write("unit.yaml", "systemd_unit: starfixd.service\n", 0o600)
	badUnit := write("badunit.yaml", "systemd_unit: --no-block\n", 0o600)
	logs := write("logs.yaml", "log_level: warn\nlog_format: json\n", 0o600)
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	tests := []struct {
		name  string
		flags Settings
		path  string
		env   map[string]string
		check func(Settings) bool
		err   string
	}{
		{name: "file", path: good, check: func(s Settings) bool {
			return s.Socket == "/run/x/s.sock" && s.Project == project && s.Prefix == "sf" && strings.Contains(s.DSN, "pw@")
		}},
		{name: "env beats file", path: good, env: map[string]string{EnvSocket: "/run/env.sock"},
			check: func(s Settings) bool { return s.Socket == "/run/env.sock" }},
		{name: "flag beats env", path: good, flags: Settings{Socket: "/run/flag.sock"}, env: map[string]string{EnvSocket: "/run/env.sock"},
			check: func(s Settings) bool { return s.Socket == "/run/flag.sock" }},
		{name: "config from env", env: map[string]string{EnvConfig: good}, check: func(s Settings) bool { return s.Project == project }},
		{name: "defaults without a file", path: "", check: func(s Settings) bool { return s.Socket == DefaultSocket && s.Prefix == "sf" }},
		{name: "password in flag refused", flags: Settings{DSN: "u:secret@tcp(h:1)/d"}, err: "process list"},
		{name: "password in a loose file refused", path: loose, err: "chmod 600"},
		{name: "loose file without password", path: nopw, check: func(s Settings) bool { return s.DSN != "" }},
		{name: "unknown key refused", path: unknown, err: "dns"},
		{name: "systemd unit", path: unit, check: func(s Settings) bool { return s.SystemdUnit == "starfixd.service" }},
		{name: "option-like unit refused", path: badUnit, err: "is not a unit name"},
		{name: "log settings default", path: "", check: func(s Settings) bool { return s.LogLevel == "info" && s.LogFormat == "text" }},
		{name: "log settings from file", path: logs, check: func(s Settings) bool { return s.LogLevel == "warn" && s.LogFormat == "json" }},
		{name: "log level env beats file", path: logs, env: map[string]string{EnvLogLevel: "debug"},
			check: func(s Settings) bool { return s.LogLevel == "debug" && s.LogFormat == "json" }},
		{name: "log format flag beats env", path: logs, flags: Settings{LogFormat: "text"}, env: map[string]string{EnvLogFormat: "json"},
			check: func(s Settings) bool { return s.LogFormat == "text" }},
		{name: "unknown log level refused", flags: Settings{LogLevel: "loud"}, err: "log level"},
		{name: "unknown log format refused", flags: Settings{LogFormat: "xml"}, err: "log format"},
		{name: "named file must exist", path: filepath.Join(dir, "missing.yaml"), err: "no such file"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env = tc.env
			s, err := ResolveSettings(tc.flags, tc.path, getenv)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("got %v, want error containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tc.check(s) {
				t.Fatalf("settings: %+v", s)
			}
		})
	}
}

func TestDispatchWork(t *testing.T) {
	s := newServer(t)
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "first", Acceptance: "it works"})
	b := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "second"})

	st := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{})
	if st.Issue.ID != a.ID || st.Issue.Status != "in_progress" || st.Issue.Assignee != "alice" ||
		st.Issue.Acceptance != "it works" || st.Handoff != nil {
		t.Fatalf("start: %+v", st)
	}
	h := mustCall[proto.WriteResult](t, s, alice, proto.OpHandoff, proto.HandoffArgs{ID: a.ID, Note: "half done", Release: true})
	if h.ID != a.ID || h.Rev != 3 {
		t.Fatalf("handoff: %+v", h)
	}
	st = mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{ID: a.ID})
	if st.Issue.Assignee != "bob" || st.Handoff == nil || st.Handoff.Body != "half done" ||
		st.Handoff.Kind != "handoff" || st.Handoff.Author != "alice" {
		t.Fatalf("start after handoff: %+v %+v", st.Issue, st.Handoff)
	}
	p0 := 0
	f := mustCall[proto.FinishResult](t, s, bob, proto.OpFinish, proto.FinishArgs{ID: a.ID, Reason: "done", Handoff: "all yours",
		Discovered: []proto.Discovered{{Title: "follow-up", Type: "bug", Priority: &p0}}})
	if f.ID != a.ID || len(f.Created) != 1 {
		t.Fatalf("finish: %+v", f)
	}
	show := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: f.Created[0]})
	if show.Issue.Type != "bug" || show.Issue.Priority != 0 || len(show.Deps) != 1 ||
		show.Deps[0].To != a.ID || show.Deps[0].Type != "discovered-from" {
		t.Fatalf("discovered: %+v", show)
	}
	cs := mustCall[proto.CommentsResult](t, s, bob, proto.OpComments, proto.IDArgs{ID: a.ID})
	if len(cs.Comments) != 2 || cs.Comments[1].Kind != "handoff" || cs.Comments[1].Body != "all yours" {
		t.Fatalf("comments: %+v", cs)
	}

	// Errors: alice holds b; bob is pointed at the next ready issue.
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: b.ID})
	tests := []struct {
		name string
		a    store.Actor
		op   string
		args any
		code proto.Code
		msg  string
		fix  string
	}{
		{name: "start held names the next ready", a: bob, op: proto.OpStart, args: proto.StartArgs{ID: b.ID},
			code: proto.CodeConflict, msg: b.ID + " is in progress by alice; next ready: " + f.Created[0],
			fix: "take that one with `sfx start " + f.Created[0] + "`"},
		{name: "finish held", a: bob, op: proto.OpFinish, args: proto.FinishArgs{ID: b.ID},
			code: proto.CodeConflict, msg: b.ID + " is in progress by alice", fix: "`sfx close " + b.ID + "`"},
		{name: "release held", a: bob, op: proto.OpHandoff, args: proto.HandoffArgs{ID: b.ID, Note: "x", Release: true},
			code: proto.CodeConflict, msg: "in progress by alice", fix: "without --release"},
		{name: "start closed", a: bob, op: proto.OpStart, args: proto.StartArgs{ID: a.ID},
			code: proto.CodeInvalid, msg: "is closed", fix: "sfx reopen " + a.ID},
		{name: "finish closed", a: bob, op: proto.OpFinish, args: proto.FinishArgs{ID: a.ID}, code: proto.CodeInvalid, fix: "nothing to do"},
		{name: "start missing", a: bob, op: proto.OpStart, args: proto.StartArgs{ID: "sf-zzzzzzzz"},
			code: proto.CodeNotFound, msg: "issue sf-zzzzzzzz not found"},
		{name: "bad discovered", a: alice, op: proto.OpFinish, args: proto.FinishArgs{ID: b.ID, Discovered: []proto.Discovered{{Title: "x", Type: "story"}}},
			code: proto.CodeInvalid, msg: "story", fix: "`sfx finish -h`"},
		{name: "empty note", a: alice, op: proto.OpHandoff, args: proto.HandoffArgs{ID: b.ID}, code: proto.CodeInvalid, msg: "comment must be"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.Empty](t, s, tc.a, tc.op, tc.args)
			if perr == nil {
				t.Fatal("succeeded")
			}
			if perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !strings.Contains(perr.Fix, tc.fix) || perr.Fix == "" {
				t.Fatalf("got %+v\nwant code %s, message containing %q, fix containing %q", perr, tc.code, tc.msg, tc.fix)
			}
		})
	}

	// Nothing left to start.
	mustCall[proto.StartResult](t, s, bob, proto.OpStart, proto.StartArgs{})
	_, perr := call[proto.Empty](t, s, bob, proto.OpStart, proto.StartArgs{})
	if perr == nil || perr.Code != proto.CodeNotFound || perr.Message != "nothing is ready to start" || !strings.Contains(perr.Fix, "sfx blocked") {
		t.Fatalf("nothing ready: %+v", perr)
	}
	_, perr = call[proto.Empty](t, s, bob, proto.OpStart, proto.StartArgs{ID: b.ID})
	if perr == nil || !strings.Contains(perr.Message, "nothing else is ready") {
		t.Fatalf("held with nothing ready: %+v", perr)
	}
}

func TestDispatchClaims(t *testing.T) {
	s := newServer(t)
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})

	st := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: a.ID, Lease: "2h"})
	if c := st.Claim; c == nil || c.Epoch != 1 || c.By != "alice" || c.Session != alice.Session ||
		time.Until(c.ExpiresAt) < 119*time.Minute {
		t.Fatalf("start claim: %+v", st.Claim)
	}
	show := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: a.ID})
	if show.Claim == nil || show.Claim.Epoch != 1 {
		t.Fatalf("show claim: %+v", show.Claim)
	}
	r := mustCall[proto.ClaimsResult](t, s, alice, proto.OpRenew, proto.RenewArgs{Lease: "3d", All: true})
	if len(r.Claims) != 1 || time.Until(r.Claims[0].ExpiresAt) < 71*time.Hour {
		t.Fatalf("renew: %+v", r)
	}

	// Another session of alice takes over; the first session's finish is
	// fenced off by its epoch.
	alice2 := store.Actor{Principal: "alice", Session: "s-a2", Machine: "desktop"}
	if st := mustCall[proto.StartResult](t, s, alice2, proto.OpStart, proto.StartArgs{ID: a.ID}); st.Claim.Epoch != 2 {
		t.Fatalf("takeover: %+v", st.Claim)
	}
	_, perr := call[proto.Empty](t, s, alice, proto.OpFinish, proto.FinishArgs{ID: a.ID, Epoch: 1})
	if perr == nil || perr.Code != proto.CodeConflict ||
		perr.Message != "your claim on "+a.ID+" (epoch 1) was lost; it is now epoch 2, held by alice/s-a2" ||
		!strings.Contains(perr.Fix, "sfx comment "+a.ID) {
		t.Fatalf("stale finish: %+v", perr)
	}
	for _, l := range []string{"5s", "8d", "soon"} {
		_, perr := call[proto.Empty](t, s, alice, proto.OpStart, proto.StartArgs{ID: a.ID, Lease: l})
		if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, "1m to 7d") {
			t.Fatalf("lease %q: %+v", l, perr)
		}
	}
	mustCall[proto.FinishResult](t, s, alice2, proto.OpFinish, proto.FinishArgs{ID: a.ID, Epoch: 2})
	if show := mustCall[proto.ShowResult](t, s, bob, proto.OpShow, proto.ShowArgs{ID: a.ID}); show.Claim != nil {
		t.Fatalf("claim after finish: %+v", show.Claim)
	}
	s.Reap(t.Context()) // nothing expired: a no-op
}

// A welcomed connection registers its session, with the hello's harness;
// a harness the store would refuse is dropped, not fatal.
func TestHandshakeRegistersAgent(t *testing.T) {
	s := newServer(t)
	bridge := func(p string) *proto.Frame { return &proto.Frame{T: proto.FrameBridge, Principal: p} }
	hello := func(session, harness string) *proto.Frame {
		return &proto.Frame{T: proto.FrameHello, Proto: 2, Project: project, Session: session, Machine: "laptop-a", Harness: harness}
	}
	for _, fs := range [][]*proto.Frame{
		{bridge("alice"), hello("s-1", "claude-code")},
		{bridge("bob"), hello("s-2", "Not A Harness")},
	} {
		if f, err := handshake(t, s, fs...); err != nil || f.Err != nil {
			t.Fatalf("handshake: %v %+v", err, f)
		}
	}
	who := mustCall[proto.WhoResult](t, s, alice, proto.OpWho, proto.WhoArgs{})
	got := map[string]string{}
	for _, a := range who.Agents {
		got[a.Principal+"/"+a.Session+" on "+a.Machine] = a.Harness
	}
	want := map[string]string{"alice/s-1 on laptop-a": "claude-code", "bob/s-2 on laptop-a": ""}
	if !maps.Equal(got, want) {
		t.Fatalf("who after handshakes = %v, want %v", got, want)
	}
}

func TestDispatchWho(t *testing.T) {
	s := newServer(t)
	a := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: a.ID})
	// renew keeps a session present, holding claims or not.
	mustCall[proto.ClaimsResult](t, s, alice, proto.OpRenew, proto.RenewArgs{})
	mustCall[proto.ClaimsResult](t, s, bob, proto.OpRenew, proto.RenewArgs{})

	who := mustCall[proto.WhoResult](t, s, bob, proto.OpWho, proto.WhoArgs{Since: "1h"})
	if len(who.Agents) != 2 || who.Now.IsZero() {
		t.Fatalf("who = %+v, want two agents and the server's now", who)
	}
	for _, ag := range who.Agents {
		var want []string
		if ag.Principal == "alice" {
			want = []string{a.ID}
		}
		if !slices.Equal(ag.Claims, want) || ag.LastSeen.IsZero() || ag.Started.IsZero() {
			t.Errorf("who agent %+v, want claims %v and times", ag, want)
		}
	}
	for _, since := range []string{"soon", "8d", "-1h"} {
		_, perr := call[proto.WhoResult](t, s, bob, proto.OpWho, proto.WhoArgs{Since: since})
		if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Fix, "7d") {
			t.Errorf("who since %q = %+v, want invalid with a fix naming 7d", since, perr)
		}
	}
}

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name          string
		level, format string
		want, notWant string
	}{
		{"text hides debug", "info", "text", "level=INFO msg=shown", "hidden"},
		{"debug shows debug", "debug", "text", "msg=hidden", ""},
		{"json", "info", "json", `"msg":"shown"`, "hidden"},
		{"warn hides info", "warn", "text", "", "shown"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log, err := NewLogger(&buf, tc.level, tc.format)
			if err != nil {
				t.Fatal(err)
			}
			log.Debug("hidden")
			log.Info("shown")
			got := buf.String()
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("NewLogger(%q, %q) wrote %q, want it to contain %q", tc.level, tc.format, got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("NewLogger(%q, %q) wrote %q, want no %q", tc.level, tc.format, got, tc.notWant)
			}
		})
	}
	if _, err := NewLogger(io.Discard, "loud", "text"); err == nil {
		t.Error("NewLogger accepted level loud")
	}
}
