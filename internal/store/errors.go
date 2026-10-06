package store

import (
	"errors"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// Errors returned by the store, always wrapped with context. Test with
// errors.Is.
var (
	// ErrNotFound: the issue (or other target) does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict: the row changed since the caller's revision, or a
	// concurrent write kept winning until retries ran out.
	ErrConflict = errors.New("conflict")
	// ErrExists: an issue with that ID already exists.
	ErrExists = errors.New("already exists")
	// ErrCycle: the change would create a cycle of blocking edges.
	ErrCycle = errors.New("dependency cycle")
	// ErrInvalid: the input failed validation.
	ErrInvalid = errors.New("invalid input")
	// ErrSchemaTooNew: the database was migrated by a newer starfix.
	ErrSchemaTooNew = errors.New("database schema is newer than this binary")
)

// retryable reports whether err is Dolt's signal that a concurrent
// transaction won: a serialization failure (1213), a lock timeout, or a
// constraint violation produced by transaction sequencing (1105).
func retryable(err error) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	switch me.Number {
	case 1213, 1205:
		return true
	case 1105:
		return strings.Contains(me.Message, "transaction sequencing")
	}
	return false
}

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}
