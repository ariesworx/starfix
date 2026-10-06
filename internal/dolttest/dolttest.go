// Package dolttest starts a throwaway dolt sql-server for tests.
package dolttest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver for the readiness check
)

// ErrNoDolt means dolt is not on PATH; callers skip their tests.
var ErrNoDolt = errors.New("dolt not found on PATH")

// Server is a running dolt sql-server on a loopback port.
type Server struct {
	Port int
	dir  string
	cmd  *exec.Cmd
	exit chan error
	n    atomic.Int64
}

// Start runs dolt sql-server with its data and config under dir, on a free
// port, and waits until it accepts connections.
func Start(ctx context.Context, dir string) (*Server, error) {
	bin, err := exec.LookPath("dolt")
	if err != nil {
		return nil, ErrNoDolt
	}
	root := filepath.Join(dir, "home")
	data := filepath.Join(dir, "data")
	for _, d := range []string{root, data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("dolttest: %w", err)
		}
	}
	env := append(os.Environ(), "DOLT_ROOT_PATH="+root)
	for _, kv := range [][2]string{{"user.name", "starfix-test"}, {"user.email", "test@example.com"}} {
		c := exec.CommandContext(ctx, bin, "config", "--global", "--add", kv[0], kv[1]) //nolint:gosec // fixed args, dolt from PATH
		c.Env = env
		if out, err := c.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("dolttest: dolt config: %w: %s", err, out)
		}
	}
	var lastErr error
	for range 3 {
		port, err := freePort()
		if err != nil {
			return nil, err
		}
		s, err := launch(ctx, bin, env, data, port, dir)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func launch(ctx context.Context, bin string, env []string, data string, port int, dir string) (*Server, error) {
	logf, err := os.Create(filepath.Join(dir, "server.log")) //nolint:gosec // test temp dir
	if err != nil {
		return nil, fmt.Errorf("dolttest: %w", err)
	}
	cmd := exec.Command(bin, "sql-server", "--host", "127.0.0.1", "--port", strconv.Itoa(port), "--data-dir", data) //nolint:gosec // dolt from PATH
	cmd.Env = env
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		return nil, fmt.Errorf("dolttest: start dolt: %w", err)
	}
	s := &Server{Port: port, dir: dir, cmd: cmd, exit: make(chan error, 1)}
	go func() { s.exit <- cmd.Wait(); _ = logf.Close() }()

	db, err := sql.Open("mysql", s.rootDSN(""))
	if err != nil {
		return nil, errors.Join(fmt.Errorf("dolttest: %w", err), s.Stop())
	}
	defer func() { _ = db.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := db.PingContext(ctx); err == nil {
			return s, nil
		}
		select {
		case err := <-s.exit:
			return nil, fmt.Errorf("dolttest: dolt exited early (see %s/server.log): %v", dir, err)
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), s.Stop())
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return nil, errors.Join(errors.New("dolttest: server not ready after 30s"), s.Stop())
		}
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("dolttest: free port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (s *Server) rootDSN(db string) string {
	return fmt.Sprintf("root@tcp(127.0.0.1:%d)/%s", s.Port, db)
}

// NewDatabase creates an empty database with a unique name and returns its
// DSN.
func (s *Server) NewDatabase(ctx context.Context) (string, error) {
	name := fmt.Sprintf("t%d", s.n.Add(1))
	db, err := sql.Open("mysql", s.rootDSN(""))
	if err != nil {
		return "", fmt.Errorf("dolttest: %w", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		return "", fmt.Errorf("dolttest: create database: %w", err)
	}
	return s.rootDSN(name), nil
}

// Stop shuts the server down.
func (s *Server) Stop() error {
	if s.cmd.Process == nil {
		return nil
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.exit:
		return nil
	case <-time.After(10 * time.Second):
		if err := s.cmd.Process.Kill(); err != nil {
			return fmt.Errorf("dolttest: kill: %w", err)
		}
		<-s.exit
		return nil
	}
}
