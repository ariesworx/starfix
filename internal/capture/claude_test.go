package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// session is the session id in the fixtures.
const session = "5f0c1d2e-3a4b-4c5d-8e6f-7a8b9c0d1e2f"

func n(v int64) *int64 { return &v }

// claudeLine builds one assistant transcript line for session, with the
// usage given as JSON, or none when usage is "".
func claudeLine(id, req, model, usage string) string {
	m := map[string]any{"type": "assistant", "sessionId": session, "requestId": req,
		"timestamp": "2026-10-08T09:00:05.000Z", "version": "2.1.300", "isSidechain": false,
		"message": map[string]any{"id": id, "model": model, "role": "assistant",
			"content": []any{map[string]any{"type": "text", "text": "Fixture reply."}}}}
	if usage != "" {
		m["message"].(map[string]any)["usage"] = json.RawMessage(usage)
	}
	b, err := json.Marshal(m)
	if err != nil {
		panic(err) // only on a programming error in the test
	}
	return string(b)
}

func TestParseClaudeLine(t *testing.T) {
	at := time.Date(2026, 10, 8, 9, 0, 5, 0, time.UTC)
	full := `{"input_tokens":10,"output_tokens":200,"cache_creation_input_tokens":3000,"cache_read_input_tokens":15000,
		"cache_creation":{"ephemeral_1h_input_tokens":1000,"ephemeral_5m_input_tokens":2000},
		"output_tokens_details":{"thinking_tokens":50},"service_tier":"standard","speed":"standard",
		"server_tool_use":{"web_search_requests":0},"iterations":[{"input_tokens":10,"output_tokens":200}],
		"inference_geo":"","fallback_credit":null}`
	tests := []struct {
		name, line string
		want       verdict
		rec        proto.UsageRecord
	}{
		{name: "every field", line: claudeLine("msg_01A", "req_01A", "claude-opus-4-5-20251101", full), want: lineUsage,
			rec: proto.UsageRecord{Harness: "claude-code", RequestID: "msg_01A", Model: "claude-opus-4-5-20251101",
				At: at, Granularity: "request", Tokens: proto.Tokens{Input: n(10), Output: n(200), CacheWrite: n(3000),
					CacheWrite1h: n(1000), CacheRead: n(15000)}}},
		{name: "missing counts are unknown, not 0", line: claudeLine("msg_01A", "req_01A", "claude-sonnet-4-5", `{"input_tokens":7,"output_tokens":30}`),
			want: lineUsage, rec: proto.UsageRecord{Harness: "claude-code", RequestID: "msg_01A", Model: "claude-sonnet-4-5",
				At: at, Granularity: "request", Tokens: proto.Tokens{Input: n(7), Output: n(30)}}},
		{name: "a one-hour part larger than the cache write is unknown", want: lineUsage,
			line: claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":1,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_1h_input_tokens":9}}`),
			rec: proto.UsageRecord{Harness: "claude-code", RequestID: "msg_01A", Model: "claude-x",
				At: at, Granularity: "request", Tokens: proto.Tokens{Input: n(1), CacheWrite: n(5)}}},
		{name: "synthetic model", line: claudeLine("msg_01A", "req_01A", "<synthetic>", `{"input_tokens":0,"output_tokens":0}`), want: lineOther},
		{name: "user line", line: `{"type":"user","sessionId":"` + session + `","message":{"role":"user","content":"x"},"toolUseResult":{"usage":{"input_tokens":9}}}`, want: lineOther},
		{name: "cost-state line", line: `{"type":"cost-state","totalCostUSD":1.5,"modelUsage":{"m":{"inputTokens":9}}}`, want: lineOther},
		{name: "another session's line", line: strings.Replace(claudeLine("msg_01A", "req_01A", "claude-x", full), session, "6a7b8c9d-0e1f-4a2b-8c3d-4e5f6a7b8c9d", 1), want: lineOther},
		{name: "not JSON", line: `{"type":"user",`, want: lineOther},
		{name: "assistant, not JSON", line: `{"type":"assistant","message":{`, want: lineBad},
		{name: "no usage", line: claudeLine("msg_01A", "req_01A", "claude-x", ""), want: lineBad},
		{name: "usage with no count", line: claudeLine("msg_01A", "req_01A", "claude-x", `{"tokens":{"in":5}}`), want: lineBad},
		{name: "no session", line: strings.Replace(claudeLine("msg_01A", "req_01A", "claude-x", full), session, "", 1), want: lineBad},
		// Lines from remote_projects sessions have no requestId (or version):
		// the message id alone names the response.
		{name: "no request id", line: claudeLine("msg_01A", "", "claude-x", `{"input_tokens":7}`), want: lineUsage,
			rec: proto.UsageRecord{Harness: "claude-code", RequestID: "msg_01A", Model: "claude-x",
				At: at, Granularity: "request", Tokens: proto.Tokens{Input: n(7)}}},
		{name: "no message id", line: claudeLine("", "req_01A", "claude-x", full), want: lineBad},
		{name: "no model", line: claudeLine("msg_01A", "req_01A", "", full), want: lineBad},
		{name: "model out of shape", line: claudeLine("msg_01A", "req_01A", "claude x\u202e", full), want: lineBad},
		{name: "message id out of shape", line: claudeLine("msg 01A", "req_01A", "claude-x", full), want: lineBad},
		{name: "count as a string", line: claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":"10"}`), want: lineBad},
		{name: "count with a fraction", line: claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":1.5}`), want: lineBad},
		{name: "negative count", line: claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":-1}`), want: lineBad},
		{name: "count past the server's bound", line: claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":1000000000001}`), want: lineBad},
		{name: "no timestamp", line: strings.Replace(claudeLine("msg_01A", "req_01A", "claude-x", full), `"timestamp":"2026-10-08T09:00:05.000Z",`, "", 1), want: lineBad},
		{name: "timestamp before 2020", line: strings.Replace(claudeLine("msg_01A", "req_01A", "claude-x", full), "2026-10-08", "2019-12-31", 1), want: lineBad},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec, got, version := parseClaudeLine([]byte(tc.line), session)
			if got != tc.want {
				t.Fatalf("parseClaudeLine(%s) = verdict %v, want %v", tc.line, got, tc.want)
			}
			if got == lineBad && strings.Contains(tc.line, `"version":"2.1.300"`) && version != "2.1.300" {
				t.Errorf("parseClaudeLine(%s) version = %q, want 2.1.300", tc.line, version)
			}
			if got == lineUsage && !sameRecord(rec, tc.rec) {
				t.Errorf("parseClaudeLine(%s) =\n%s\nwant\n%s", tc.line, show(rec), show(tc.rec))
			}
		})
	}
}

// A version that is not a plain version string is not repeated in the
// note.
func TestParseClaudeLineVersion(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"2.1.300", "2.1.300"},
		{"2.1.300-beta.1+abc", "2.1.300-beta.1+abc"},
		{"2.1.300\x1b[2J", ""},
		{strings.Repeat("9", 40), ""},
	} {
		line := strings.Replace(claudeLine("msg_01A", "req_01A", "claude-x", ""), "2.1.300", tc.version, 1)
		if _, _, got := parseClaudeLine([]byte(line), session); got != tc.want {
			t.Errorf("parseClaudeLine(version %q) version = %q, want %q", tc.version, got, tc.want)
		}
	}
}

