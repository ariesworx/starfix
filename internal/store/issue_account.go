package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

// Accounts (design §12.1). An issue's account is what its time and tokens
// are reported against: a client's engagement code name or an internal
// department, never a client's real name. An issue without one inherits
// its parent's, up the chain; starfixd's account: setting covers the
// rest. Code names only, so the pattern admits nothing but lowercase
// letters, digits and inner hyphens.

// accountPattern is a code name: runs of lowercase letters and digits
// joined by single hyphens.
var accountPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// maxAccount bounds an account name; the column holds 64.
const maxAccount = 64

// ValidAccount reports whether a may be an account: a code name of 1-64
// lowercase letters, digits and inner hyphens.
func ValidAccount(a string) bool { return len(a) <= maxAccount && accountPattern.MatchString(a) }

// validAccount is ValidAccount as an error wrapping ErrInvalid; empty,
// which inherits, is valid.
func validAccount(a string) error {
	if a != "" && !ValidAccount(a) {
		return fmt.Errorf("%w: account %q must be a code name: 1-64 lowercase letters, digits and inner hyphens", ErrInvalid, a)
	}
	return nil
}

// maxParentDepth bounds the walk up a parent chain. Parent links cannot
// form a cycle (checkEdge), so this only stops a corrupt database from
// looping.
const maxParentDepth = 100

// ResolveAccount returns the account the issue id reports against and
// the issue that sets it: id itself, or the nearest ancestor with one.
// It returns "" when no issue in the chain sets one, and the project
// default applies. An issue that does not exist is ErrNotFound.
func (s *Store) ResolveAccount(ctx context.Context, id IssueID) (string, IssueID, error) {
	if err := id.Validate(); err != nil {
		return "", "", err
	}
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return "", "", err
	}
	defer end()
	return resolveAccount(ctx, tx, id)
}

// resolveAccount is ResolveAccount on q.
func resolveAccount(ctx context.Context, q querier, id IssueID) (string, IssueID, error) {
	cur := id
	for range maxParentDepth {
		var account, parent sql.NullString
		err := q.QueryRowContext(ctx, `SELECT account, parent_id FROM issues WHERE id = ?`, string(cur)).Scan(&account, &parent)
		if errors.Is(err, sql.ErrNoRows) {
			if cur == id {
				return "", "", fmt.Errorf("issue %s: %w", id, ErrNotFound)
			}
			return "", "", nil // a dangling parent link sets nothing
		}
		if err != nil {
			return "", "", fmt.Errorf("account of %s: %w", id, err)
		}
		if account.String != "" {
			return account.String, cur, nil
		}
		if parent.String == "" {
			return "", "", nil
		}
		cur = IssueID(parent.String)
	}
	return "", "", nil
}
