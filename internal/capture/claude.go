package capture

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Claude Code writes one transcript per session, as JSON lines, and one
// per subagent beside it, in <dir>/<session>/subagents/agent-*.jsonl.
// Each API response appears once per content block, so on several lines,
// all carrying the response's usage; message.id and requestId together
// name the response. The format is internal to Claude Code and changes
// between versions: what is read here was checked against version 2.1.x
// on 8 Oct 2026.

// Harness is the harness name records carry: `sfx setup`'s agent name.
const Harness = "claude-code"

// maxLine bounds a transcript line. Assistant lines are far shorter; a
// longer line, such as a large tool result, is passed over.
const maxLine = 4 << 20

// syntheticModel is the model of messages Claude Code makes up itself,
// such as an API error shown as a reply. No request was billed.
const syntheticModel = "<synthetic>"

// versionShape is a Claude Code version as a note may repeat it.
var versionShape = regexp.MustCompile(`^[0-9A-Za-z.+-]{1,32}$`)

// claudeEntry is the part of a transcript line usage needs. Decoding into
// it skips the conversation text, which is never held past the line.
type claudeEntry struct {
	Type      string    `json:"type"`
	SessionID string    `json:"sessionId"`
	RequestID string    `json:"requestId"`
	Timestamp time.Time `json:"timestamp"`
	Version   string    `json:"version"`
	Message   *struct {
		ID    string       `json:"id"`
		Model string       `json:"model"`
		Usage *claudeUsage `json:"usage"`
	} `json:"message"`
}

