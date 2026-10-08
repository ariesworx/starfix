package dolttest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/version"
)

// startOrSkip starts a server for this package's own tests. It skips without
// dolt unless STARFIX_REQUIRE_DOLT is set, as every Dolt-backed package does.
func startOrSkip(t *testing.T) *Server {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := Start(ctx, t.TempDir())
	if errors.Is(err, ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the dolttest tests")
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })
	return s
}

func TestStartWithoutDolt(t *testing.T) {
	tests := []struct {
		name    string
		require string
		skip    bool // the error is ErrNoDolt, so callers skip
	}{
		{name: "not required", require: "", skip: true},
		{name: "required", require: "1", skip: false},
		{name: "explicitly not required", require: "0", skip: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			t.Setenv(RequireEnv, tc.require)
			_, err := Start(t.Context(), t.TempDir())
			if err == nil {
				t.Fatal("Start without dolt on PATH succeeded")
			}
			if got := errors.Is(err, ErrNoDolt); got != tc.skip {
				t.Errorf("Start with %s=%q: errors.Is(%v, ErrNoDolt) = %v, want %v", RequireEnv, tc.require, err, got, tc.skip)
			}
			if !tc.skip && !strings.Contains(err.Error(), RequireEnv) {
				t.Errorf("Start with %s=%q: error %q does not name %s", RequireEnv, tc.require, err, RequireEnv)
			}
		})
	}
}

// TestDoltVersion checks that the Dolt the tests run against is the one
// this release names (version.Dolt, and CI's DOLT_VERSION).
func TestDoltVersion(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		if required() {
			t.Fatalf("%s is set but dolt is not on PATH", RequireEnv)
		}
		t.Skip("dolt is not on PATH")
	}
	got, err := Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got != version.Dolt {
		t.Errorf("Version() = %q, want version.Dolt %q", got, version.Dolt)
	}
}

// TestStartPortTaken reproduces T-8: the port Start picks is taken by
// another Dolt before this one binds it. Start must not mistake the other
// server for its own; it retries on another port.
func TestStartPortTaken(t *testing.T) {
	a := startOrSkip(t)
	if _, err := a.NewDatabase(t.Context()); err != nil {
		t.Fatal(err)
	}
	ports := []int{a.Port}
	pickPort = func() (int, error) {
		if len(ports) > 0 {
			p := ports[0]
			ports = ports[1:]
			return p, nil
		}
		return freePort()
	}
	t.Cleanup(func() { pickPort = freePort })
	b := startOrSkip(t)
	if b.Port == a.Port {
		t.Fatalf("second server reports the first one's port %d", a.Port)
	}
	dsn, err := b.NewDatabase(t.Context())
	if err != nil {
		t.Fatalf("NewDatabase on the second server: %v", err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatalf("ping the second server's database: %v", err)
	}
}

// TestStopLeavesNothingRunning checks that nothing dolt started still runs
// once Stop returns, so the caller can remove the directory. As it exits,
// dolt starts a detached "dolt send-metrics" unless DOLT_DISABLE_EVENT_FLUSH
// is set, and that process wrote under the server's directory while
// TempDir's cleanup was removing it.
func TestStopLeavesNothingRunning(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("listing processes needs /proc")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	s, err := Start(ctx, dir)
	if errors.Is(err, ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the dolttest tests")
	}
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	left, err := running(filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) > 0 {
		t.Errorf("after Stop, still running with the server's DOLT_ROOT_PATH: %q", left)
	}
}

// running returns the pid and command line of each process whose
// environment sets DOLT_ROOT_PATH to root. It reads /proc, so Linux only.
func running(root string) ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	want := "\x00DOLT_ROOT_PATH=" + root + "\x00"
	var found []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue // not a process
		}
		env, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !strings.Contains("\x00"+string(env)+"\x00", want) {
			continue // exited, another user's, or not this server's
		}
		cmd, _ := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		found = append(found, e.Name()+" "+strings.TrimSpace(strings.ReplaceAll(string(cmd), "\x00", " ")))
	}
	return found, nil
}

// TestLockedDown checks that the test server is configured as
// docs/server.md says a production one must be: NewDatabase's DSN logs in as
// User, not root, and secure_file_priv names a directory.
func TestLockedDown(t *testing.T) {
	s := startOrSkip(t)
	dsn, err := s.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var user string
	var priv sql.NullString
	if err := db.QueryRowContext(t.Context(), "SELECT CURRENT_USER(), @@secure_file_priv").Scan(&user, &priv); err != nil {
		t.Fatal(err)
	}
	if user != User+"@localhost" {
		t.Errorf("NewDatabase DSN logs in as %q, want %s@localhost", user, User)
	}
	if !priv.Valid || priv.String == "" {
		t.Errorf("secure_file_priv = %q (valid %v), want a directory", priv.String, priv.Valid)
	}
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = ?",
		"'"+User+"'@'localhost'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%s holds %d global privileges, want none", User, n)
	}
}
