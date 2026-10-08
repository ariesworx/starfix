package capture

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// sink is a Send that keeps what it is sent, failing while fail is set.
// Like the server, it refuses a whole batch that holds a record it
// refuses (refuse), or more than limit records, and keeps the first
// record it gets for each request id.
type sink struct {
	mu      sync.Mutex
	batches [][]proto.UsageRecord
	fail    error
	refuse  func(proto.UsageRecord) bool
	limit   int
	// output is the output count kept for each request id: the first sent.
	output map[string]int64
}

func (s *sink) send(_ context.Context, recs []proto.UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	if s.limit > 0 && len(recs) > s.limit {
		return proto.Errf(proto.CodeInvalid, "send fewer", fmt.Sprintf("at most %d usage records per call, not %d", s.limit, len(recs)))
	}
	for i, r := range recs {
		if s.refuse != nil && s.refuse(r) {
			return proto.Errf(proto.CodeInvalid, "correct it and retry", fmt.Sprintf("usage record %d: at is more than an hour past the server's clock", i+1))
		}
	}
	s.batches = append(s.batches, slices.Clone(recs))
	if s.output == nil {
		s.output = map[string]int64{}
	}
	for _, r := range recs {
		if _, ok := s.output[r.RequestID]; !ok && r.Output != nil {
			s.output[r.RequestID] = *r.Output
		}
	}
	return nil
}

// ids lists the request ids sent so far, and forgets them.
func (s *sink) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, b := range s.batches {
		for _, r := range b {
			out = append(out, r.RequestID)
		}
	}
	s.batches = nil
	return out
}

// layout is a session's transcripts laid out as Claude Code does: the
// main one, and a subagent's in <session>/subagents.
type layout struct {
	dir, main, sub, state string
}

func newLayout(t *testing.T) layout {
	t.Helper()
	dir := t.TempDir()
	l := layout{dir: dir, main: filepath.Join(dir, session+".jsonl"), state: filepath.Join(t.TempDir(), "starfix")}
	subdir := filepath.Join(dir, session, "subagents")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, "session.jsonl", l.main)
	l.sub = filepath.Join(subdir, "agent-a1b2c3d.jsonl")
	copyFile(t, "agent-a1b2c3d.jsonl", l.sub)
	return l
}

func copyFile(t *testing.T, name, to string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "claude", name)) //nolint:gosec // the test's fixtures
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil { //nolint:gosec // the test's temp dir
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0) //nolint:gosec // the test's temp file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// run captures the layout's session once, as a Stop hook would.
func (l layout) run(t *testing.T, s *sink) (Result, error) {
	t.Helper()
	return l.capture(t, s, false)
}

// capture captures the layout's session once; final is a SessionEnd run.
func (l layout) capture(t *testing.T, s *sink, final bool) (Result, error) {
	t.Helper()
	files, err := ClaudeFiles(l.main, session, "")
	if err != nil {
		t.Fatal(err)
	}
	return Claude(t.Context(), Input{Session: session, Files: files, StateDir: l.state, Send: s.send, Final: final})
}

// mainIDs are the responses in the main fixture. A Stop run sends
// stopIDs: the last, echoID, is followed by no other message, so it may
// still grow, and waits. So does the subagent's one response, deltaID.
var (
	mainIDs = []string{"msg_01FixtureAlpha", "msg_01FixtureBravo", "msg_01FixtureCharlie", echoID}
	stopIDs = mainIDs[:3]
)

const (
	echoID  = "msg_01FixtureEcho"
	deltaID = "msg_01FixtureDelta"
)