// sameRecord compares records, counts by value.
func sameRecord(a, b proto.UsageRecord) bool {
	return show(a) == show(b)
}

// show renders a record for comparison and messages.
func show(r proto.UsageRecord) string {
	b, err := json.Marshal(r)
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// The fixture transcript mirrors a real one: each response on one line
// per content block, all with the same usage, among lines of other types.
func TestReadClaudeFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	copyFile(t, "session.jsonl", path)
	f, err := os.Open(path) //nolint:gosec // the test's temp file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	got, err := readClaude(f, session, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got.recs {
		ids = append(ids, r.RequestID)
	}
	want := []string{"msg_01FixtureAlpha", "msg_01FixtureBravo", "msg_01FixtureCharlie", "msg_01FixtureEcho"}
	if !slices.Equal(ids, want) {
		t.Fatalf("readClaude(session.jsonl) request ids = %v, want %v", ids, want)
	}
	// Response B is on seven lines: counted once, not seven times, with
	// the output of its last line, which grew from 5 to 120.
	b := got.recs[1]
	if *b.Input != 4 || *b.Output != 120 || *b.CacheWrite != 500 || *b.CacheWrite1h != 0 || *b.CacheRead != 18000 {
		t.Errorf("response on seven lines = %s, want its counts once", show(b))
	}
	if c := got.recs[2]; c.Input == nil || *c.Input != 7 || c.CacheWrite != nil || c.CacheWrite1h != nil || c.CacheRead != nil {
		t.Errorf("response without cache fields = %s, want them unknown", show(c))
	}
	if e := got.recs[3]; *e.Input != 6 || *e.Output != 50 || *e.CacheRead != 4000 {
		t.Errorf("response with no requestId = %s, want its counts, output 50", show(e))
	}
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if got.end != st.Size() || got.bad != 0 || got.long != 0 {
		t.Errorf("readClaude(session.jsonl) end %d bad %d long %d, want end %d and none skipped", got.end, got.bad, got.long, st.Size())
	}
}

