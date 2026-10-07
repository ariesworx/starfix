package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func TestAccountProblems(t *testing.T) {
	dir := sql.NullString{String: "/var/lib/dolt/no-files", Valid: true}
	tests := []struct {
		name      string
		user      string
		global    []string
		grantable bool
		filePriv  sql.NullString
		want      []string // substrings, one per problem
	}{
		{name: "least privilege", user: "starfix", filePriv: dir},
		{name: "usage only", user: "starfix", global: []string{"USAGE"}, filePriv: dir},
		{name: "secure_file_priv NULL disables file access", user: "starfix"},
		{name: "root", user: "root", filePriv: dir, want: []string{"root"}},
		{name: "file", user: "starfix", global: []string{"FILE"}, filePriv: dir, want: []string{"FILE"}},
		{name: "super and select", user: "starfix", global: []string{"SELECT", "SUPER"}, filePriv: dir, want: []string{"SELECT, SUPER"}},
		{name: "grant option", user: "starfix", grantable: true, filePriv: dir, want: []string{"GRANT OPTION"}},
		{name: "empty secure_file_priv", user: "starfix", filePriv: sql.NullString{Valid: true}, want: []string{"secure_file_priv"}},
		{name: "dolt default root", user: "root", global: []string{"FILE", "SUPER"}, grantable: true,
			filePriv: sql.NullString{Valid: true}, want: []string{"root", "FILE, SUPER", "GRANT OPTION", "secure_file_priv"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := accountProblems(tc.user, tc.global, tc.grantable, tc.filePriv)
			if len(got) != len(tc.want) {
				t.Fatalf("accountProblems(%q, %v, %v, %v) = %q, want %d problems", tc.user, tc.global, tc.grantable, tc.filePriv, got, len(tc.want))
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("problem %d = %q, want it to mention %q", i, got[i], w)
				}
			}
		})
	}
}

// rootDSN is dsn's database reached as Dolt's root.
func rootDSN(t *testing.T, dsn string) string {
	t.Helper()
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return server.RootDSN(cfg.DBName)
}

// S-2: a store opened as Dolt's root, which can read and write files and
// rewrite the event log behind starfixd's back, is refused unless the
// caller explicitly allows it (starfixd allows it only with --dev).
func TestOpenRefusesUnsafeAccount(t *testing.T) {
	dsn := newDSN(t)
	_, err := Open(t.Context(), rootDSN(t, dsn), Options{CommitInterval: -1})
	var ua *UnsafeAccountError
	if !errors.As(err, &ua) {
		t.Fatalf("Open as root = %v, want an *UnsafeAccountError", err)
	}
	if ua.User != "root@localhost" || !slices.ContainsFunc(ua.Problems, func(p string) bool { return strings.Contains(p, "FILE") }) {
		t.Errorf("UnsafeAccountError = %+v, want root@localhost with FILE among the problems", ua)
	}
	for _, w := range []string{"GRANT ALL ON", "secure_file_priv", "--allow-unsafe-dolt"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q does not name %q", err, w)
		}
	}
	s, err := Open(t.Context(), rootDSN(t, dsn), Options{CommitInterval: -1, AllowUnsafeAccount: true})
	if err != nil {
		t.Fatalf("Open as root with AllowUnsafeAccount: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// The least-privileged account dolttest hands out opens.
	openStore(t, dsn, Options{})
}