// Each run sends only what earlier runs did not. A line still being
// written waits for the next run, and so does a response that no other
// message follows yet, even across tool results, so its final output is
// the one sent: the sink, like the server, keeps the first.
func TestClaudeAcrossRuns(t *testing.T) {
	l := newLayout(t)
	s := &sink{}
	newFirst := claudeLine("msg_01New", "req_01New", "claude-x", `{"input_tokens":1,"output_tokens":5}`) + "\n"
	newLast := claudeLine("msg_01New", "req_01New", "claude-x", `{"input_tokens":1,"output_tokens":90}`) + "\n"
	next := claudeLine("msg_01Next", "", "claude-x", `{"input_tokens":2,"output_tokens":7}`) + "\n"
	result := `{"type":"user","sessionId":"` + session + `","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}` + "\n"
	half := claudeLine("msg_01Half", "", "claude-x", `{"input_tokens":1}`)
	steps := []struct {
		name   string
		before func()
		final  bool
		want   []string
	}{
		{name: "first run: the last response of each file waits", want: stopIDs},
		{name: "second run sends nothing"},
		{name: "a new response sends the one before; its tool result does not end it", want: []string{echoID},
			before: func() { appendTo(t, l.main, newFirst+result) }},
		{name: "its later lines still wait", before: func() { appendTo(t, l.main, newLast+result) }},
		{name: "the next response sends it once", want: []string{"msg_01New"}, before: func() { appendTo(t, l.main, next) }},
		{name: "half a line waits", before: func() { appendTo(t, l.sub, half[:len(half)/2]) }},
		{name: "the rest of it ends the subagent's response", want: []string{deltaID},
			before: func() { appendTo(t, l.sub, half[len(half)/2:]+"\n") }},
		{name: "session end sends the open responses", final: true, want: []string{"msg_01Next", "msg_01Half"}},
		{name: "another session end sends nothing", final: true},
	}
	for _, st := range steps {
		if st.before != nil {
			st.before()
		}
		res, err := l.capture(t, s, st.final)
		if got := s.ids(); err != nil || !slices.Equal(got, st.want) || res.Sent != len(st.want) {
			t.Fatalf("%s: sent %v (Sent %d), %v; want %v", st.name, got, res.Sent, err, st.want)
		}
	}
	// The new response was split by tool results and straddled runs; the
	// count kept is its last line's.
	if got := s.output["msg_01New"]; got != 90 {
		t.Errorf("response across tool results and runs kept output %d, want 90, its last line's", got)
	}
	if got := s.output["msg_01FixtureAlpha"]; got != 200 {
		t.Errorf("fixture response split by a tool result kept output %d, want 200", got)
	}
}

// A send that fails leaves the offsets, so the next run sends the same
// records again; the server keeps one of each.
func TestClaudeRetriesAfterFailedSend(t *testing.T) {
	l := newLayout(t)
	down := proto.Errf(proto.CodeUnavailable, "retry", "server down")
	s := &sink{fail: down}
	if res, err := l.run(t, s); !errors.Is(err, down) || res.Sent != 0 {
		t.Fatalf("run with the server down = Sent %d, %v; want 0 and the send's error", res.Sent, err)
	}
	s.fail = nil
	if _, err := l.run(t, s); err != nil {
		t.Fatal(err)
	}
	if got := s.ids(); !slices.Equal(got, stopIDs) {
		t.Fatalf("run after the failure sent %v, want %v", got, stopIDs)
	}
}

