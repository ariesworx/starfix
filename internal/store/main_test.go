package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
)

var (
	server    *dolttest.Server
	serverErr error
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "starfix-dolt-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	server, serverErr = dolttest.Start(ctx, dir)
	cancel()
	if server != nil {
		defer func() { _ = server.Stop() }()
	}
	return m.Run()
}

// newDSN returns a fresh database, or skips when dolt is missing.
func newDSN(t *testing.T) string {
	t.Helper()
	if errors.Is(serverErr, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the store integration tests")
	}
	if serverErr != nil {
		t.Fatalf("dolt sql-server: %v", serverErr)
	}
	dsn, err := server.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

func openStore(t *testing.T, dsn string, opts Options) *Store {
	t.Helper()
	if opts.CommitInterval == 0 {
		opts.CommitInterval = -1
	}
	if opts.Prefix == "" {
		opts.Prefix = "tst"
	}
	s, err := Open(t.Context(), dsn, opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

func newStore(t *testing.T) *Store {
	t.Helper()
	return openStore(t, newDSN(t), Options{})
}

var (
	alice = Actor{Principal: "alice", Session: "sess-a", Machine: "laptop-a"}
	bob   = Actor{Principal: "bob", Session: "sess-b", Machine: "laptop-b"}
)

func mustCreate(t *testing.T, s *Store, in NewIssue) Issue {
	t.Helper()
	if in.Title == "" {
		in.Title = "issue"
	}
	is, err := s.CreateIssue(t.Context(), alice, in)
	if err != nil {
		t.Fatalf("create %q: %v", in.Title, err)
	}
	return is
}

func prio(p Priority) *Priority { return &p }
func ptr[T any](v T) *T         { return &v }

func lastSeq(t *testing.T, s *Store) int64 {
	t.Helper()
	n, err := lastEventSeq(t.Context(), s.r)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// barrier holds the first k write transactions that reach it until all k
// have, so they overlap. Later transactions (retries) pass straight through.
type barrier struct {
	k       int32
	arrived atomic.Int32
	wg      sync.WaitGroup
}

func newBarrier(k int) *barrier {
	b := &barrier{k: int32(k)} //nolint:gosec // small test constant
	b.wg.Add(k)
	return b
}

func (b *barrier) hook(context.Context) error {
	if b.arrived.Add(1) > b.k {
		return nil
	}
	b.wg.Done()
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("barrier: the other transaction never arrived")
	}
}
