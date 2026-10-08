package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
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
//
// A box ticked in the text ("- [x] …") counts only when the issue is
// created or imported, where it is stored as ticked by the creator; after
// that the text's boxes are ignored, so editing the text cannot tick an
// item, and an edit that drops an item still open is refused
// (checkDropped). Ticks go through Accept or finish, each on the record.

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
// at create has the creator and the create time.
type AcceptanceItem struct {
	N      int        `json:"n"`
	Text   string     `json:"text"`
	State  ItemState  `json:"state,omitempty"`
	Reason string     `json:"reason,omitempty"`
	By     string     `json:"by,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

// Acceptance changes items by number, counting from 1: Untick reopens
// them, Tick ticks them, Waive waives them with a reason. An item may
// appear only once across the three.
type Acceptance struct {
	Tick   []int
	Untick []int
	Waive  map[int]string
}

// empty reports whether a changes no item.
func (a Acceptance) empty() bool { return len(a.Tick) == 0 && len(a.Untick) == 0 && len(a.Waive) == 0 }

// validate refuses, with ErrInvalid, numbers below 1 or past most (the
// most items an issue may have), an item given twice, more than most
// numbers in all, and a waiver without a one-line reason of up to 500
// bytes.
func (a Acceptance) validate(most int) error {
	if len(a.Tick)+len(a.Untick)+len(a.Waive) > most {
		return fmt.Errorf("%w: an issue has at most %d acceptance items, so name at most that many", ErrInvalid, most)
	}
	seen := map[int]bool{}
	check := func(n int) error {
		if n < 1 {
			return fmt.Errorf("%w: acceptance items are numbered from 1, not %d", ErrInvalid, n)
		}
		if n > most {
			return fmt.Errorf("%w: an issue has at most %d acceptance items; there is no item %d", ErrInvalid, most, n)
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
// (numbers) are neither ticked nor waived, or (Dropped) an update whose
// acceptance text leaves out those items while they are open. It wraps
// ErrInvalid.
type AcceptanceError struct {
	ID      IssueID
	Open    []int
	Dropped bool
}

func (e *AcceptanceError) Error() string {
	if e.Dropped {
		return fmt.Sprintf("the new acceptance text of %s drops items neither ticked nor waived: %s", e.ID, joinInts(e.Open))
	}
	return fmt.Sprintf("issue %s has acceptance items neither ticked nor waived: %s", e.ID, joinInts(e.Open))
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *AcceptanceError) Unwrap() error { return ErrInvalid }

// joinInts formats ns as a comma-separated list.
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

// collapse turns every run of whitespace in s into one space, and trims
// the ends.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// itemKey is an item's key in acceptance_state: the SHA-256 of its text,
// in hex, so its state follows the text and not the item's number.
func itemKey(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// AcceptanceItems returns an issue's acceptance items and their state, in
// order, from one snapshot; nil when it has no criteria. A missing issue
// is ErrNotFound.
func (s *Store) AcceptanceItems(ctx context.Context, id IssueID) ([]AcceptanceItem, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("acceptance items of %s: %w", id, err)
	}
	defer end()
	is, err := loadIssue(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	return acceptanceItems(ctx, tx, is)
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
		if r, ok := set[itemKey(p.text)]; ok {
			it.State, it.Reason, it.By = ItemState(r.state), r.reason, r.by
			// A stored "open" is an untick: ItemOpen, with who and when.
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

// tickInText stores the items text ticks ("- [x] …") as ticked by w's
// actor, for an issue being created or imported. An item with a stored
// state keeps it.
func tickInText(ctx context.Context, w *wtx, id IssueID, text string) error {
	for i, p := range parseAcceptance(text) {
		if !p.ticked {
			continue
		}
		var n int
		err := w.tx.QueryRowContext(ctx, `SELECT n FROM acceptance_state WHERE issue_id = ? AND item_key = ?`,
			string(id), itemKey(p.text)).Scan(&n)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("acceptance item %d of %s: %w", i+1, id, err)
		}
		if err := setItemState(ctx, w, id, AcceptanceItem{N: i + 1, Text: p.text}, string(ItemTicked), ""); err != nil {
			return err
		}
	}
	return nil
}

// checkDropped refuses new acceptance text for is that leaves out an item
// still open, with an *AcceptanceError (Dropped) naming them.
func checkDropped(ctx context.Context, q querier, is Issue, text string) error {
	items, err := acceptanceItems(ctx, q, is)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, p := range parseAcceptance(text) {
		keep[p.text] = true
	}
	var dropped []int
	for _, it := range items {
		if it.State == ItemOpen && !keep[it.Text] {
			dropped = append(dropped, it.N)
		}
	}
	if len(dropped) > 0 {
		return &AcceptanceError{ID: is.ID, Open: dropped, Dropped: true}
	}
	return nil
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

// Accept ticks, unticks and waives an issue's acceptance items, with an
// event for each item it changes, and returns them all. Changing an item
// to the state it has is a no-op. An empty a, a number past the issue's
// items, and a closed issue (reopen it first) are refused with
// ErrInvalid; an issue another principal holds with a [*ForbiddenError]
// unless the actor is an admin.
func (s *Store) Accept(ctx context.Context, actor Actor, id IssueID, a Acceptance) ([]AcceptanceItem, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	if a.empty() {
		return nil, fmt.Errorf("%w: name an acceptance item to tick, untick or waive", ErrInvalid)
	}
	if err := a.validate(s.opts.Limits.AcceptanceItems); err != nil {
		return nil, err
	}
	var out []AcceptanceItem
	err := s.write(ctx, actor, func(w *wtx) error {
		is, err := loadIssue(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if is.Status == StatusClosed {
			return &StateError{ID: id, Reason: StateClosed}
		}
		c, err := loadClaim(ctx, w.tx, id)
		if err != nil {
			return err
		}
		if err := w.guard(ctx, c, "accept"); err != nil {
			return err
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
		// ItemOpen is the empty string, but acceptance_state spells the
		// state "open" (migration 0009).
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

// setItemState writes one item's state row, as set by w's actor now.
func setItemState(ctx context.Context, w *wtx, id IssueID, it AcceptanceItem, state, reason string) error {
	wid := randomInt63()
	key := itemKey(it.Text)
	var n int
	err := w.tx.QueryRowContext(ctx, `SELECT n FROM acceptance_state WHERE issue_id = ? AND item_key = ?`, string(id), key).Scan(&n)
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
	return slices.Sorted(maps.Keys(m))
}

// checkItems refuses acceptance text with more than most items.
func checkItems(text string, most int) error {
	if n := len(parseAcceptance(text)); n > most {
		return fmt.Errorf("%w: acceptance text has %d items; an issue has at most %d acceptance items", ErrInvalid, n, most)
	}
	return nil
}
