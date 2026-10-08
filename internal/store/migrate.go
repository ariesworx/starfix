package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migration is one migrations/NNNN_name.sql file: its version (NNNN),
// its file name without .sql, and its statements.
type migration struct {
	version int
	name    string
	stmts   []string
}

// loadMigrations parses migrations/NNNN_name.sql in version order.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	var ms []migration
	for _, n := range names {
		base := path.Base(n)
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v < 1 {
			return nil, fmt.Errorf("migration %s: name must be NNNN_name.sql", base)
		}
		b, err := fs.ReadFile(fsys, n)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", base, err)
		}
		ms = append(ms, migration{version: v, name: strings.TrimSuffix(base, ".sql"), stmts: splitSQL(string(b))})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migration %s: versions must run 1..n without gaps", m.name)
		}
	}
	return ms, nil
}

// splitSQL drops "--" comment lines and splits on semicolons that end a line.
func splitSQL(src string) []string {
	var stmts []string
	var cur strings.Builder
	for line := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteByte('\n')
		if strings.HasSuffix(t, ";") {
			stmts = append(stmts, strings.TrimSuffix(strings.TrimSpace(cur.String()), ";"))
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

// migrate applies pending migrations and returns how many ran. DDL is not
// transactional, so a migration that fails halfway needs repair by hand;
// each is small and runs once.
func migrate(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	ms, err := loadMigrations(migrationFiles)
	if err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INT          NOT NULL PRIMARY KEY,
  name       VARCHAR(255) NOT NULL,
  applied_at DATETIME(6)  NOT NULL)`); err != nil {
		return 0, fmt.Errorf("create schema_migrations: %w", err)
	}
	cur, err := lastKey(ctx, db, `SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if cur > int64(len(ms)) {
		return 0, fmt.Errorf("%w: database at version %d, binary knows %d", ErrSchemaTooNew, cur, len(ms))
	}
	n := 0
	for _, m := range ms[cur:] { //nolint:gosec // 0 <= cur <= len(ms)
		for _, s := range m.stmts {
			if _, err := db.ExecContext(ctx, s); err != nil {
				return n, fmt.Errorf("migration %s: %w", m.name, err)
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, now); err != nil {
			return n, fmt.Errorf("record migration %s: %w", m.name, err)
		}
		n++
	}
	return n, nil
}
