// Command dolt is the stage 0 spike: it measures whether a Dolt sql-server
// can serve as the authority database behind starfixd.
//
// Usage:
//
//	go run ./spike/dolt -dsn 'root@tcp(127.0.0.1:3306)/' <test> [flags]
//
// Tests: claims, forupdate, lostupdate, commits, vectors. Each test creates
// and drops its own database, so it is safe to run them in any order against
// a scratch server. Do not point it at a server holding real data.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

var (
	dsnBase = flag.String("dsn", "root@tcp(127.0.0.1:3306)/", "server DSN without a database name")
	dbName  = flag.String("db", "spike", "scratch database the tests create and drop")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: dolt [-dsn DSN] [-db NAME] claims|forupdate|lostupdate|commits|vectors [flags]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	args := flag.Args()[1:]
	var err error
	switch flag.Arg(0) {
	case "claims":
		err = runClaims(ctx, args)
	case "forupdate":
		err = runForUpdate(ctx, args)
	case "lostupdate":
		err = runLostUpdate(ctx, args)
	case "commits":
		err = runCommits(ctx, args)
	case "vectors":
		err = runVectors(ctx, args)
	default:
		flag.Usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// open returns a pool on the scratch database, creating it first when fresh
// is set (dropping any previous copy).
func open(ctx context.Context, fresh bool, maxConns int) (*sql.DB, error) {
	if fresh {
		root, err := sql.Open("mysql", *dsnBase)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		for _, q := range []string{
			"DROP DATABASE IF EXISTS " + *dbName,
			"CREATE DATABASE " + *dbName,
		} {
			if _, err := root.ExecContext(ctx, q); err != nil {
				return nil, fmt.Errorf("%s: %w", q, err)
			}
		}
	}
	db, err := sql.Open("mysql", *dsnBase+*dbName+"?parseTime=true&interpolateParams=false")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	return db, db.PingContext(ctx)
}

func execAll(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, stmts ...string) error {
	for _, s := range stmts {
		if _, err := q.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", firstLine(s), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// errClass groups server errors so the report can say what kind of conflict
// a writer saw.
func errClass(err error) string {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		msg := me.Message
		if len(msg) > 60 {
			msg = msg[:60]
		}
		return fmt.Sprintf("%d %s", me.Number, msg)
	}
	s := err.Error()
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// isRetryable reports whether a writer should retry: a serialization or
// deadlock error, or Dolt's merge conflict on commit.
func isRetryable(err error) bool {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case 1213, 1205: // ER_LOCK_DEADLOCK, ER_LOCK_WAIT_TIMEOUT
			return true
		}
		m := strings.ToLower(me.Message)
		return strings.Contains(m, "conflict") || strings.Contains(m, "serializ") ||
			strings.Contains(m, "retry") || strings.Contains(m, "deadlock")
	}
	return false
}

type latencies []time.Duration

func (l latencies) pct(p float64) time.Duration {
	if len(l) == 0 {
		return 0
	}
	s := append(latencies(nil), l...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(p * float64(len(s)-1))
	return s[i]
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d.Microseconds())/1000) }
