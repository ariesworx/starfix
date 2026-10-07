package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The acceptance checklist (design §12 item 4): an issue's acceptance
// criteria are items, parsed from its acceptance text on every read. A
// Markdown list (-, *, + or 1. and 1), with or without [ ] and [x]) gives
// one item per entry; other text is one item; no text, no items. What
// people did to the items (ticked, waived with a reason, or unticked) is
// stored per item text in acceptance_state, so it follows an item that
// moves and lapses when its text changes. Close and finish refuse while
// an item is neither ticked nor waived.

// ItemState is an acceptance item's state.
type ItemState string

// Item states.
const (
	ItemOpen   ItemState = ""
	ItemTicked ItemState = "ticked"
	ItemWaived ItemState = "waived"
)

// Acceptance event operations.
const (
	OpAcceptTick   Op = "acceptance.tick"
	OpAcceptUntick Op = "acceptance.untick"
	OpAcceptWaive  Op = "acceptance.waive"
)

// AcceptanceItem is one acceptance criterion and its state. N counts from
// 1. By and At say who set the state and when; an item ticked in the text
// itself ("- [x] …") has neither.
type AcceptanceItem struct {
	N      int        `json:"n"`
	Text   string     `json:"text"`
	State  ItemState  `json:"state,omitempty"`
	Reason string     `json:"reason,omitempty"`
	By     string     `json:"by,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

// Acceptance changes items by number: Untick reopens them, Tick ticks
// them, Waive waives them with a reason.
type Acceptance struct {
	Tick   []int
	Untick []int
	Waive  map[int]string
}

func (a Acceptance) empty() bool { return len(a.Tick) == 0 && len(a.Untick) == 0 && len(a.Waive) == 0 }

// validate checks the numbers and reasons; each item may appear once.
func (a Acceptance) validate() error {
	seen := map[int]bool{}
	check := func(n int) error {
		if n < 1 {
			return fmt.Errorf("%w: acceptance items are numbered from 1, not %d", ErrInvalid, n)
		}
		if seen[n] {
			return fmt.Errorf("%w: acceptance item %d is given twice", ErrInvalid, n)
		}
		seen[n] = true
		return nil
	}
	for _, n := range slices.Concat(a.Untick, a.Tick) {
		if err := check(n); err != nil {
			return err
		}
	}
	for n, r := range a.Waive {
		if err := check(n); err != nil {
			return err
		}
		if checkLine("waive reason", r, 500, true) != nil {
			return fmt.Errorf("%w: waiving acceptance item %d needs a reason of one line, up to 500 bytes", ErrInvalid, n)
		}
	}
	return nil
}

// AcceptanceError refuses closing an issue whose acceptance items Open
// (numbers) are neither ticked nor waived. It wraps ErrInvalid.
type AcceptanceError struct {
	ID   IssueID
	Open []int
}

func (e *AcceptanceError) Error() string {
	return fmt.Sprintf("issue %s has acceptance items neither ticked nor waived: %s", e.ID, joinInts(e.Open))
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *AcceptanceError) Unwrap() error { return ErrInvalid }

func joinInts(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = fmt.Sprint(n)
	}
	return strings.Join(s, ", ")
}

// parsedItem is one item of acceptance text.
type parsedItem struct {
	text   string
	ticked bool
}

// listItem is a Markdown list entry: a bullet or number, then an optional
// task box, then the text.
var listItem = regexp.MustCompile(`^\s*(?:[-*+]|\d{1,9}[.)])(?:\s+|$)(?:\[([ xX])\](?:\s+|$))?(.*)$`)

// parseAcceptance splits acceptance text into items. A Markdown list
// gives one item per entry, an indented line continuing the entry above;
// other lines around a list are not items. Text without a list is one
// item. Whitespace inside an item collapses to single spaces.
func parseAcceptance(text string) []parsedItem {
	var items []parsedItem
	list := false
	last := -1 // the entry an indented line continues, or -1
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := listItem.FindStringSubmatch(line); m != nil {
			list = true
			t := collapse(m[2])
			if t == "" {
				last = -1
				continue
			}
			items = append(items, parsedItem{text: t, ticked: m[1] == "x" || m[1] == "X"})
			last = len(items) - 1
			continue
		}
		switch {
		case strings.TrimSpace(line) == "":
		case last >= 0 && (line[0] == ' ' || line[0] == '\t'):
			items[last].text += " " + collapse(line)
		default:
			last = -1
		}
	}
	if list {
		return items
	}
	if t := collapse(text); t != "" {
		return []parsedItem{{text: t}}
	}
	return nil
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func itemKey(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// AcceptanceItems returns an issue's acceptance items and their state, in
// order; nil when it has no criteria.
func (s *Store) AcceptanceItems(ctx context.Context, id IssueID) ([]AcceptanceItem, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	is, err := loadIssue(ctx, s.r, id)
	if err != nil {
		return nil, err
	}
	return acceptanceItems(ctx, s.r, is)
}

// acceptanceItems parses is's criteria and applies their stored state.
func acceptanceItems(ctx context.Context, q querier, is Issue) ([]AcceptanceItem, error) {
	parsed := parseAcceptance(is.Acceptance)
	if len(parsed) == 0 {
		return nil, nil
	}
	type row struct {
		state, reason, by string
		at                time.Time
	}
	rows, err := q.QueryContext(ctx, `SELECT item_key, state, reason, by_principal, at FROM acceptance_state WHERE issue_id = ?`,
		string(is.ID))
	if err != nil {
		return nil, fmt.Errorf("acceptance state of %s: %w", is.ID, err)
	}
	defer func() { _ = rows.Close() }()
	set := map[string]row{}
	for rows.Next() {
		var k string
		var r row
		if err := rows.Scan(&k, &r.state, &r.reason, &r.by, &r.at); err != nil {
			return nil, fmt.Errorf("acceptance state of %s: %w", is.ID, err)
		}
		set[k] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("acceptance state of %s: %w", is.ID, err)
	}
	items := make([]AcceptanceItem, len(parsed))
	for i, p := range parsed {
		it := AcceptanceItem{N: i + 1, Text: p.text}
		if p.ticked {
			it.State = ItemTicked
		}
		if r, ok := set[itemKey(p.text)]; ok {
			it.State, it.Reason, it.By = ItemState(r.state), r.reason, r.by
			if it.State == "open" {
				it.State = ItemOpen
			}
			at := r.at.UTC()
			it.At = &at
		}
		items[i] = it
	}
	return items, nil
}

// openItems lists the numbers of the items neither ticked nor waived.
func openItems(items []AcceptanceItem) []int {
	var open []int
	for _, it := range items {
		if it.State == ItemOpen {
			open = append(open, it.N)
		}
	}
	return open
}

// Accept ticks, unticks and waives an issue's acceptance items and
// returns them all. Changing an item to the state it has is a no-op. A
// closed issue is refused: reopen it first.
func (s *Store) Accept(ctx context.Context, actor Actor, id IssueID, a Acceptance) ([]AcceptanceItem, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if a.empty() {
		return nil, fmt.Errorf("%w: name an acceptance item to tick, untick or waive", ErrInvalid)
	}
	if err := a.validate(); err != nil {
		return nil, err
	}
	var out []AcceptanceItem
	err := s.write(ctx, actor, func(w *wtx) error {
		is, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if is.Status == StatusClosed {
			return fmt.Errorf("%w: issue %s is closed; reopen it first", ErrInvalid, id)
		}
		out, err = applyAcceptance(ctx, w, is, a)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// applyAcceptance makes a's changes to is's items in w, with an event per
// item changed, and returns the items as they now stand.
func applyAcceptance(ctx context.Context, w *wtx, is Issue, a Acceptance) ([]AcceptanceItem, error) {
	items, err := acceptanceItems(ctx, w.tx, is)
	if err != nil {
		return nil, err
	}
	for _, n := range slices.Concat(a.Untick, a.Tick, mapKeys(a.Waive)) {
		if n > len(items) {
			return nil, fmt.Errorf("%w: issue %s has %d acceptance items; there is no item %d", ErrInvalid, is.ID, len(items), n)
		}
	}
	type change struct {
		n      int
		state  ItemState
		reason string
		op     Op
	}
	var changes []change
	for _, n := range a.Untick {
		changes = append(changes, change{n, ItemOpen, "", OpAcceptUntick})
	}
	for _, n := range a.Tick {
		changes = append(changes, change{n, ItemTicked, "", OpAcceptTick})
	}
	for _, n := range mapKeys(a.Waive) {
		changes = append(changes, change{n, ItemWaived, a.Waive[n], OpAcceptWaive})
	}
	for _, c := range changes {
		it := &items[c.n-1]
		if it.State == c.state && it.Reason == c.reason {
			continue
		}
		stored := string(c.state)
		if c.state == ItemOpen {
			stored = "open"
		}
		if err := setItemState(ctx, w, is.ID, *it, stored, c.reason); err != nil {
			return nil, err
		}
		after := map[string]any{"n": it.N, "item": it.Text}
		if c.reason != "" {
			after["reason"] = c.reason
		}
		if err := w.event(ctx, c.op, string(is.ID), nil, after); err != nil {
			return nil, err
		}
		at := w.now
		it.State, it.Reason, it.By, it.At = c.state, c.reason, w.actor.Principal, &at
	}
	return items, nil
}

// setItemState writes one item's state row.
func setItemState(ctx context.Context, w *wtx, id IssueID, it AcceptanceItem, state, reason string) error {
	wid, err := randomInt63()
	if err != nil {
		return err
	}
	key := itemKey(it.Text)
	var n int
	err = w.tx.QueryRowContext(ctx, `SELECT n FROM acceptance_state WHERE issue_id = ? AND item_key = ?`, string(id), key).Scan(&n)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = w.exec(ctx, `INSERT INTO acceptance_state (issue_id, item_key, n, state, reason, by_principal, at, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, string(id), key, it.N, state, reason, w.actor.Principal, w.now, wid)
	case err == nil:
		_, err = w.exec(ctx, `UPDATE acceptance_state SET n = ?, state = ?, reason = ?, by_principal = ?, at = ?, write_id = ?
  WHERE issue_id = ? AND item_key = ?`, it.N, state, reason, w.actor.Principal, w.now, wid, string(id), key)
	}
	if err != nil {
		return fmt.Errorf("acceptance item %d of %s: %w", it.N, id, err)
	}
	return nil
}

// mapKeys returns m's keys in order.
func mapKeys(m map[int]string) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
