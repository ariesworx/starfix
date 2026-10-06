package store

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// IssueID is "<prefix>-<suffix>". New IDs have an 8-character suffix (40
// random bits, lowercase base32); imported bd IDs keep their own form.
type IssueID string

var (
	idEncoding    = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
	prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	idPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*-[a-z0-9]+(\.[0-9]+)*$`)
)

// minShort is the shortest suffix ShortestUnique returns, so display IDs
// stay stable while a project is small.
const minShort = 4

// NewID returns a random ID with the given prefix.
func NewID(prefix string) (IssueID, error) {
	if len(prefix) > 32 || !prefixPattern.MatchString(prefix) {
		return "", fmt.Errorf("%w: prefix %q must be lowercase letters, digits and inner hyphens, at most 32", ErrInvalid, prefix)
	}
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("new id: %w", err)
	}
	return IssueID(prefix + "-" + idEncoding.EncodeToString(b[:])), nil
}

// Validate checks the ID's shape.
func (id IssueID) Validate() error {
	if len(id) > 64 || !idPattern.MatchString(string(id)) {
		return fmt.Errorf("%w: issue id %q", ErrInvalid, string(id))
	}
	return nil
}

// split returns the prefix and the suffix after the last hyphen.
func (id IssueID) split() (string, string) {
	s := string(id)
	i := strings.LastIndexByte(s, '-')
	if i < 0 {
		return "", s
	}
	return s[:i], s[i+1:]
}

// ShortestUnique maps each ID to its display form: the prefix plus the
// shortest suffix prefix (at least four characters) that no other ID in ids
// shares.
func ShortestUnique(ids []IssueID) map[IssueID]string {
	groups := map[string][]string{}
	for _, id := range ids {
		p, s := id.split()
		groups[p] = append(groups[p], s)
	}
	out := make(map[IssueID]string, len(ids))
	for p, suffixes := range groups {
		sort.Strings(suffixes)
		for i, s := range suffixes {
			need := 0
			if i > 0 {
				need = max(need, commonPrefix(s, suffixes[i-1]))
			}
			if i+1 < len(suffixes) {
				need = max(need, commonPrefix(s, suffixes[i+1]))
			}
			n := min(max(need+1, minShort), len(s))
			short := s[:n]
			if p != "" {
				short = p + "-" + short
			}
			out[IssueID(joinID(p, s))] = short
		}
	}
	return out
}

func joinID(p, s string) string {
	if p == "" {
		return s
	}
	return p + "-" + s
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// randomInt63 returns a random non-negative int64, used for write_id.
func randomInt63() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, fmt.Errorf("random: %w", err)
	}
	return int64(binary.BigEndian.Uint64(b[:]) >> 1), nil //nolint:gosec // shifted right, fits in int63
}

// newCommentID returns a random 16-character comment ID.
func newCommentID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("comment id: %w", err)
	}
	return idEncoding.EncodeToString(b[:]), nil
}
