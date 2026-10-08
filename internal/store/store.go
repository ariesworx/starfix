package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
)

// Options configure a Store. The zero value is usable.
type Options struct {
	// Prefix is used for IDs the store generates. Default "sf".
	Prefix string
	// CommitInterval is how often pending writes become a Dolt commit.
	// Default 1s; negative disables the background committer (call Flush).
	CommitInterval time.Duration
	// MaxAttempts bounds retries of a write that loses to a concurrent
	// transaction. Default 5.
	MaxAttempts int
	// Readers caps the read pool. Default 8.
	Readers int
	// Now is the server clock. Default time.Now.
	Now func() time.Time
	// Logger receives background committer errors, and the warning that
	// AllowUnsafeAccount let an unsafe account through. Default discards.
	Logger *slog.Logger
	// Admins are the principals who may change issues others hold and
	// force a close. None may be a [Reserved] name.
	Admins []string
	// AllowUnsafeAccount opens the store even when the database account
	// is root, holds rights beyond its database, or the server leaves
	// secure_file_priv empty. Off, Open refuses such an account with an
	// [*UnsafeAccountError]; starfixd turns it on only for --dev
	// --allow-unsafe-dolt.
	AllowUnsafeAccount bool
	// Limits bound requests and per-principal rows. Zero fields take
	// [DefaultLimits].
	Limits Limits

	// tick, when set by tests, replaces the committer's CommitInterval
	// ticker, so a test decides when each commit happens.
	tick <-chan time.Time
}

// Store is the server-side issue store. It is safe for concurrent use.
// [Open] returns one, and [Store.Close] releases it.
type Store struct {
	// w is the single writer connection; r is the read pool.
	w, r *sql.DB
	opts Options
	// dirty is set by a write that changed rows since the last Dolt
	// commit, and cleared by Flush.
	dirty atomic.Bool

	// stop asks the committer to return; it closes done when it has.
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error

	// watch holds the open inbox watches; publish feeds them after each
	// commit that wrote inbox items.
	watch watchers
	// similar caches closed titles for SimilarClosed (similar.go).
	similar similarCache

	// beforeCommit, when set by tests, runs inside every write transaction
	// just before COMMIT.
	beforeCommit func(context.Context) error
	// wrapRead, when set by tests, wraps the querier beginRead returns, so
	// a test can act between a read's queries.
	wrapRead func(querier) querier
}

// Open connects to the Dolt database named in dsn (go-sql-driver/mysql
// format), checks the account, applies pending migrations and starts the
// committer. ctx bounds only the opening; the caller must Close the
// store.
//
// Open refuses, with [ErrInvalid], a dsn that names no database and an
// invalid prefix, admin or limit in opts. It refuses an unsafe account
// with an [*UnsafeAccountError] unless opts.AllowUnsafeAccount is set,
// and a database a newer binary migrated with [ErrSchemaTooNew].
func Open(ctx context.Context, dsn string, opts Options) (*Store, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.DBName == "" {
		return nil, fmt.Errorf("%w: dsn names no database", ErrInvalid)
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.InterpolateParams = false
	conn, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("connector: %w", err)
	}
	if opts.Prefix == "" {
		opts.Prefix = "sf"
	}
	if !prefixPattern.MatchString(opts.Prefix) {
		return nil, fmt.Errorf("%w: prefix %q", ErrInvalid, opts.Prefix)
	}
	if err := checkAdmins(opts.Admins); err != nil {
		return nil, err
	}
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	opts.Limits = opts.Limits.withDefaults()
	opts.Admins = slices.Clone(opts.Admins)
	if opts.CommitInterval == 0 {
		opts.CommitInterval = time.Second
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 5
	}
	if opts.Readers <= 0 {
		opts.Readers = 8
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	s := &Store{
		w:    sql.OpenDB(conn),
		r:    sql.OpenDB(conn),
		opts: opts,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	// One writer connection: writes are serialized in this process, so the
	// event sequence is gapless without contention (stage 0: one writer
	// ran at 233 req/s, within 15% of the best pool).
	s.w.SetMaxOpenConns(1)
	s.w.SetMaxIdleConns(1)
	s.r.SetMaxOpenConns(opts.Readers)
	s.r.SetMaxIdleConns(opts.Readers)

	if err := checkAccount(ctx, s.w); err != nil {
		var ua *UnsafeAccountError
		if !opts.AllowUnsafeAccount || !errors.As(err, &ua) {
			return nil, errors.Join(err, s.w.Close(), s.r.Close())
		}
		opts.Logger.Warn("dolt account is unsafe; allowed for development", "account", ua.User, "problems", strings.Join(ua.Problems, "; "))
	}
	n, err := migrate(ctx, s.w, s.now())
	if err != nil {
		return nil, errors.Join(err, s.w.Close(), s.r.Close())
	}
	if n > 0 {
		s.dirty.Store(true)
	}
	if opts.CommitInterval > 0 {
		go s.commitLoop(opts.CommitInterval) //nolint:gosec // outlives Open's ctx; stopped by Close
	} else {
		close(s.done)
	}
	return s, nil
}

// Close stops the committer, makes a final Dolt commit of pending writes
// and closes the connections. It is safe to call more than once; every
// call returns the first one's error.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s.closeErr = errors.Join(s.Flush(ctx), s.w.Close(), s.r.Close())
	})
	return s.closeErr
}

