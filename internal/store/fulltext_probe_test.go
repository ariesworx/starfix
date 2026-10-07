package store

import (
	"database/sql"
	"testing"
)

// TestDoltFulltextIsUnreliable pins why similar closed issues are scored
// in Go rather than with a FULLTEXT index (design §12 item 6, As built):
// on the pinned Dolt, MATCH ... AGAINST in a WHERE clause returns rows
// more than once, and boolean mode is unsupported. When a Dolt upgrade
// makes this fail, FULLTEXT may have become usable: reconsider it.
func TestDoltFulltextIsUnreliable(t *testing.T) {
	db, err := sql.Open("mysql", newDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := t.Context()
	for _, q := range []string{
		`CREATE TABLE ft (id VARCHAR(10) PRIMARY KEY, title VARCHAR(500), body TEXT)`,
		`CREATE FULLTEXT INDEX ft_text ON ft (title, body)`,
		`INSERT INTO ft VALUES ('a', 'Login fails with expired token', 'The session token expires'),
		 ('b', 'Add dark mode', 'theme colors'), ('c', 'Login page slow', 'token refresh'),
		 ('d', 'Fix token expiry on login', 'refresh the token before expiry')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	var rows, distinct int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT id) FROM ft
  WHERE MATCH(title, body) AGAINST ('login token expired')`).Scan(&rows, &distinct); err != nil {
		t.Fatal(err)
	}
	_, boolErr := db.ExecContext(ctx, `SELECT id FROM ft WHERE MATCH(title, body) AGAINST ('+login' IN BOOLEAN MODE)`)
	if rows == distinct {
		t.Errorf("MATCH now returns each of %d rows once (boolean mode: %v): reconsider FULLTEXT for similar issues", rows, boolErr)
	}
	t.Logf("MATCH returned %d rows for %d issues; boolean mode: %v", rows, distinct, boolErr)
}
