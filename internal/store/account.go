package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The account check (S-2). A Dolt sql-server started without a config
// lets root in from localhost with no password, grants it FILE and every
// other right, and leaves secure_file_priv empty, so any user on the host
// could skip starfixd, rewrite the event log as anyone, and read or write
// files as the Dolt user. Open refuses such an account unless
// Options.AllowUnsafeAccount says otherwise.

// UnsafeAccountError refuses a database account with more rights than
// starfix needs. User is the account as CURRENT_USER() names it
// (user@host), and Problems lists what is unsafe, in words. Its message
// ends with the fix.
type UnsafeAccountError struct {
	User     string
	Problems []string
}

func (e *UnsafeAccountError) Error() string {
	return fmt.Sprintf("dolt account %s is unsafe for starfixd: %s; fix: as Dolt's root, run "+
		"CREATE USER 'starfix'@'localhost' IDENTIFIED BY '...'; GRANT ALL ON <database>.* TO 'starfix'@'localhost'; "+
		"put that user in the DSN, and set system_variables: secure_file_priv: to a directory that does not exist "+
		"in Dolt's config.yaml (docs/server.md, steps 2 and 4); for local development only, pass --dev --allow-unsafe-dolt",
		e.User, strings.Join(e.Problems, "; "))
}

// checkAccount refuses the connected account when it is root, holds any
// right beyond its own database or may grant rights, or when the server
// lets file functions read and write anywhere.
func checkAccount(ctx context.Context, q querier) error {
	var current string
	var filePriv sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT CURRENT_USER(), @@secure_file_priv`).Scan(&current, &filePriv); err != nil {
		return fmt.Errorf("account check: %w", err)
	}
	user, host, _ := strings.Cut(current, "@")
	grantee := "'" + user + "'@'" + host + "'"
	rows, err := q.QueryContext(ctx, `SELECT PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.USER_PRIVILEGES
  WHERE GRANTEE = ? ORDER BY PRIVILEGE_TYPE`, grantee)
	if err != nil {
		return fmt.Errorf("account check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var global []string
	grantable := false
	for rows.Next() {
		var p, g string
		if err := rows.Scan(&p, &g); err != nil {
			return fmt.Errorf("account check: %w", err)
		}
		global = append(global, p)
		grantable = grantable || g == "YES"
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("account check: %w", err)
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.SCHEMA_PRIVILEGES
  WHERE GRANTEE = ? AND IS_GRANTABLE = 'YES'`, grantee).Scan(&n); err != nil {
		return fmt.Errorf("account check: %w", err)
	}
	if p := accountProblems(user, global, grantable || n > 0, filePriv); len(p) > 0 {
		return &UnsafeAccountError{User: current, Problems: p}
	}
	return nil
}

// accountProblems lists what is unsafe about an account named user with
// the global (*.*) privileges given, whether it may grant any of its
// rights, and the server's secure_file_priv. NULL secure_file_priv turns
// file import and export off, which is safe; empty allows every path.
func accountProblems(user string, global []string, grantable bool, filePriv sql.NullString) []string {
	var out []string
	if user == "root" {
		out = append(out, "it is root")
	}
	var extra []string
	for _, p := range global {
		if p != "USAGE" {
			extra = append(extra, p)
		}
	}
	if len(extra) > 0 {
		out = append(out, "it holds global privileges "+strings.Join(extra, ", ")+" on *.*")
	}
	if grantable {
		out = append(out, "it has GRANT OPTION")
	}
	if filePriv.Valid && filePriv.String == "" {
		out = append(out, "secure_file_priv is empty, so file functions reach every path")
	}
	return out
}
