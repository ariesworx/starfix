package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Errors returned by the store, always wrapped with context. Test with
// [errors.Is].
//
// starfixd's error mapping (internal/server/errors.go) chooses the next
// step by error, or by a typed error such as [*StateError], never by
// message. It shows the messages, trimmed, and its tests pin what it
// shows.
var (
	// ErrNotFound means the issue, or another target, does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict means the row changed since the caller's revision, or
	// a concurrent write kept winning until retries ran out.
	ErrConflict = errors.New("conflict")
	// ErrExists means an issue with that ID already exists.
	ErrExists = errors.New("already exists")
	// ErrCycle means the change would make a cycle of blocking edges or
	// parent links.
	ErrCycle = errors.New("dependency cycle")
	// ErrInvalid means the input failed validation, or the change does
	// not fit the issue's state, such as a status change on a closed
	// issue.
	ErrInvalid = errors.New("invalid input")
	// ErrNothingReady means StartIssue was asked for the top ready issue
	// and none is ready. It wraps ErrNotFound.
	ErrNothingReady = fmt.Errorf("nothing is ready to start: %w", ErrNotFound)
	// ErrSchemaTooNew means a newer starfixd migrated the database.
	ErrSchemaTooNew = errors.New("database schema is newer than this binary")
	// ErrForbidden means the actor may not make this change: another
	// principal holds the issue, or the change is for admins.
	ErrForbidden = errors.New("forbidden")

	// errRetry marks a failure that rerunning the write resolves.
	errRetry = errors.New("retry")
)

// retryable reports whether rerunning the write may succeed: err is
// errRetry, or Dolt's signal that a concurrent transaction won, which is
// a serialization failure (1213), a lock wait timeout (1205), or a
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

// isDuplicate reports whether err is a duplicate key error (1062).
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

// StateError refuses a change that does not fit the issue's state, for
// the Reason given. It wraps ErrInvalid, and its text starts with
// ErrInvalid's.
type StateError struct {
	ID     IssueID
	Reason StateReason
	// Holder holds the issue's claim, when Reason is StateClaimed.
	Holder Actor
}

// StateReason says why a [*StateError] refused a change.
type StateReason string

// The reasons a [*StateError] gives.
const (
	// StateClaimed: a status or assignee change to an issue under a live
	// claim, which changes them only through finish, close or a releasing
	// handoff.
	StateClaimed StateReason = "claimed"
	// StateClosed: a change that needs the issue open, such as a status
	// change, a start, a release or an acceptance change.
	StateClosed StateReason = "closed"
	// StateAlreadyClosed: a close or finish of a closed issue.
	StateAlreadyClosed StateReason = "already closed"
	// StateNotClosed: a reopen of an issue that is not closed.
	StateNotClosed StateReason = "not closed"
)

func (e *StateError) Error() string {
	switch e.Reason {
	case StateClaimed:
		return fmt.Sprintf("%v: issue %s is claimed by %s/%s; %s", ErrInvalid, e.ID, e.Holder.Principal, e.Holder.Session, errClaimedFields)
	case StateClosed:
		return fmt.Sprintf("%v: issue %s is closed; reopen it first", ErrInvalid, e.ID)
	case StateAlreadyClosed:
		return fmt.Sprintf("%v: issue %s is already closed", ErrInvalid, e.ID)
	}
	return fmt.Sprintf("%v: issue %s is not closed", ErrInvalid, e.ID)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *StateError) Unwrap() error { return ErrInvalid }
