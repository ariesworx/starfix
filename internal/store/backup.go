package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

// backupTagPattern bounds a backup tag name to what Dolt accepts as a ref.
var backupTagPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,99}$`)

// BackupTag makes a Dolt commit of everything written so far, by this
// store or any other session, and tags it name. It returns the tag made.
//
// It is idempotent: when name already tags the current commit nothing
// changes. When name tags an older commit (an earlier upgrade from the
// same version that was rolled back) that tag is kept and a new one,
// name-<UTC time>, is made, so no restore point is lost.
func (s *Store) BackupTag(ctx context.Context, name string) (string, error) {
	if !backupTagPattern.MatchString(name) {
		return "", fmt.Errorf("%w: backup tag %q", ErrInvalid, name)
	}
	c, err := s.w.Conn(ctx)
	if err != nil {
		return "", fmt.Errorf("backup tag: %w", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.ExecContext(ctx, `CALL DOLT_COMMIT('-Am', ?, '--skip-empty')`, "starfix: backup before upgrade"); err != nil {
		return "", fmt.Errorf("backup commit: %w", err)
	}
	var head string
	if err := c.QueryRowContext(ctx, `SELECT HASHOF('HEAD')`).Scan(&head); err != nil {
		return "", fmt.Errorf("backup tag: %w", err)
	}
	var tagged string
	err = c.QueryRowContext(ctx, `SELECT tag_hash FROM dolt_tags WHERE tag_name = ?`, name).Scan(&tagged)
	switch {
	case err == nil && tagged == head:
		return name, nil
	case err == nil:
		name += "-" + s.now().Format("20060102t150405z")
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("backup tag: %w", err)
	}
	if _, err := c.ExecContext(ctx, `CALL DOLT_TAG(?, 'HEAD')`, name); err != nil {
		return "", fmt.Errorf("backup tag %s: %w", name, err)
	}
	return name, nil
}
