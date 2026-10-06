package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

const commitDDL1 = `CREATE TABLE comments (
  id     BIGINT AUTO_INCREMENT PRIMARY KEY,
  issue  VARCHAR(32)  NOT NULL,
  author VARCHAR(96)  NOT NULL,
  body   TEXT         NOT NULL,
  at     DATETIME(6)  NOT NULL,
  KEY by_issue (issue)
)`

const commitDDL2 = `CREATE TABLE events (
  seq    BIGINT AUTO_INCREMENT PRIMARY KEY,
  at     DATETIME(6)  NOT NULL,
  actor  VARCHAR(96)  NOT NULL,
  op     VARCHAR(32)  NOT NULL,
  target VARCHAR(32)  NOT NULL,
  after  JSON         NULL
)`

// runCommits drives a sustained write mix and compares Dolt commit
// strategies: none (working set only), one DOLT_COMMIT per request, or a
// background committer every N writes or every interval.
func runCommits(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("commits", flag.ExitOnError)
	workers := fs.Int("workers", 8, "concurrent writers")
	mode := fs.String("mode", "none", "none | per-request | every-n | interval")
	everyN := fs.Int("n", 50, "writes per commit for every-n")
	interval := fs.Duration("interval", time.Second, "commit interval for interval mode")
	dur := fs.Duration("dur", 20*time.Second, "run length")
	n := fs.Int("issues", 2000, "issues to update")
	fs.Parse(args)

	db, err := open(ctx, true, *workers+4)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := seedIssues(ctx, db, *n); err != nil {
		return err
	}
	if err := execAll(ctx, db, commitDDL1, commitDDL2, "CALL DOLT_COMMIT('-Am','schema')"); err != nil {
		return err
	}

	var writes, commits, commitErrs, conflicts atomic.Int64
	commitCh := make(chan struct{}, 1)
	stop := make(chan struct{})
	var cwg sync.WaitGroup
	var commitLat latencies
	if *mode == "every-n" || *mode == "interval" {
		cwg.Add(1)
		go func() {
			defer cwg.Done()
			conn, _ := db.Conn(ctx)
			defer conn.Close()
			tick := time.NewTicker(*interval)
			defer tick.Stop()
			for {
				if *mode == "interval" {
					select {
					case <-stop:
						return
					case <-tick.C:
					}
				} else {
					select {
					case <-stop:
						return
					case <-commitCh:
					}
				}
				t0 := time.Now()
				_, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-Am','batch','--skip-empty')")
				commitLat = append(commitLat, time.Since(t0))
				if err != nil {
					commitErrs.Add(1)
				} else {
					commits.Add(1)
				}
			}
		}()
	}

	deadline := time.Now().Add(*dur)
	lats := make([]latencies, *workers)
	errs := make([]map[string]int, *workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := range *workers {
		errs[w] = map[string]int{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				errs[w]["conn"]++
				return
			}
			defer conn.Close()
			actor := fmt.Sprintf("writer-%02d", w)
			for time.Now().Before(deadline) {
				t0 := time.Now()
				err := writeRequest(ctx, conn, actor, *n, *mode == "per-request")
				lats[w] = append(lats[w], time.Since(t0))
				if err != nil {
					conn.ExecContext(ctx, "ROLLBACK")
					errs[w][errClass(err)]++
					if isRetryable(err) {
						conflicts.Add(1)
					}
					continue
				}
				c := writes.Add(1)
				if *mode == "per-request" {
					commits.Add(1)
				}
				if *mode == "every-n" && c%int64(*everyN) == 0 {
					select {
					case commitCh <- struct{}{}:
					default:
					}
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(stop)
	cwg.Wait()

	var all latencies
	allErrs := map[string]int{}
	for w := range *workers {
		all = append(all, lats[w]...)
		for k, v := range errs[w] {
			allErrs[k] += v
		}
	}
	var dc int
	db.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_log").Scan(&dc)
	fmt.Printf("mode=%s workers=%d dur=%.1fs\n", *mode, *workers, elapsed.Seconds())
	fmt.Printf("requests ok=%d req/s=%.0f p50=%sms p99=%sms max=%sms errors=%d (retryable %d) dolt commits=%d (dolt_log rows=%d)\n",
		writes.Load(), float64(writes.Load())/elapsed.Seconds(), ms(all.pct(.5)), ms(all.pct(.99)), ms(all.pct(1)),
		sumMap(allErrs), conflicts.Load(), commits.Load(), dc)
	if len(commitLat) > 0 {
		fmt.Printf("background DOLT_COMMIT latency p50=%sms p99=%sms errors=%d\n", ms(commitLat.pct(.5)), ms(commitLat.pct(.99)), commitErrs.Load())
	}
	printErrs(allErrs)
	return nil
}

// writeRequest is one starfixd request: a CAS update on an issue, an event,
// and every other time a comment, in one SQL transaction.
func writeRequest(ctx context.Context, conn *sql.Conn, actor string, n int, doltCommit bool) error {
	id := fmt.Sprintf("sf-%06d", rand.IntN(n))
	if _, err := conn.ExecContext(ctx, "START TRANSACTION"); err != nil {
		return err
	}
	var rev int64
	if err := conn.QueryRowContext(ctx, "SELECT rev FROM issues WHERE id=?", id).Scan(&rev); err != nil {
		return err
	}
	res, err := conn.ExecContext(ctx, "UPDATE issues SET priority=?, title=?, rev=rev+1, updated_at=NOW(6) WHERE id=? AND rev=?",
		rand.IntN(5), fmt.Sprintf("edited by %s", actor), id, rev)
	if err != nil {
		return err
	}
	if k, _ := res.RowsAffected(); k != 1 {
		conn.ExecContext(ctx, "ROLLBACK")
		return errCASLost
	}
	if rand.IntN(2) == 0 {
		if _, err := conn.ExecContext(ctx, "INSERT INTO comments (issue, author, body, at) VALUES (?,?,?,NOW(6))",
			id, actor, "a comment of moderate length, about as long as an agent's note"); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO events (at, actor, op, target, after) VALUES (NOW(6),?,'update',?,JSON_OBJECT('rev',?))",
		actor, id, rev+1); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	if doltCommit {
		_, err := conn.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', ?, '--skip-empty')", "update "+id)
		return err
	}
	return nil
}

var errCASLost = fmt.Errorf("cas lost")