// commitLoop flushes every interval, or on each test tick, until Close
// stops it. A failed commit is logged and tried again on the next tick,
// since Flush leaves the writes pending.
func (s *Store) commitLoop(every time.Duration) {
	defer close(s.done)
	tick := s.opts.tick
	if tick == nil {
		t := time.NewTicker(every)
		defer t.Stop()
		tick = t.C
	}
	for {
		select {
		case <-s.stop:
			return
		case <-tick:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.Flush(ctx); err != nil {
				s.opts.Logger.Error("dolt commit failed", "err", err)
			}
			cancel()
		}
	}
}

// Flush makes a Dolt commit now if any write happened since the last one.
// The message names the last event sequence it covers. When the commit
// fails, the writes stay pending for the next Flush.
func (s *Store) Flush(ctx context.Context) error {
	if !s.dirty.Swap(false) {
		return nil
	}
	err := s.commit(ctx)
	if err != nil {
		s.dirty.Store(true)
	}
	return err
}

// commit makes a Dolt commit of the working set, named for the last
// event sequence.
func (s *Store) commit(ctx context.Context) error {
	// Hold the single writer connection so no write lands between reading
	// the sequence and committing.
	c, err := s.w.Conn(ctx)
	if err != nil {
		return fmt.Errorf("dolt commit: %w", err)
	}
	defer func() { _ = c.Close() }()
	seq, err := lastEventSeq(ctx, c)
	if err != nil {
		return fmt.Errorf("dolt commit: %w", err)
	}
	msg := fmt.Sprintf("starfix: events through %d", seq)
	if _, err := c.ExecContext(ctx, `CALL DOLT_COMMIT('-Am', ?, '--skip-empty')`, msg); err != nil {
		return fmt.Errorf("dolt commit: %w", err)
	}
	return nil
}

// Now reads the server clock (Options.Now), in UTC to the microsecond.
func (s *Store) Now() time.Time { return s.now() }

func (s *Store) now() time.Time {
	return s.opts.Now().UTC().Truncate(time.Microsecond)
}

// beginRead starts a read of several queries on one read-only
// transaction, so that they see one snapshot: a write that commits while
// the read runs shows in none of them. It returns the transaction to run
// the queries on and a func that ends it, to call once they are done.
func (s *Store) beginRead(ctx context.Context) (querier, func(), error) {
	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, nil, fmt.Errorf("begin read: %w", err)
	}
	var q querier = tx
	if s.wrapRead != nil {
		q = s.wrapRead(q)
	}
	return q, func() { _ = tx.Rollback() }, nil // read-only: nothing to keep
}

// querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// wtx is one attempt at a write transaction. Every attempt gets a fresh
// one, so none of a failed attempt's events, inbox items or idempotency
// stamp carries into the rerun.
//
// The transaction's connection serves one result set at a time: read
// rows to the end and close them before running the next statement.
type wtx struct {
	tx    *sql.Tx
	actor Actor
	// admin is set when actor's principal is one of the store's admins.
	admin bool
	// now is the attempt's server time, used for every timestamp it
	// writes.
	now time.Time
	// mutated is set once a statement changed a row; writeOnce then
	// requires an event unless quiet is set.
	mutated bool
	events  int
	// quiet allows a mutation with no event, for bookkeeping that is not
	// history: a lease renewal, a registry touch, an inbox ack, a prune.
	quiet bool
	// inbox are the items this attempt wrote, pushed once it commits.
	inbox []InboxItem
	// pending are the events recorded, written by flush.
	pending []pendingEvent
	// idem is the operation's idempotency stamp, when it has a key.
	idem *idemStamp
	// lim are the store's limits.
	lim Limits
	// closedChanged is set when the attempt closed or reopened an issue,
	// or changed a closed one, so the similar-title cache is stale once
	// it commits.
	closedChanged bool
}

// exec runs a mutating statement. It refuses an UPDATE that does not set
// write_id: without it Dolt merges concurrent writes silently (stage 0).
func (w *wtx) exec(ctx context.Context, q string, args ...any) (int64, error) {
	if strings.HasPrefix(strings.TrimSpace(q), "UPDATE") && !strings.Contains(q, "write_id = ?") {
		return 0, errors.New("store: UPDATE without write_id")
	}
	res, err := w.tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n > 0 {
		w.mutated = true
	}
	return n, nil
}

// event records an event for op on target, with the before and after
// states as JSON (nil is NULL). Every change that is history records at
// least one. Events are buffered and written in order, with the next
// gapless sequence numbers, once the operation's closure returns (flush),
// so the last can carry the operation's idempotency stamp and result.
func (w *wtx) event(_ context.Context, op Op, target string, before, after any) error {
	b, err := jsonOrNull(before)
	if err != nil {
		return err
	}
	a, err := jsonOrNull(after)
	if err != nil {
		return err
	}
	w.pending = append(w.pending, pendingEvent{op: op, target: target, before: b, after: a})
	w.events++
	return nil
}

// pendingEvent is an event recorded but not yet written.
type pendingEvent struct {
	op            Op
	target        string
	before, after any
}

// flush writes the buffered events with the next sequence numbers. The
// last carries the idempotency stamp, if the operation has one.
func (w *wtx) flush(ctx context.Context) error {
	if len(w.pending) == 0 {
		if w.idem != nil && w.idem.result != nil {
			return errors.New("store: idempotent operation recorded no event")
		}
		return nil
	}
	seq, err := lastEventSeq(ctx, w.tx)
	if err != nil {
		return fmt.Errorf("next event seq: %w", err)
	}
	for i, e := range w.pending {
		seq++
		var key, args, result any
		if st := w.idem; st != nil && i == len(w.pending)-1 {
			if st.result == nil {
				return errors.New("store: idempotent operation set no result")
			}
			key, args, result = st.key, st.args, string(st.result)
		}
		if _, err := w.exec(ctx, `INSERT INTO events
  (seq, at, principal, session, machine, op, target, before_state, after_state, idem_key, idem_args, idem_result)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			seq, w.now, w.actor.Principal, w.actor.Session, w.actor.Machine, string(e.op), e.target, e.before, e.after,
			key, args, result); err != nil {
			if key != nil && isDuplicate(err) {
				// A concurrent request with the same key committed first;
				// the rerun replays it.
				return fmt.Errorf("%w: idempotency key %q: %w", errRetry, w.idem.key, err)
			}
			return fmt.Errorf("insert event: %w", err)
		}
	}
	w.pending = nil
	return nil
}

// lastEventSeq returns the highest event seq, or 0. It avoids MAX(): Dolt
// 2.4.2 returns no row at all for MAX over an empty table's primary key.
func lastEventSeq(ctx context.Context, q querier) (int64, error) {
	return lastKey(ctx, q, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1`)
}

// lastKey runs query, which selects one integer in descending order with
// LIMIT 1, and returns it, or 0 when the table is empty.
func lastKey(ctx context.Context, q querier, query string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, query).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// jsonOrNull encodes an event state as compacted JSON text, or returns
// nil, which is NULL, for a nil state.
func jsonOrNull(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode event state: %w", err)
	}
	if b, err = compactState(b); err != nil {
		return nil, err
	}
	return string(b), nil
}