// Lines of one response may carry a growing output count; the largest
// is the response's.
func TestReadClaudeKeepsLargestOutput(t *testing.T) {
	lines := claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5,"output_tokens":1}`) + "\n" +
		claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5,"output_tokens":90}`) + "\n" +
		claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5,"output_tokens":1}`) + "\n"
	got, err := readClaude(strings.NewReader(lines), session, true)
	if err != nil || len(got.recs) != 1 || *got.recs[0].Output != 90 {
		t.Fatalf("readClaude(growing output) = %+v, %v; want one record with output 90", got.recs, err)
	}
}

// A last line without its newline is still being written: it is left
// for the next run.
func TestReadClaudePartialLine(t *testing.T) {
	first := claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5}`) + "\n"
	second := claudeLine("msg_01B", "req_01B", "claude-x", `{"input_tokens":6}`)
	got, err := readClaude(strings.NewReader(first+second[:len(second)/2]), session, true)
	if err != nil || len(got.recs) != 1 || got.end != int64(len(first)) {
		t.Fatalf("readClaude(partial last line) = %d records, end %d, %v; want 1 record, end %d", len(got.recs), got.end, err, len(first))
	}
}

// An over-long line is passed over and counted, not buffered whole.
func TestReadClaudeLongLine(t *testing.T) {
	long := `{"type":"user","message":"` + strings.Repeat("x", maxLine) + `"}` + "\n"
	ok := claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5}`) + "\n"
	got, err := readClaude(strings.NewReader(long+ok), session, true)
	if err != nil || len(got.recs) != 1 || got.long != 1 || got.end != int64(len(long)+len(ok)) {
		t.Fatalf("readClaude(long line) = %d records, %d long, end %d, %v; want 1, 1, %d", len(got.recs), got.long, got.end, err, len(long)+len(ok))
	}
}

// Lines just under and at the bound are read; one byte over is not.
func TestScanLinesBound(t *testing.T) {
	tests := []struct {
		size     int
		lines    int
		long     int
		trailing string
	}{
		{size: 10, lines: 1},
		{size: 10, lines: 0, long: 0, trailing: "partial"},
		{size: 16, lines: 1},
		{size: 17, long: 1},
		{size: 100, long: 1},
	}
	for _, tc := range tests {
		in := strings.Repeat("a", tc.size)
		if tc.trailing == "" {
			in += "\n"
		}
		var seen int
		n, long, err := scanLines(strings.NewReader(in), 16, 16, func(b []byte, at int64) {
			seen++
			if len(b) != tc.size || at != 0 {
				t.Errorf("scanLines(%d bytes) passed a line of %d at %d, want %d at 0", tc.size, len(b), at, tc.size)
			}
		})
		wantN := int64(len(in))
		if tc.trailing != "" {
			wantN = 0
		}
		if err != nil || seen != tc.lines || long != tc.long || n != wantN {
			t.Errorf("scanLines(%d bytes, bound 16) = %d lines, %d long, n %d, %v; want %d, %d, %d", tc.size, seen, long, n, err, tc.lines, tc.long, wantN)
		}
	}
}

// scanLines gives each line's offset, counting lines passed over.
func TestScanLinesOffsets(t *testing.T) {
	in := "ab\n" + strings.Repeat("x", 20) + "\ncde\n\nf\n"
	var got []int64
	if _, _, err := scanLines(strings.NewReader(in), 16, 16, func(_ []byte, at int64) { got = append(got, at) }); err != nil {
		t.Fatal(err)
	}
	if want := []int64{0, 24, 28, 29}; !slices.Equal(got, want) {
		t.Errorf("scanLines(%q) offsets = %v, want %v", in, got, want)
	}
}

// A response's output grows from line to line, so a response is sent
// only once a later line shows it finished: a line of another message,
// or of another type. Until then the read ends before its first line,
// so the next read takes it whole. A final read, at session end, sends
// it as it is.
func TestReadClaudeHoldsOpenResponse(t *testing.T) {
	a1 := claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5,"output_tokens":5}`) + "\n"
	a2 := claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5,"output_tokens":197}`) + "\n"
	b1 := claudeLine("msg_01B", "req_01B", "claude-x", `{"input_tokens":6,"output_tokens":3}`) + "\n"
	user := `{"type":"user","sessionId":"` + session + `","message":{"role":"user","content":"x"}}` + "\n"
	other := strings.Replace(b1, session, "6a7b8c9d-0e1f-4a2b-8c3d-4e5f6a7b8c9d", 1)
	tests := []struct {
		name  string
		in    string
		final bool
		// want is each record as id=output; end is where the read stops.
		want []string
		end  int
	}{
		{name: "open at the end", in: a1 + a2, end: 0},
		{name: "after a finished one", in: user + a1 + b1, want: []string{"msg_01A=5"}, end: len(user + a1)},
		{name: "closed by another message", in: a1 + a2 + b1, want: []string{"msg_01A=197"}, end: len(a1 + a2)},
		{name: "closed by a user line", in: a1 + a2 + user, want: []string{"msg_01A=197"}, end: len(a1 + a2 + user)},
		{name: "closed by another session's line", in: a1 + other, want: []string{"msg_01A=5"}, end: len(a1 + other)},
		{name: "closed by a bad line", in: a1 + "{\"type\":\"assistant\",\n", want: []string{"msg_01A=5"}, end: len(a1) + 21},
		{name: "a partial line does not close it", in: a1 + b1[:10], end: 0},
		{name: "final read sends it", in: a1 + a2, final: true, want: []string{"msg_01A=197"}, end: len(a1 + a2)},
		{name: "final read sends every one", in: a1 + b1, final: true, want: []string{"msg_01A=5", "msg_01B=3"}, end: len(a1 + b1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readClaude(strings.NewReader(tc.in), session, tc.final)
			var recs []string
			for _, r := range got.recs {
				recs = append(recs, fmt.Sprintf("%s=%d", r.RequestID, *r.Output))
			}
			if err != nil || !slices.Equal(recs, tc.want) || got.end != int64(tc.end) {
				t.Errorf("readClaude(final=%v) = %v, end %d, %v; want %v, end %d", tc.final, recs, got.end, err, tc.want, tc.end)
			}
		})
	}
}

func FuzzParseClaudeLine(f *testing.F) {
	f.Add([]byte(claudeLine("msg_01A", "req_01A", "claude-x", `{"input_tokens":5,"cache_creation_input_tokens":3,"cache_creation":{"ephemeral_1h_input_tokens":2}}`)))
	f.Add([]byte(`{"type":"assistant","message":{"usage":null}}`))
	f.Add([]byte(`{"type":"user"}`))
	f.Add([]byte(`not json`))
	f.Fuzz(func(t *testing.T, line []byte) {
		rec, v, version := parseClaudeLine(line, session)
		if v != lineUsage {
			return
		}
		if !proto.UsageRequestID.MatchString(rec.RequestID) || !proto.UsageModel.MatchString(rec.Model) {
			t.Errorf("parseClaudeLine(%q) = record out of shape %s", line, show(rec))
		}
		if h := rec.CacheWrite1h; h != nil && (rec.CacheWrite == nil || *h > *rec.CacheWrite) {
			t.Errorf("parseClaudeLine(%q) = one-hour part not within the cache write: %s", line, show(rec))
		}
		if len(version) > 32 {
			t.Errorf("parseClaudeLine(%q) version %q too long", line, version)
		}
	})
}