// Records go in batches of at most MaxBatch. When a later batch fails,
// a file whose records all went in earlier batches keeps its offset.
func TestClaudeBatches(t *testing.T) {
	dir := t.TempDir()
	big, small := filepath.Join(dir, "big.jsonl"), filepath.Join(dir, "small.jsonl")
	var b strings.Builder
	for i := range MaxBatch + 1 {
		fmt.Fprintln(&b, claudeLine(fmt.Sprintf("msg_%04d", i), "req_01", "claude-x", `{"input_tokens":1}`))
	}
	if err := os.WriteFile(big, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(small, []byte(claudeLine("msg_small", "req_01", "claude-x", `{"input_tokens":1}`)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "starfix")
	in := Input{Session: session, Files: []string{small, big}, StateDir: state, Final: true}
	// small fills batch 1 with 499 of big's; batch 2 holds big's last 2.
	calls := 0
	in.Send = func(_ context.Context, recs []proto.UsageRecord) error {
		calls++
		if len(recs) > MaxBatch {
			t.Errorf("batch of %d, more than %d", len(recs), MaxBatch)
		}
		if calls == 2 {
			return errors.New("lost")
		}
		return nil
	}
	if res, err := Claude(t.Context(), in); err == nil || res.Sent != MaxBatch {
		t.Fatalf("run failing on batch 2 = Sent %d, %v; want %d and an error", res.Sent, err, MaxBatch)
	}
	s := &sink{}
	in.Send = s.send
	if _, err := Claude(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if got := s.ids(); len(got) != MaxBatch+1 || slices.Contains(got, "msg_small") {
		t.Fatalf("rerun sent %d records (small's included: %v), want big's %d again and not small's", len(got), slices.Contains(got, "msg_small"), MaxBatch+1)
	}
}

// A file truncated or replaced since the last run is read from its start.
func TestClaudeTruncatedOrReplaced(t *testing.T) {
	tests := []struct {
		name    string
		replace func(t *testing.T, l layout)
		want    []string
	}{
		{name: "truncated", want: []string{"msg_01T"}, replace: func(t *testing.T, l layout) {
			if err := os.WriteFile(l.main, []byte(claudeLine("msg_01T", "req_01T", "claude-x", `{"input_tokens":1}`)+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "replaced by a longer file", want: []string{"msg_01R"}, replace: func(t *testing.T, l layout) {
			line := claudeLine("msg_01R", "req_01R", "claude-x", `{"input_tokens":1}`) + "\n"
			if err := os.WriteFile(l.main+".new", []byte(strings.Repeat(line, 100)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(l.main+".new", l.main); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Session-end runs: each file's last response is sent.
			l := newLayout(t)
			s := &sink{}
			if _, err := l.capture(t, s, true); err != nil {
				t.Fatal(err)
			}
			s.ids()
			tc.replace(t, l)
			if _, err := l.capture(t, s, true); err != nil {
				t.Fatal(err)
			}
			if got := s.ids(); !slices.Equal(got, tc.want) {
				t.Fatalf("after the file was %s, sent %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}

// A transcript in a format sfx does not know sends nothing and says so,
// with the version; its offset stays, so a later sfx can read it.
func TestClaudeUnrecognized(t *testing.T) {
	l := newLayout(t)
	unknown := filepath.Join(l.dir, "unknown.jsonl")
	copyFile(t, "unknown.jsonl", unknown)
	s := &sink{}
	for run := range 2 {
		res, err := Claude(t.Context(), Input{Session: session, Files: []string{unknown}, StateDir: l.state, Send: s.send})
		if err != nil || !res.Unrecognized || res.Version != "9.0.0" || res.Skipped != 2 {
			t.Fatalf("run %d over an unknown format = %+v, %v; want Unrecognized, version 9.0.0, 2 skipped", run+1, res, err)
		}
		if got := s.ids(); len(got) != 0 {
			t.Fatalf("run %d sent %v from an unknown format", run+1, got)
		}
	}
	// A file that parsed is not unrecognized for a few bad lines.
	appendTo(t, l.main, `{"type":"assistant","message":{`+"\n")
	if res, err := l.run(t, s); err != nil || res.Unrecognized || res.Skipped != 1 {
		t.Fatalf("run with one bad line among good ones = %+v, %v; want 1 skipped, not unrecognized", res, err)
	}
}

func TestStateFile(t *testing.T) {
	l := newLayout(t)
	if _, err := l.capture(t, &sink{}, true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(l.state, StateFile)
	b, err := os.ReadFile(path) //nolint:gosec // the test's temp file
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Files map[string]struct {
			Offset int64  `json:"offset"`
			Head   string `json:"head"`
		} `json:"files"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("state file is not JSON: %v\n%s", err, b)
	}
	if st, err := os.Stat(l.main); err != nil || doc.Files[l.main].Offset != st.Size() || doc.Files[l.main].Head == "" {
		t.Errorf("state for %s = %+v, want its size as offset, and a head", l.main, doc.Files[l.main])
	}
	if strings.Contains(string(b), "Fixture") {
		t.Errorf("state file holds transcript text:\n%s", b)
	}
	if runtime.GOOS == "windows" {
		return // no POSIX modes
	}
	for _, c := range []struct {
		path string
		want os.FileMode
	}{{l.state, 0o700}, {path, 0o600}} {
		if st, err := os.Stat(c.path); err != nil || st.Mode().Perm() != c.want {
			t.Errorf("mode of %s = %v, %v; want %v", c.path, st.Mode().Perm(), err, c.want)
		}
	}
}

// The state directory is tightened to 0700 if it was made wider.
func TestStateDirTightened(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	l := newLayout(t)
	if err := os.MkdirAll(l.state, 0o755); err != nil { //nolint:gosec // too wide on purpose
		t.Fatal(err)
	}
	if err := os.Chmod(l.state, 0o755); err != nil { //nolint:gosec // too wide on purpose
		t.Fatal(err)
	}
	if _, err := l.run(t, &sink{}); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(l.state); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode = %v, %v; want 0700", st.Mode().Perm(), err)
	}
}

// Entries for files that are gone are dropped; a state file that is not
// JSON is started over.
func TestStatePruneAndCorrupt(t *testing.T) {
	l := newLayout(t)
	if _, err := l.run(t, &sink{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(l.sub); err != nil {
		t.Fatal(err)
	}
	appendTo(t, l.main, claudeLine("msg_01P", "req_01P", "claude-x", `{"input_tokens":1}`)+"\n")
	if _, err := l.capture(t, &sink{}, true); err != nil {
		t.Fatal(err)
	}
	marks := loadState(l.state)
	if _, ok := marks[l.sub]; ok || len(marks) != 1 {
		t.Errorf("state after %s was removed = %v, want only %s", l.sub, marks, l.main)
	}

	if err := os.WriteFile(filepath.Join(l.state, StateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &sink{}
	if _, err := l.capture(t, s, true); err != nil {
		t.Fatalf("run over a corrupt state file: %v", err)
	}
	if got, want := s.ids(), append(slices.Clone(mainIDs), "msg_01P"); !slices.Equal(got, want) {
		t.Errorf("run over a corrupt state file sent %v, want the main transcript again: %v", got, want)
	}
	if len(loadState(l.state)) != 1 {
		t.Errorf("corrupt state file was not rewritten")
	}
}

// Runs at once, as Stop and SubagentStop can be, leave a state file that
// still reads, and nothing is lost: a later run sends nothing new.
func TestClaudeConcurrentRuns(t *testing.T) {
	l := newLayout(t)
	s := &sink{}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			files, err := ClaudeFiles(l.main, session, "")
			if err == nil {
				_, err = Claude(t.Context(), Input{Session: session, Files: files, StateDir: l.state, Send: s.send})
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent run: %v", err)
		}
	}
	got := s.ids()
	slices.Sort(got)
	if got = slices.Compact(got); !slices.Equal(got, stopIDs) {
		t.Errorf("concurrent runs sent %v, want %v (each at least once)", got, stopIDs)
	}
	if len(loadState(l.state)) != 2 {
		t.Errorf("state after concurrent runs = %v, want both files", loadState(l.state))
	}
	if _, err := l.run(t, s); err != nil || len(s.ids()) != 0 {
		t.Errorf("run after the concurrent ones sent again, or failed: %v", err)
	}
}

// One record the server refuses, such as one whose time is past the
// server's clock, refuses its whole batch. The rest are sent without it,
// it is dropped and counted, and the offsets move on, so it does not
// block every later run.
func TestClaudeDropsRefusedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	var b strings.Builder
	for i := range 7 {
		id := fmt.Sprintf("msg_%02d", i)
		if i == 4 {
			id = "msg_bad"
		}
		b.WriteString(claudeLine(id, "", "claude-x", `{"input_tokens":1}`) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &sink{refuse: func(r proto.UsageRecord) bool { return r.RequestID == "msg_bad" }}
	in := Input{Session: session, Files: []string{path}, StateDir: filepath.Join(t.TempDir(), "starfix"), Send: s.send, Final: true}
	res, err := Claude(t.Context(), in)
	got := s.ids()
	slices.Sort(got)
	want := []string{"msg_00", "msg_01", "msg_02", "msg_03", "msg_05", "msg_06"}
	if err != nil || !slices.Equal(got, want) || res.Sent != 6 || res.Skipped != 1 || res.Refused != 1 {
		t.Fatalf("run with one refused record = sent %v, %+v, %v; want %v, Sent 6, Skipped 1, Refused 1", got, res, err, want)
	}
	if pe, ok := errors.AsType[*proto.Error](res.Refusal); !ok || pe.Code != proto.CodeInvalid {
		t.Errorf("Refusal = %v, want the server's invalid refusal", res.Refusal)
	}
	if res, err := Claude(t.Context(), in); err != nil || len(s.ids()) != 0 || res.Refused != 0 {
		t.Errorf("run after the refusal = %+v, %v, sent again; want nothing sent", res, err)
	}
}

// A refusal that is not invalid, such as busy, drops nothing: the run
// stops and the next one sends the same records.
func TestClaudeKeepsRecordsWhenBusy(t *testing.T) {
	l := newLayout(t)
	s := &sink{fail: proto.Errf(proto.CodeBusy, "wait", "too many usage records today")}
	if res, err := l.run(t, s); err == nil || res.Sent != 0 || res.Refused != 0 {
		t.Fatalf("run while busy = %+v, %v; want an error and nothing dropped", res, err)
	}
	s.fail = nil
	if _, err := l.run(t, s); err != nil || !slices.Equal(s.ids(), stopIDs) {
		t.Fatalf("run after busy did not send %v again: %v", stopIDs, err)
	}
}

// A server with a smaller batch limit refuses a batch as invalid; the
// records still all go, in smaller batches, and none is dropped.
func TestClaudeSmallerBatchLimit(t *testing.T) {
	l := newLayout(t)
	s := &sink{limit: 1}
	if res, err := l.capture(t, s, true); err != nil || res.Sent != 5 || res.Refused != 0 {
		t.Fatalf("run against a limit of 1 = %+v, %v; want 5 sent, none refused", res, err)
	}
}

// Files the harness has finished writing, such as a stopped subagent's,
// are read whole even on a run that is not final.
func TestClaudeDoneFiles(t *testing.T) {
	l := newLayout(t)
	s := &sink{}
	files, err := ClaudeFiles(l.main, session, l.sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Claude(t.Context(), Input{Session: session, Files: files, StateDir: l.state, Send: s.send, Done: []string{l.sub}}); err != nil {
		t.Fatal(err)
	}
	if got, want := s.ids(), append(slices.Clone(stopIDs), deltaID); !slices.Equal(got, want) {
		t.Errorf("SubagentStop run sent %v, want %v: the stopped subagent's response too", got, want)
	}
}

func TestClaudeFiles(t *testing.T) {
	l := newLayout(t)
	other := filepath.Join(t.TempDir(), "agent-elsewhere.jsonl")
	tests := []struct {
		name                       string
		transcript, session, agent string
		want                       []string
	}{
		{name: "main and subagents", transcript: l.main, session: session, want: []string{l.main, l.sub}},
		{name: "SubagentStop's path already listed", transcript: l.main, session: session, agent: l.sub, want: []string{l.main, l.sub}},
		{name: "SubagentStop's path elsewhere", transcript: l.main, session: session, agent: other, want: []string{l.main, l.sub, other}},
		{name: "no subagents directory", transcript: l.main, session: "6a7b8c9d-0e1f-4a2b-8c3d-4e5f6a7b8c9d", want: []string{l.main}},
		{name: "session that is not a file name", transcript: l.main, session: "../" + session, want: []string{l.main}},
		{name: "relative transcript", transcript: "x.jsonl", session: session},
		{name: "relative agent path", transcript: l.main, session: "none", agent: "agent-1.jsonl", want: []string{l.main}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ClaudeFiles(tc.transcript, tc.session, tc.agent)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Errorf("ClaudeFiles(%q, %q, %q) = %v, %v; want %v", tc.transcript, tc.session, tc.agent, got, err, tc.want)
			}
		})
	}
}
