package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Similar closed issues (design §12 item 6): create and show list the
// closed issues whose titles share the most words with the issue's.
//
// Dolt's FULLTEXT index was the plan, but on the pinned Dolt a MATCH in a
// WHERE clause returns rows more than once and boolean mode is missing
// (TestDoltFulltextIsUnreliable), so titles are scored here instead: the
// most recently closed SimilarScan issues, by the Jaccard overlap of their
// title words after dropping stopwords and a light stemming. Vectors (§10)
// replace this later.

// SimilarScan bounds how many recently closed issues SimilarClosed reads.
const SimilarScan = 2000

// MaxSimilar is the most similar issues SimilarClosed returns.
const MaxSimilar = 3

// SimilarIssue is a closed issue like another, with its overlap score
// (0 to 1).
type SimilarIssue struct {
	ID       IssueID
	Title    string
	Priority Priority
	Score    float64
}

// SimilarClosed returns up to limit (at most MaxSimilar) closed issues
// whose titles resemble title, best first, leaving out exclude. An issue
// counts when it shares two words with title, or one word that is half
// of both titles' words together.
func (s *Store) SimilarClosed(ctx context.Context, title string, exclude IssueID, limit int) ([]SimilarIssue, error) {
	limit = min(max(limit, 1), MaxSimilar)
	want := titleTokens(title)
	if len(want) == 0 {
		return nil, nil
	}
	rows, err := s.r.QueryContext(ctx, `SELECT id, title, priority FROM issues
  WHERE status = ? AND template = FALSE ORDER BY closed_at DESC LIMIT ?`, string(StatusClosed), SimilarScan)
	if err != nil {
		return nil, fmt.Errorf("similar issues: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SimilarIssue
	for rows.Next() {
		var is SimilarIssue
		if err := rows.Scan(&is.ID, &is.Title, &is.Priority); err != nil {
			return nil, fmt.Errorf("similar issues: %w", err)
		}
		if is.ID == exclude {
			continue
		}
		shared, union := overlap(want, titleTokens(is.Title))
		is.Score = float64(shared) / float64(union)
		if shared >= 2 || (shared == 1 && is.Score >= 0.5) {
			out = append(out, is)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("similar issues: %w", err)
	}
	// Stable: equal scores keep the most recently closed first.
	slices.SortStableFunc(out, func(a, b SimilarIssue) int { return cmp.Compare(b.Score, a.Score) })
	return out[:min(len(out), limit)], nil
}

// overlap counts the tokens a and b share and the tokens of both.
func overlap(a, b []string) (shared, union int) {
	for _, t := range b {
		if slices.Contains(a, t) {
			shared++
		}
	}
	return shared, len(a) + len(b) - shared
}

// titleTokens returns a title's distinct words, in order: lowercased,
// split on anything but letters and digits, without one-letter words and
// stopwords, lightly stemmed.
func titleTokens(title string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) < 2 || stopwords[w] {
			continue
		}
		if w = stem(w); !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// stem strips a plural or tense ending, then a final e or y, so expire,
// expires, expired and expiry agree. It leaves at least three letters.
func stem(w string) string {
	if len(w) < 4 {
		return w
	}
	for _, suf := range []string{"ing", "ed", "s"} {
		if strings.HasSuffix(w, suf) && len(w)-len(suf) >= 3 && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-len(suf)]
			break
		}
	}
	if (strings.HasSuffix(w, "e") || strings.HasSuffix(w, "y")) && len(w) > 3 {
		w = w[:len(w)-1]
	}
	return w
}

// stopwords are words too common in issue titles to say two are alike.
var stopwords = map[string]bool{}

func init() {
	for w := range strings.FieldsSeq(`a an the and or but of to in on at by for from with without into onto via as is are be
		been was were it its this that these those not no can should must when if then than so do does
		fix fixes add adds added update updates remove make use support allow handle implement improve change new`) {
		stopwords[w] = true
	}
}
