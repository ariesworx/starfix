package store

import (
	"strings"
	"testing"
	"time"
)

func TestBackupTag(t *testing.T) {
	dsn := newDSN(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s := openStore(t, dsn, Options{Now: func() time.Time { return now }})
	ctx := t.Context()

	// An uncommitted write is covered by the backup.
	if _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "before upgrade"}); err != nil {
		t.Fatal(err)
	}
	name, err := s.BackupTag(ctx, "starfix-v0.2.0")
	if err != nil || name != "starfix-v0.2.0" {
		t.Fatalf("BackupTag = %q, %v", name, err)
	}
	var n int
	if err := s.r.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues AS OF 'starfix-v0.2.0'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("issues at the tag: %d, %v", n, err)
	}

	// Again with nothing new: the same tag, nothing made.
	if name, err := s.BackupTag(ctx, "starfix-v0.2.0"); err != nil || name != "starfix-v0.2.0" {
		t.Fatalf("second BackupTag = %q, %v", name, err)
	}
	if got := tags(t, s); got != "starfix-v0.2.0" {
		t.Fatalf("tags after a repeat: %s", got)
	}

	// After more writes the old restore point is kept beside a new one.
	if _, err := s.CreateIssue(ctx, alice, NewIssue{Title: "after rollback"}); err != nil {
		t.Fatal(err)
	}
	name, err = s.BackupTag(ctx, "starfix-v0.2.0")
	if err != nil || name != "starfix-v0.2.0-20261007t120000z" {
		t.Fatalf("BackupTag after writes = %q, %v", name, err)
	}
	if got := tags(t, s); got != "starfix-v0.2.0,starfix-v0.2.0-20261007t120000z" {
		t.Fatalf("tags: %s", got)
	}

	for _, bad := range []string{"", "-v1", "Starfix", "a b", "x'; DROP TABLE issues; --"} {
		if _, err := s.BackupTag(ctx, bad); err == nil {
			t.Errorf("BackupTag(%q) accepted", bad)
		}
	}
}

func tags(t *testing.T, s *Store) string {
	t.Helper()
	rows, err := s.r.QueryContext(t.Context(), `SELECT tag_name FROM dolt_tags ORDER BY tag_name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(names, ",")
}
