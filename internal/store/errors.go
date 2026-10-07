package store

import (
	"errors"
	"fmt"
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
	// ErrNothingReady: StartIssue was asked for the top ready issue and
	// none is ready. It wraps ErrNotFound.
	ErrNothingReady = fmt.Errorf("nothing is ready to start: %w", ErrNotFound)
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

// HeldError reports an issue in progress under another principal. It
// wraps ErrConflict.
type HeldError struct {
	ID IssueID
	By string
}

func (e *HeldError) Error() string { return fmt.Sprintf("issue %s is in progress by %s", e.ID, e.By) }

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *HeldError) Unwrap() error { return ErrConflict }

// StaleEpochError refuses a finish or release that names a claim epoch
// other than the current one: the issue was taken again since. It wraps
// ErrConflict.
type StaleEpochError struct {
	ID      IssueID
	Epoch   int64
	Current int64
	// By is the current holder, if the issue is held.
	By Actor
}

func (e *StaleEpochError) Error() string {
	return fmt.Sprintf("claim epoch %d on %s is stale (now %d)", e.Epoch, e.ID, e.Current)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *StaleEpochError) Unwrap() error { return ErrConflict }
