package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"
)

const issuesDDL = `CREATE TABLE issues (
  id         VARCHAR(32)  NOT NULL PRIMARY KEY,
  title      VARCHAR(200) NOT NULL,
  status     VARCHAR(16)  NOT NULL,
  priority   INT          NOT NULL,
  assignee   VARCHAR(96)  NULL,
  rev        BIGINT       NOT NULL DEFAULT 1,
  updated_at DATETIME(6)  NOT NULL,
  KEY ready (status, priority, id)
)`

const claimsDDL = `CREATE TABLE claims (
  issue      VARCHAR(32) NOT NULL PRIMARY KEY,
  holder     VARCHAR(96) NOT NULL,
  epoch      BIGINT      NOT NULL,
  expires_at DATETIME(6) NOT NULL
)`

func seedIssues(ctx context.Context, db *sql.DB, n int) error {
	if err := execAll(ctx, db, issuesDDL, claimsDDL); err != nil {
		return err
	}
	const batch = 500
	for start := 0; start < n; start += batch {
		var b strings.Builder
		b.WriteString("INSERT INTO issues (id, title, status, priority, updated_at) VALUES ")
		for i := start; i < min(start+batch, n); i++ {
			if i > start {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "('sf-%06d','issue %d','open',%d,NOW(6))", i, i, rand.IntN(5))
		}
		if _, err := db.ExecContext(ctx, b.String()); err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx, "CALL DOLT_COMMIT('-Am', 'seed')")
	return err
}

type claimStats struct {
	won       []string
	attempts  int
	lost      int // CAS matched zero rows
	errs      map[string]int
	claimLat  latencies // first attempt to win, including retries
	attemptLt latencies
}

