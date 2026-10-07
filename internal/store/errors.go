package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

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
	// ErrForbidden: the actor may not make this change: another
	// principal holds the issue, or the change is for admins.
	ErrForbidden = errors.New("forbidden")

	// errRetry marks a failure that rerunning the write resolves.
	errRetry = errors.New("retry")
)

// retryable reports whether err is Dolt's signal that a concurrent
// transaction won: a serialization failure (1213), a lock timeout, or a
// constraint violation produced by transaction sequencing (1105).
func retryable(err error) bool {
	if errors.Is(err, errRetry) {
		return true
	}
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

// HeldError refuses a start of an issue held under a live claim: by
// another principal, or (Own) by another session of the actor's own
// principal, which a start may take over only when asked to. It wraps
// ErrConflict.
type HeldError struct {
	ID IssueID
	By string
	// Own is set when By is the actor's principal; Session is the
	// session holding it then.
	Own     bool
	Session string
}

func (e *HeldError) Error() string {
	if e.Own {
		return fmt.Sprintf("issue %s is held by your session %s", e.ID, e.Session)
	}
	return fmt.Sprintf("issue %s is in progress by %s", e.ID, e.By)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *HeldError) Unwrap() error { return ErrConflict }

// IdemError refuses an idempotency key the principal already used for a
// different request, or used more than IdemTTL ago (Expired). It wraps
// ErrConflict.
type IdemError struct {
	Key     string
	Expired bool
}

func (e *IdemError) Error() string {
	if e.Expired {
		return fmt.Sprintf("idempotency key %q expired: it was used more than %s ago", e.Key, IdemTTL)
	}
	return fmt.Sprintf("idempotency key %q was already used for a different request", e.Key)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *IdemError) Unwrap() error { return ErrConflict }

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

// ForbiddenError refuses a change the actor may not make. Either Holder,
// another principal, holds the issue under a live claim until Until, and
// only the holder or an admin may change it; or Action is for admins only
// (for example "close --force"). It wraps ErrForbidden.
type ForbiddenError struct {
	ID     IssueID
	Holder Actor
	Until  time.Time
	Action string
}

func (e *ForbiddenError) Error() string {
	if e.Action != "" {
		return fmt.Sprintf("%s on %s is for admins", e.Action, e.ID)
	}
	return fmt.Sprintf("issue %s is held by %s/%s until %s; only the holder or an admin may change it",
		e.ID, e.Holder.Principal, e.Holder.Session, e.Until.UTC().Format(time.RFC3339))
}

// Unwrap makes errors.Is(err, ErrForbidden) hold.
func (e *ForbiddenError) Unwrap() error { return ErrForbidden }