// Event text bounds (S-10). An event keeps the before and after of what
// changed, and Dolt keeps every version, so without a bound an edit loop
// over four 64 KiB fields would grow the log by 256 KiB a write. A string
// longer than EventTextMax is kept as its first EventTextKeep bytes, its
// length and its SHA-256: enough to tell what changed and to check a
// copy, while the text itself lives in the issue or comment row.
const (
	EventTextMax  = 8 << 10
	EventTextKeep = 512
)

// compactState shortens every string in the JSON document b longer than
// EventTextMax, keeping it a string so readers decode it as before.
func compactState(b []byte) ([]byte, error) {
	if len(b) <= EventTextMax {
		return b, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("compact event state: %w", err)
	}
	changed := false
	var walk func(any) any
	walk = func(v any) any {
		switch x := v.(type) {
		case string:
			if len(x) > EventTextMax {
				changed = true
				return elide(x)
			}
		case map[string]any:
			for k, e := range x {
				x[k] = walk(e)
			}
		case []any:
			for i, e := range x {
				x[i] = walk(e)
			}
		}
		return v
	}
	v = walk(v)
	if !changed {
		return b, nil
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("compact event state: %w", err)
	}
	return out, nil
}

// elide is s's first EventTextKeep bytes, on a rune boundary, then its
// length and SHA-256.
func elide(s string) string {
	n := EventTextKeep
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%s… [%d bytes, sha256:%s]", s[:n], len(s), hex.EncodeToString(sum[:]))
}

// write runs fn in a transaction on the writer connection as actor,
// retrying when a concurrent transaction wins (see retryable). It
// refuses an invalid actor with ErrInvalid before trying. fn must be safe
// to rerun: each attempt starts from a fresh wtx, and fn must set every
// result it hands back afresh. Between attempts write waits a random,
// growing backoff; when ctx ends first, or attempts reach
// Options.MaxAttempts, it gives up, the latter with ErrConflict.
func (s *Store) write(ctx context.Context, actor Actor, fn func(*wtx) error) error {
	if err := actor.validate(); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		err := s.writeOnce(ctx, actor, fn)
		if err == nil {
			return nil
		}
		if !retryable(err) {
			return err
		}
		if attempt >= s.opts.MaxAttempts {
			return fmt.Errorf("%w: gave up after %d attempts: %w", ErrConflict, attempt, err)
		}
		d := time.Duration(rand.Int64N(int64(5*time.Millisecond) << min(attempt, 5))) //nolint:gosec // jitter, not security
		select {
		case <-ctx.Done():
			return fmt.Errorf("write retry: %w", ctx.Err())
		case <-time.After(d):
		}
	}
}

// writeOnce makes one attempt: it runs fn in a new transaction, refuses a
// mutation that recorded no event unless the attempt is quiet, writes the
// buffered events and commits. Only once the commit succeeds does it mark
// the store dirty, invalidate the similar-title cache and push inbox
// items, so a rolled-back attempt leaves no trace.
func (s *Store) writeOnce(ctx context.Context, actor Actor, fn func(*wtx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	w := &wtx{tx: tx, actor: actor, admin: s.IsAdmin(actor.Principal), now: s.now(), lim: s.opts.Limits}
	if err := fn(w); err != nil {
		return errors.Join(err, rollback(tx))
	}
	if w.mutated && w.events == 0 && !w.quiet {
		return errors.Join(errors.New("store: mutation without an event"), rollback(tx))
	}
	if err := w.flush(ctx); err != nil {
		return errors.Join(err, rollback(tx))
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(ctx); err != nil {
			return errors.Join(err, rollback(tx))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	if w.mutated {
		s.dirty.Store(true)
	}
	if w.closedChanged {
		s.similar.invalidate()
	}
	if len(w.inbox) > 0 {
		s.publish(w.inbox)
	}
	return nil
}

// rollback rolls tx back; a transaction already done is not an error.
func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}
