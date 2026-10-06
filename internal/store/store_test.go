package store

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func doltCommits(t *testing.T, s *Store) (int, string) {
	t.Helper()
	var n int
	var msg string
	if err := s.r.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM dolt_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := s.r.QueryRowContext(t.Context(), `SELECT message FROM dolt_log LIMIT 1`).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	return n, msg
}

func TestBatchedCommits(t *testing.T) {
	s := openStore(t, newDSN(t), Options{CommitInterval: 50 * time.Millisecond})
	// waitFor polls dolt_log until the newest commit message is want.
	waitFor := func(want string) int {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			n, msg := doltCommits(t, s)
			if msg == want {
				return n
			}
			if time.Now().After(deadline) {
				t.Fatalf("newest commit %q, want %q", msg, want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// The migration is committed by the first tick.
	base := waitFor("starfix: events through 0")

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { mustCreate(t, s, NewIssue{}) })
	}
	wg.Wait()
	n := waitFor("starfix: events through 20")
	if n-base > 5 {
		t.Errorf("%d commits for 20 writes; commits are not batched", n-base)
	}
	// Idle ticks add nothing.
	time.Sleep(300 * time.Millisecond)
	if idle, _ := doltCommits(t, s); idle != n {
		t.Errorf("idle committer made %d empty commits", idle-n)
	}
	if err := s.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMigrations(t *testing.T) {
	dsn := newDSN(t)
	s := openStore(t, dsn, Options{})
	if n, err := migrate(t.Context(), s.w, s.now()); err != nil || n != 0 {
		t.Errorf("second migrate ran %d (err %v), want 0", n, err)
	}
	if _, err := s.w.ExecContext(t.Context(), `INSERT INTO schema_migrations VALUES (99, 'future', NOW())`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), dsn, Options{CommitInterval: -1}); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("open newer schema: %v, want ErrSchemaTooNew", err)
	}
}

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations(migrationFiles)
	if err != nil || len(ms) == 0 || ms[0].version != 1 || len(ms[0].stmts) != 6 {
		t.Fatalf("embedded migrations: %v %+v", err, ms)
	}
	bad := []struct {
		name string
		fs   fstest.MapFS
	}{
		{"gap", fstest.MapFS{"migrations/0001_a.sql": {}, "migrations/0003_c.sql": {}}},
		{"bad name", fstest.MapFS{"migrations/init.sql": {}}},
		{"zero", fstest.MapFS{"migrations/0000_a.sql": {}}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadMigrations(tc.fs); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestSplitSQL(t *testing.T) {
	got := splitSQL("-- comment\nCREATE TABLE a (\n  x INT\n);\n\nCREATE TABLE b (y INT);\nSELECT 1")
	if len(got) != 3 || !strings.HasPrefix(got[0], "CREATE TABLE a") || got[2] != "SELECT 1" {
		t.Errorf("splitSQL = %q", got)
	}
}

// TestEveryUpdateSetsWriteID scans the package source: every UPDATE must
// stamp write_id, or Dolt merges concurrent writes silently (stage 0).
func TestEveryUpdateSetsWriteID(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || !strings.Contains(lit.Value, "UPDATE ") {
				return true
			}
			found++
			if !strings.Contains(lit.Value, "write_id") {
				t.Errorf("%s: UPDATE without write_id", fset.Position(lit.Pos()))
			}
			return true
		})
	}
	if found == 0 {
		t.Error("found no UPDATE statements; the scan is broken")
	}
}