// claudeUsage is a response's usage. Its other fields are left: thinking
// tokens are already in output_tokens, and iterations are already summed
// in the top level.
type claudeUsage struct {
	Input         *int64 `json:"input_tokens"`
	Output        *int64 `json:"output_tokens"`
	CacheWrite    *int64 `json:"cache_creation_input_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreation *struct {
		OneHour *int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// verdict is what one line held.
type verdict int

const (
	// lineOther is a line of another kind, or another session's.
	lineOther verdict = iota
	// lineUsage is a response's usage.
	lineUsage
	// lineBad is a response line whose usage could not be read.
	lineBad
)

// assistantType finds an assistant line that does not decode.
var assistantType = []byte(`"type":"assistant"`)

// parseClaudeLine reads one transcript line of session. A line of
// another type, of another session (history copied into a forked
// session's file), or from a synthetic message is lineOther. An assistant
// line whose usage cannot be read in full, or that falls outside what the
// server accepts, is lineBad, with the Claude Code version the line names
// if it looks like one. Counts are never guessed: a missing count is
// unknown.
func parseClaudeLine(line []byte, session string) (proto.UsageRecord, verdict, string) {
	var e claudeEntry
	err := json.Unmarshal(line, &e)
	if _, typeErr := errors.AsType[*json.UnmarshalTypeError](err); err != nil && !typeErr {
		if bytes.Contains(line, assistantType) {
			return proto.UsageRecord{}, lineBad, ""
		}
		return proto.UsageRecord{}, lineOther, ""
	}
	// On a type error, the fields that did decode are still set.
	if e.Type != "assistant" || (e.SessionID != "" && e.SessionID != session) {
		return proto.UsageRecord{}, lineOther, ""
	}
	version := ""
	if versionShape.MatchString(e.Version) {
		version = e.Version
	}
	m := e.Message
	if err == nil && m != nil && m.Model == syntheticModel {
		return proto.UsageRecord{}, lineOther, ""
	}
	if err != nil || m == nil || m.Usage == nil || e.SessionID == "" {
		return proto.UsageRecord{}, lineBad, version
	}
	u := m.Usage
	rec := proto.UsageRecord{Harness: Harness, RequestID: m.ID + ":" + e.RequestID, Model: m.Model,
		At: e.Timestamp.UTC(), Granularity: "request",
		Tokens: proto.Tokens{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead}}
	if c := u.CacheCreation; c != nil && c.OneHour != nil && u.CacheWrite != nil && *c.OneHour <= *u.CacheWrite {
		rec.CacheWrite1h = c.OneHour
	}
	if m.ID == "" || e.RequestID == "" || !valid(rec) {
		return proto.UsageRecord{}, lineBad, version
	}
	return rec, lineUsage, version
}

// Bounds the server holds a record to (store.UsageRecord.validate); a
// record outside them would refuse its whole batch.
var (
	earliest = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	maxCount = int64(1_000_000_000_000)
)

// valid reports whether the server would accept rec, as far as the
// client can tell: it cannot know the server's clock.
func valid(rec proto.UsageRecord) bool {
	if !proto.UsageRequestID.MatchString(rec.RequestID) || !proto.UsageModel.MatchString(rec.Model) ||
		rec.At.Before(earliest) {
		return false
	}
	known := false
	for _, c := range []*int64{rec.Input, rec.Output, rec.CacheWrite, rec.CacheWrite1h, rec.CacheRead} {
		if c == nil {
			continue
		}
		if *c < 0 || *c > maxCount {
			return false
		}
		known = true
	}
	return known
}

// fileRead is what one read of a transcript found.
type fileRead struct {
	// recs are the responses, one record each, in the order first seen.
	recs []proto.UsageRecord
	// end is the offset, from where the read began, just past the last
	// complete line.
	end int64
	// bad counts response lines that could not be read; long, lines
	// passed over for their length.
	bad, long int
	// version is the Claude Code version a bad line named, if any.
	version string
}

// readClaude reads session's responses from r up to its last complete
// line. A response on several lines is one record: the one with the
// largest output count, since output can grow from one content block's
// line to the next.
func readClaude(r io.Reader, session string) (fileRead, error) {
	var out fileRead
	seen := map[string]int{}
	end, long, err := scanLines(r, maxLine, 64<<10, func(line []byte) {
		rec, v, version := parseClaudeLine(line, session)
		switch v {
		case lineBad:
			out.bad++
			if version != "" {
				out.version = version
			}
		case lineUsage:
			i, ok := seen[rec.RequestID]
			switch {
			case !ok:
				seen[rec.RequestID] = len(out.recs)
				out.recs = append(out.recs, rec)
			case more(rec.Output, out.recs[i].Output):
				out.recs[i] = rec
			}
		}
	})
	out.end, out.long = end, long
	return out, err
}

// more reports whether count a is known and larger than b.
func more(a, b *int64) bool {
	return a != nil && (b == nil || *a > *b)
}

// scanLines calls f with each complete line of r, without its newline,
// and returns the bytes those lines took. The slice f gets is valid only
// until it returns. A line longer than limit is passed over, never held
// whole, and counted in long. A last line without a newline is left
// unread: the harness may still be writing it. size is the read buffer's.
func scanLines(r io.Reader, limit, size int, f func([]byte)) (n int64, long int, err error) {
	br := bufio.NewReaderSize(r, size)
	var buf []byte // a line longer than the read buffer, so far
	var part int64 // bytes of the current line read so far
	over := false  // the current line is past limit
	for {
		chunk, err := br.ReadSlice('\n')
		part += int64(len(chunk))
		full := err == nil
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			return n, long, err
		}
		if full {
			chunk = chunk[:len(chunk)-1]
		}
		if !over && len(buf)+len(chunk) > limit {
			over, buf = true, buf[:0]
		}
		if !full {
			if !over {
				buf = append(buf, chunk...)
			}
			continue
		}
		switch {
		case over:
			long++
		case len(buf) > 0:
			f(append(buf, chunk...))
		default:
			f(chunk)
		}
		n, part, buf, over = n+part, 0, buf[:0], false
	}
}

// ClaudeFiles lists a session's transcripts: transcript itself, its
// subagents' in <dir>/<session>/subagents/agent-*.jsonl, and agent (the
// agent_transcript_path a SubagentStop hook gives) when it is not among
// them. A path that is not absolute is left out, and so are the subagents
// when session cannot be a file name. A missing subagent directory is no
// error.
func ClaudeFiles(transcript, session, agent string) ([]string, error) {
	if !filepath.IsAbs(transcript) {
		return nil, nil
	}
	files := []string{filepath.Clean(transcript)}
	if session != "" && filepath.IsLocal(session) && !strings.ContainsAny(session, `/\`) {
		dir := filepath.Join(filepath.Dir(transcript), session, "subagents")
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return files, err
		}
		for _, e := range entries {
			if name := e.Name(); e.Type().IsRegular() && strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl") {
				files = append(files, filepath.Join(dir, name))
			}
		}
	}
	if agent != "" && filepath.IsAbs(agent) && strings.HasSuffix(agent, ".jsonl") {
		if agent = filepath.Clean(agent); !slices.Contains(files, agent) {
			files = append(files, agent)
		}
	}
	return files, nil
}