// runClaims has many workers, each on its own connection, claim issues from
// one ready queue until it is empty or the time limit passes.
func runClaims(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("claims", flag.ExitOnError)
	workers := fs.Int("workers", 20, "concurrent claimers, one connection each")
	n := fs.Int("issues", 2000, "open issues in the ready queue")
	pick := fs.Int("pick", 1, "each attempt takes a random issue from the top N of the queue; 0 partitions the queue so workers never pick the same issue (no logical conflicts)")
	mode := fs.String("mode", "tx", "tx: CAS update + claims insert in one transaction; auto: autocommit CAS update only; forupdate: SELECT ... FOR UPDATE then update")
	limit := fs.Duration("limit", 60*time.Second, "stop after this long")
	fs.Parse(args)

	db, err := open(ctx, true, *workers+2)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := seedIssues(ctx, db, *n); err != nil {
		return err
	}

	deadline := time.Now().Add(*limit)
	stats := make([]*claimStats, *workers)
	var wg sync.WaitGroup
	start := time.Now()
	for w := range *workers {
		st := &claimStats{errs: map[string]int{}}
		stats[w] = st
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := db.Conn(ctx)
			if err != nil {
				st.errs["conn: "+err.Error()]++
				return
			}
			defer conn.Close()
			holder := fmt.Sprintf("worker-%02d/session-%d", w, rand.Uint32())
			part := ""
			if *pick == 0 {
				part = fmt.Sprintf(" AND CAST(SUBSTRING(id, 4) AS UNSIGNED) %% %d = %d", *workers, w)
			}
			claimWorker(ctx, conn, holder, *mode, *pick, part, deadline, st)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	// Verify: each issue won by exactly one client, and the database agrees.
	winner := map[string]string{}
	double := 0
	var all latencies
	var allAttempt latencies
	attempts, lost := 0, 0
	errs := map[string]int{}
	for w, st := range stats {
		for _, id := range st.won {
			if prev, ok := winner[id]; ok {
				double++
				fmt.Printf("DOUBLE CLAIM %s: %s and worker %d\n", id, prev, w)
			}
			winner[id] = fmt.Sprintf("worker-%02d/", w)
		}
		all = append(all, st.claimLat...)
		allAttempt = append(allAttempt, st.attemptLt...)
		attempts += st.attempts
		lost += st.lost
		for k, v := range st.errs {
			errs[k] += v
		}
	}
	rows, err := db.QueryContext(ctx, "SELECT i.id, COALESCE(i.assignee,''), COALESCE(c.holder,''), COALESCE(c.epoch,0) FROM issues i LEFT JOIN claims c ON c.issue=i.id")
	if err != nil {
		return err
	}
	mismatch, assigned, epochBad := 0, 0, 0
	for rows.Next() {
		var id, assignee, holder string
		var epoch int64
		if err := rows.Scan(&id, &assignee, &holder, &epoch); err != nil {
			return err
		}
		if assignee != "" {
			assigned++
		}
		want := winner[id]
		if !strings.HasPrefix(assignee, want) || (want == "") != (assignee == "") {
			mismatch++
		}
		if *mode != "auto" && assignee != "" && (holder != assignee || epoch != 1) {
			epochBad++
		}
	}
	rows.Close()

	won := len(winner)
	fmt.Printf("mode=%s workers=%d pick=%d issues=%d elapsed=%.1fs\n", *mode, *workers, *pick, *n, elapsed.Seconds())
	fmt.Printf("claims won=%d claims/s=%.0f attempts=%d attempts/claim=%.2f cas-lost=%d errors=%d\n",
		won, float64(won)/elapsed.Seconds(), attempts, float64(attempts)/max(1, float64(won)), lost, sumMap(errs))
	fmt.Printf("conflict rate (failed attempts / attempts)=%.1f%%\n", 100*float64(attempts-won)/max(1, float64(attempts)))
	fmt.Printf("claim latency ms (incl. retries): p50=%s p99=%s max=%s; per-attempt p50=%s p99=%s\n",
		ms(all.pct(.5)), ms(all.pct(.99)), ms(all.pct(1)), ms(allAttempt.pct(.5)), ms(allAttempt.pct(.99)))
	fmt.Printf("verify: double-claims=%d db-assigned=%d client-won=%d db/client-mismatch=%d claim-row-mismatch=%d\n",
		double, assigned, won, mismatch, epochBad)
	printErrs(errs)
	return nil
}

func sumMap(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

func printErrs(errs map[string]int) {
	keys := make([]string, 0, len(errs))
	for k := range errs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  error x%d: %s\n", errs[k], k)
	}
}

func claimWorker(ctx context.Context, conn *sql.Conn, holder, mode string, pick int, part string, deadline time.Time, st *claimStats) {
	for time.Now().Before(deadline) {
		begin := time.Now()
		for time.Now().Before(deadline) {
			t0 := time.Now()
			st.attempts++
			id, ok, empty, err := claimOnce(ctx, conn, holder, mode, max(pick, 1), part)
			st.attemptLt = append(st.attemptLt, time.Since(t0))
			if empty {
				return
			}
			if err != nil {
				st.errs[errClass(err)]++
				conn.ExecContext(ctx, "ROLLBACK")
				if !isRetryable(err) {
					time.Sleep(5 * time.Millisecond)
				}
				continue
			}
			if !ok {
				st.lost++
				continue
			}
			st.won = append(st.won, id)
			st.claimLat = append(st.claimLat, time.Since(begin))
			break
		}
	}
}

// claimOnce makes one attempt. empty means the queue is drained.
func claimOnce(ctx context.Context, conn *sql.Conn, holder, mode string, pick int, part string) (id string, won, empty bool, err error) {
	if mode == "forupdate" {
		return claimForUpdate(ctx, conn, holder)
	}
	rows, err := conn.QueryContext(ctx,
		"SELECT id, rev FROM issues WHERE status='open' AND assignee IS NULL"+part+" ORDER BY priority, id LIMIT ?", pick)
	if err != nil {
		return "", false, false, err
	}
	type cand struct {
		id  string
		rev int64
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.rev); err != nil {
			rows.Close()
			return "", false, false, err
		}
		cs = append(cs, c)
	}
	rows.Close()
	if len(cs) == 0 {
		return "", false, true, nil
	}
	c := cs[rand.IntN(len(cs))]

	const cas = "UPDATE issues SET assignee=?, status='in_progress', rev=rev+1, updated_at=NOW(6) WHERE id=? AND rev=? AND assignee IS NULL"
	if mode == "auto" {
		res, err := conn.ExecContext(ctx, cas, holder, c.id, c.rev)
		if err != nil {
			return "", false, false, err
		}
		n, _ := res.RowsAffected()
		return c.id, n == 1, false, nil
	}
	if _, err := conn.ExecContext(ctx, "START TRANSACTION"); err != nil {
		return "", false, false, err
	}
	res, err := conn.ExecContext(ctx, cas, holder, c.id, c.rev)
	if err != nil {
		return "", false, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		_, err := conn.ExecContext(ctx, "ROLLBACK")
		return c.id, false, false, err
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT INTO claims (issue, holder, epoch, expires_at) VALUES (?, ?, 1, NOW(6) + INTERVAL 15 MINUTE) ON DUPLICATE KEY UPDATE holder=VALUES(holder), epoch=epoch+1, expires_at=VALUES(expires_at)",
		c.id, holder); err != nil {
		return "", false, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", false, false, err
	}
	return c.id, true, false, nil
}

func claimForUpdate(ctx context.Context, conn *sql.Conn, holder string) (string, bool, bool, error) {
	if _, err := conn.ExecContext(ctx, "START TRANSACTION"); err != nil {
		return "", false, false, err
	}
	var id string
	err := conn.QueryRowContext(ctx,
		"SELECT id FROM issues WHERE status='open' AND assignee IS NULL ORDER BY priority, id LIMIT 1 FOR UPDATE").Scan(&id)
	if err == sql.ErrNoRows {
		conn.ExecContext(ctx, "ROLLBACK")
		return "", false, true, nil
	}
	if err != nil {
		return "", false, false, err
	}
	// No rev guard: if FOR UPDATE really locked, this cannot lose.
	res, err := conn.ExecContext(ctx,
		"UPDATE issues SET assignee=?, status='in_progress', rev=rev+1, updated_at=NOW(6) WHERE id=?", holder, id)
	if err != nil {
		return "", false, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		_, err := conn.ExecContext(ctx, "ROLLBACK")
		return id, false, false, err
	}
	if _, err := conn.ExecContext(ctx,
		"INSERT INTO claims (issue, holder, epoch, expires_at) VALUES (?, ?, 1, NOW(6) + INTERVAL 15 MINUTE) ON DUPLICATE KEY UPDATE holder=VALUES(holder), epoch=epoch+1, expires_at=VALUES(expires_at)",
		id, holder); err != nil {
		return "", false, false, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return "", false, false, err
	}
	return id, true, false, nil
}
