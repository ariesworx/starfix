package main

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"
)

// twoConns returns two dedicated connections on a fresh database holding
// one issue row.
func twoConns(ctx context.Context) (*sql.DB, *sql.Conn, *sql.Conn, error) {
	db, err := open(ctx, true, 4)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := execAll(ctx, db, issuesDDL,
		"INSERT INTO issues (id, title, status, priority, updated_at) VALUES ('sf-1','orig','open',1,NOW(6))",
		"CALL DOLT_COMMIT('-Am','seed')"); err != nil {
		return nil, nil, nil, err
	}
	a, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	b, err := db.Conn(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	return db, a, b, nil
}

func reset(ctx context.Context, db *sql.DB) error {
	return execAll(ctx, db, "UPDATE issues SET title='orig', status='open', priority=1, assignee=NULL, rev=1 WHERE id='sf-1'")
}

func row(ctx context.Context, db *sql.DB) string {
	var title, status string
	var assignee sql.NullString
	var prio, rev int
	db.QueryRowContext(ctx, "SELECT title, status, priority, assignee, rev FROM issues WHERE id='sf-1'").Scan(&title, &status, &prio, &assignee, &rev)
	return fmt.Sprintf("title=%s status=%s priority=%d assignee=%s rev=%d", title, status, prio, assignee.String, rev)
}

func errStr(err error) string {
	if err == nil {
		return "ok"
	}
	return "ERROR " + errClass(err)
}

// runForUpdate checks whether SELECT ... FOR UPDATE blocks a second
// transaction locking the same row, and what happens when both then write.
func runForUpdate(ctx context.Context, _ []string) error {
	db, a, b, err := twoConns(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	var id string
	execAll(ctx, a, "START TRANSACTION")
	if err := a.QueryRowContext(ctx, "SELECT id FROM issues WHERE id='sf-1' FOR UPDATE").Scan(&id); err != nil {
		return fmt.Errorf("A lock: %w", err)
	}
	execAll(ctx, b, "START TRANSACTION")
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	t0 := time.Now()
	err = b.QueryRowContext(cctx, "SELECT id FROM issues WHERE id='sf-1' FOR UPDATE").Scan(&id)
	cancel()
	wait := time.Since(t0)
	if wait > 2*time.Second {
		fmt.Printf("B's SELECT FOR UPDATE blocked %s (row lock held): %s\n", wait.Round(time.Millisecond), errStr(err))
		return nil
	}
	fmt.Printf("B's SELECT FOR UPDATE returned in %s while A held it: %s (no row lock)\n", wait.Round(time.Microsecond), errStr(err))

	_, eb := b.ExecContext(ctx, "UPDATE issues SET assignee='B', rev=rev+1 WHERE id='sf-1'")
	fmt.Println("B update:", errStr(eb))
	fmt.Println("B commit:", errStr(execAll(ctx, b, "COMMIT")))
	_, ea := a.ExecContext(ctx, "UPDATE issues SET assignee='A', rev=rev+1 WHERE id='sf-1'")
	fmt.Println("A update (after B committed):", errStr(ea))
	fmt.Println("A commit:", errStr(execAll(ctx, a, "COMMIT")))
	fmt.Println("final:", row(ctx, db))
	return nil
}

// runLostUpdate runs two overlapping transactions under the server's default
// isolation for several write patterns and reports what survives.
func runLostUpdate(ctx context.Context, _ []string) error {
	db, a, b, err := twoConns(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	var iso string
	db.QueryRowContext(ctx, "SELECT @@transaction_isolation").Scan(&iso)
	fmt.Println("isolation:", iso)

	cases := []struct {
		name string
		a, b string
	}{
		{"different columns", "UPDATE issues SET title='A' WHERE id='sf-1'", "UPDATE issues SET priority=9 WHERE id='sf-1'"},
		{"same column, different values", "UPDATE issues SET title='A' WHERE id='sf-1'", "UPDATE issues SET title='B' WHERE id='sf-1'"},
		{"same column, same value", "UPDATE issues SET status='closed' WHERE id='sf-1'", "UPDATE issues SET status='closed' WHERE id='sf-1'"},
		{"increment (priority=priority+1)", "UPDATE issues SET priority=priority+1 WHERE id='sf-1'", "UPDATE issues SET priority=priority+1 WHERE id='sf-1'"},
		{"CAS rev, different columns", "UPDATE issues SET title='A', rev=rev+1 WHERE id='sf-1' AND rev=1", "UPDATE issues SET priority=9, rev=rev+1 WHERE id='sf-1' AND rev=1"},
		{"CAS rev, same claim value (same holder)", "UPDATE issues SET assignee='h', rev=rev+1 WHERE id='sf-1' AND rev=1 AND assignee IS NULL", "UPDATE issues SET assignee='h', rev=rev+1 WHERE id='sf-1' AND rev=1 AND assignee IS NULL"},
		// The fix under test: every write also sets a column to a value no
		// other writer can produce, so any two concurrent writes to a row
		// touch the same cell with different values and one fails.
		{"CAS rev + unique stamp, different cols", "UPDATE issues SET title='A', rev=rev+1, updated_at=NOW(6) WHERE id='sf-1' AND rev=1", "UPDATE issues SET priority=9, rev=rev+1, updated_at=NOW(6) + INTERVAL 1 MICROSECOND WHERE id='sf-1' AND rev=1"},
		{"increment + unique stamp", "UPDATE issues SET priority=priority+1, updated_at=NOW(6) WHERE id='sf-1'", "UPDATE issues SET priority=priority+1, updated_at=NOW(6) + INTERVAL 1 MICROSECOND WHERE id='sf-1'"},
	}
	for _, c := range cases {
		if err := reset(ctx, db); err != nil {
			return err
		}
		// Both transactions take their snapshot before either writes.
		execAll(ctx, a, "START TRANSACTION")
		execAll(ctx, b, "START TRANSACTION")
		var x string
		a.QueryRowContext(ctx, "SELECT title FROM issues WHERE id='sf-1'").Scan(&x)
		b.QueryRowContext(ctx, "SELECT title FROM issues WHERE id='sf-1'").Scan(&x)
		ra, ea := a.ExecContext(ctx, c.a)
		rb, eb := b.ExecContext(ctx, c.b)
		na, nb := affected(ra), affected(rb)
		ca := execAll(ctx, a, "COMMIT")
		cb := execAll(ctx, b, "COMMIT")
		if ca != nil {
			a.ExecContext(ctx, "ROLLBACK")
		}
		if cb != nil {
			b.ExecContext(ctx, "ROLLBACK")
		}
		fmt.Printf("%-40s A: rows=%d %s commit=%s | B: rows=%d %s commit=%s | final: %s\n",
			c.name, na, errStr(ea), errStr(ca), nb, errStr(eb), errStr(cb), row(ctx, db))
	}

	// Autocommit (no explicit transaction): the shape starfixd would use for
	// a single-statement CAS.
	if err := reset(ctx, db); err != nil {
		return err
	}
	fmt.Println("autocommit, sequential CAS on the same rev:")
	r1, e1 := a.ExecContext(ctx, "UPDATE issues SET assignee='A', rev=rev+1 WHERE id='sf-1' AND rev=1 AND assignee IS NULL")
	r2, e2 := b.ExecContext(ctx, "UPDATE issues SET assignee='B', rev=rev+1 WHERE id='sf-1' AND rev=1 AND assignee IS NULL")
	fmt.Printf("  A rows=%d %s, B rows=%d %s, final: %s\n", affected(r1), errStr(e1), affected(r2), errStr(e2), row(ctx, db))

	// Concurrent autocommit statements on one hot row.
	for _, c := range []struct{ name, q, set string }{
		{"autocommit increment", "UPDATE issues SET priority=priority+1 WHERE id='sf-1'", ""},
		{"autocommit rev CAS, title=worker name", "", "title=CONCAT('w', ?)"},
		{"autocommit rev CAS, title unique per write", "", "title=CONCAT('w', ?, '-', UUID())"},
		{"autocommit rev CAS, half title/half priority", "", "split"},
		{"autocommit rev CAS + unique stamp, half/half", "", "split-stamp"},
	} {
		if err := reset(ctx, db); err != nil {
			return err
		}
		const workers, each = 20, 50
		var mu sync.Mutex
		ok, errs := 0, map[string]int{}
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := db.Conn(ctx)
				if err != nil {
					return
				}
				defer conn.Close()
				for range each {
					var res sql.Result
					var err error
					if c.q != "" {
						res, err = conn.ExecContext(ctx, c.q)
					} else {
						var rev int64
						if err = conn.QueryRowContext(ctx, "SELECT rev FROM issues WHERE id='sf-1'").Scan(&rev); err == nil {
							set := c.set
							switch c.set {
							case "split":
								set = map[bool]string{true: "title=CONCAT('w', ?)", false: "priority=?"}[w%2 == 0]
							case "split-stamp":
								set = map[bool]string{true: "title=CONCAT('w', ?)", false: "priority=?"}[w%2 == 0] + ", updated_at=NOW(6), assignee=UUID()"
							}
							res, err = conn.ExecContext(ctx, "UPDATE issues SET "+set+", rev=rev+1 WHERE id='sf-1' AND rev=?", w, rev)
						}
					}
					mu.Lock()
					if err != nil {
						errs[errClass(err)]++
					} else if affected(res) == 1 {
						ok++
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		fmt.Printf("%s: %d workers x %d, statements reporting 1 row=%d, final: %s\n", c.name, workers, each, ok, row(ctx, db))
		printErrs(errs)
	}
	return nil
}

func affected(r sql.Result) int64 {
	if r == nil {
		return -1
	}
	n, _ := r.RowsAffected()
	return n
}
