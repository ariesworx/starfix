package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
	// Logger receives background committer errors. Default discards.
	Logger *slog.Logger
}

// Store is the server-side issue store. It is safe for concurrent use.
type Store struct {
	w, r  *sql.DB
	opts  Options
	dirty atomic.Bool

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error

	// beforeCommit, when set by tests, runs inside every write transaction
	// just before COMMIT.
	beforeCommit func(context.Context) error
}

// Open connects to the Dolt database named in dsn (go-sql-driver/mysql
// format), applies pending migrations and starts the committer.
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
// and closes the connections.
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

func (s *Store) commitLoop(every time.Duration) {
	defer close(s.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := s.Flush(ctx); err != nil {
				s.opts.Logger.Error("dolt commit failed", "err", err)
			}
			cancel()
		}
	}
}

// Flush makes a Dolt commit now if any write happened since the last one.
// The message names the last event sequence it covers.
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

func (s *Store) now() time.Time {
	return s.opts.Now().UTC().Truncate(time.Microsecond)
}

// querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// wtx is one attempt at a write transaction.
type wtx struct {
	tx      *sql.Tx
	actor   Actor
	now     time.Time
	mutated bool
	events  int
	// quiet allows a mutation with no event: a lease renewal.
	quiet bool
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

// event appends the next gapless event. Every mutation calls it once.
func (w *wtx) event(ctx context.Context, op Op, target string, before, after any, idemKey string) error {
	b, err := jsonOrNull(before)
	if err != nil {
		return err
	}
	a, err := jsonOrNull(after)
	if err != nil {
		return err
	}
	seq, err := lastEventSeq(ctx, w.tx)
	if err != nil {
		return fmt.Errorf("next event seq: %w", err)
	}
	seq++
	var idem any
	if idemKey != "" {
		idem = idemKey
	}
	if _, err := w.exec(ctx, `INSERT INTO events
  (seq, at, principal, session, machine, op, target, before_state, after_state, idem_key)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, w.now, w.actor.Principal, w.actor.Session, w.actor.Machine, string(op), target, b, a, idem); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	w.events++
	return nil
}

// lastEventSeq returns the highest event seq, or 0. It avoids MAX(): Dolt
// 2.4.2 returns no row at all for MAX over an empty table's primary key.
func lastEventSeq(ctx context.Context, q querier) (int64, error) {
	return lastKey(ctx, q, `SELECT seq FROM events ORDER BY seq DESC LIMIT 1`)
}

func lastKey(ctx context.Context, q querier, query string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, query).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

func jsonOrNull(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode event state: %w", err)
	}
	return string(b), nil
}

// write runs fn in a transaction on the writer connection, retrying when a
// concurrent transaction wins (Dolt error 1213). fn must be safe to rerun.
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

func (s *Store) writeOnce(ctx context.Context, actor Actor, fn func(*wtx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	w := &wtx{tx: tx, actor: actor, now: s.now()}
	if err := fn(w); err != nil {
		return errors.Join(err, rollback(tx))
	}
	if w.mutated && w.events == 0 && !w.quiet {
		return errors.Join(errors.New("store: mutation without an event"), rollback(tx))
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
	return nil
}

func rollback(tx *sql.Tx) error {
	if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}
