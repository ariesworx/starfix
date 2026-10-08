package capture

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"

	"github.com/ariesworx/starfix/internal/proto"
)

// MaxBatch is the most records one usage call sends: the server's
// default usage_records limit.
const MaxBatch = 500

// StateFile is the offsets file in the state directory.
const StateFile = "usage-offsets.json"

// headSize is how much of a file's start identifies it. A transcript's
// first line names its session and time, so another file does not share
// it.
const headSize = 4096

// Input is one capture run: the transcripts to read and where to send
// what they hold.
type Input struct {
	// Session is the harness's session id, as the hook input gives it.
	// Lines of other sessions are not read as this one's.
	Session string
	// Files are the session's transcripts, as absolute paths.
	Files []string
	// StateDir holds the offsets file. It is made, mode 0700, if missing.
	StateDir string
	// Send sends one batch of at most MaxBatch records; it returns once
	// the server has accepted them.
	Send func(context.Context, []proto.UsageRecord) error
	// Final says the session has stopped writing (SessionEnd).
	Final bool
}

// Result is what a run did.
type Result struct {
	// Sent counts the records the server accepted.
	Sent int
	// Skipped counts lines passed over: response lines that could not be
	// read, and lines too long to read.
	Skipped int
	// Unrecognized says a transcript had response lines but none could
	// be read: its format is not one sfx knows. Version is the Claude
	// Code version such a line named, if any.
	Unrecognized bool
	Version      string
}

// Claude sends what a Claude Code session's transcripts hold that earlier
// runs did not send. Each file is read from the offset the state file
// keeps for it, up to its last complete line, or unless in.Final up to
// its last response, which may still be growing (readClaude). The offset
// always lies on a line boundary, and it moves only
// once the server has accepted every record read from the file, so a
// failed send is retried by the next run. The server keeps one record per
// request, so a record sent twice, by runs at once or after a lost state
// file, is counted once. A file truncated or replaced since its offset
// was kept is read from its start. A file in a format sfx does not know
// keeps its offset, so a later sfx can read what it holds.
//
// Errors reading one file do not stop the others; all are returned,
// joined, with the send's.
func Claude(ctx context.Context, in Input) (Result, error) {
	var res Result
	var errs []error
	marks := loadState(in.StateDir)
	// pending is a file read: the mark to keep once batches up to last
	// are accepted; last is -1 when it gave no records.
	type pending struct {
		path string
		mark mark
		last int
	}
	var done []pending
	var recs []proto.UsageRecord
	for _, path := range in.Files {
		fr, m, err := readFile(path, marks[path], in.Session, in.Final)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		res.Skipped += fr.bad + fr.long
		if fr.good == 0 && fr.bad > 0 {
			res.Unrecognized = true
			res.Version = cmp.Or(res.Version, fr.version)
			continue
		}
		recs = append(recs, fr.recs...)
		last := -1
		if len(fr.recs) > 0 {
			last = (len(recs) - 1) / MaxBatch
		}
		done = append(done, pending{path, m, last})
	}
	accepted := 0
	for i := 0; i < len(recs); i += MaxBatch {
		batch := recs[i:min(i+MaxBatch, len(recs))]
		if err := in.Send(ctx, batch); err != nil {
			errs = append(errs, err)
			break
		}
		res.Sent += len(batch)
		accepted++
	}
	keep := map[string]mark{}
	for _, d := range done {
		if d.last < accepted && d.mark != marks[d.path] {
			keep[d.path] = d.mark
		}
	}
	if err := saveState(in.StateDir, keep); err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

// mark is where the last run left a file: Offset, just past the last
// line it sent, and Head, the SHA-256 of the file's first
// min(Offset, headSize) bytes, which a file truncated and rewritten, or
// replaced, no longer matches. A file shorter than Offset was truncated.
type mark struct {
	Offset int64  `json:"offset"`
	Head   string `json:"head"`
}

// readFile reads path from where m left it, up to the file's size as
// opened, and returns what it found and the mark to keep once that is
// sent.
func readFile(path string, m mark, session string, final bool) (fileRead, mark, error) {
	f, err := os.Open(path) //nolint:gosec // a transcript the harness named
	if err != nil {
		return fileRead{}, mark{}, fmt.Errorf("read transcript %s: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read only
	st, err := f.Stat()
	if err != nil {
		return fileRead{}, mark{}, fmt.Errorf("read transcript %s: %w", path, err)
	}
	start := int64(0)
	if m.Offset > 0 && m.Offset <= st.Size() {
		if head, err := headHash(f, m.Offset); err == nil && head == m.Head {
			start = m.Offset
		}
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return fileRead{}, mark{}, fmt.Errorf("read transcript %s: %w", path, err)
	}
	fr, err := readClaude(io.LimitReader(f, st.Size()-start), session, final)
	if err != nil {
		return fileRead{}, mark{}, fmt.Errorf("read transcript %s: %w", path, err)
	}
	end := start + fr.end
	head, err := headHash(f, end)
	if err != nil {
		return fileRead{}, mark{}, fmt.Errorf("read transcript %s: %w", path, err)
	}
	return fr, mark{Offset: end, Head: head}, nil
}

// headHash is the hex SHA-256 of f's first min(offset, headSize) bytes.
func headHash(f io.ReaderAt, offset int64) (string, error) {
	b := make([]byte, min(offset, headSize))
	if _, err := f.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// stateDoc is the offsets file: a mark per transcript, by absolute path.
type stateDoc struct {
	Files map[string]mark `json:"files"`
}

// loadState reads the offsets in dir. A missing or unreadable file is
// no offsets: every transcript is read from its start, and the server
// drops what it already has.
func loadState(dir string) map[string]mark {
	var doc stateDoc
	b, err := os.ReadFile(filepath.Join(dir, StateFile)) //nolint:gosec // sfx's own state file
	if err != nil || json.Unmarshal(b, &doc) != nil || doc.Files == nil {
		return map[string]mark{}
	}
	return doc.Files
}

// saveState merges keep into the offsets in dir, as they are now, drops
// the files that no longer exist, and writes the result to a temporary
// file renamed into place, so runs at once each leave a whole file. The
// last to rename wins; a mark another run kept and this one lost costs a
// resend, which the server drops.
func saveState(dir string, keep map[string]mark) error {
	if len(keep) == 0 {
		return nil
	}
	fail := func(err error) error { return fmt.Errorf("save usage offsets in %s: %w", dir, err) }
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fail(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
		return fail(err)
	}
	marks := loadState(dir)
	maps.Copy(marks, keep)
	for path := range marks {
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			delete(marks, path)
		}
	}
	b, err := json.Marshal(stateDoc{Files: marks})
	if err != nil {
		return fail(err)
	}
	f, err := os.CreateTemp(dir, "."+StateFile+".tmp-*") // mode 0600
	if err != nil {
		return fail(err)
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	if err := errors.Join(werr, f.Close()); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, StateFile)); err != nil {
		_ = os.Remove(tmp)
		return fail(err)
	}
	return nil
}
