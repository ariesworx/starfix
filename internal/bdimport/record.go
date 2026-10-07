package bdimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/safetext"
	"github.com/ariesworx/starfix/internal/store"
)

// bdIssue is one issue line of bd's JSONL export (bd 1.2.2,
// types.IssueWithCounts plus "_type"). expires_at and a comment's session
// are starfix additions that bd ignores, so a starfix export round-trips.
type bdIssue struct {
	ID                 string          `json:"id"`
	Title              string          `json:"title"`
	Description        string          `json:"description,omitempty"`
	Design             string          `json:"design,omitempty"`
	AcceptanceCriteria string          `json:"acceptance_criteria,omitempty"`
	Notes              string          `json:"notes,omitempty"`
	Status             string          `json:"status,omitempty"`
	Priority           *int            `json:"priority,omitempty"`
	IssueType          string          `json:"issue_type,omitempty"`
	Assignee           string          `json:"assignee,omitempty"`
	Owner              string          `json:"owner,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	CreatedBy          string          `json:"created_by,omitempty"`
	UpdatedAt          time.Time       `json:"updated_at"`
	ClosedAt           *time.Time      `json:"closed_at,omitempty"`
	CloseReason        string          `json:"close_reason,omitempty"`
	DueAt              *time.Time      `json:"due_at,omitempty"`
	DeferUntil         *time.Time      `json:"defer_until,omitempty"`
	ExpiresAt          *time.Time      `json:"expires_at,omitempty"`
	Ephemeral          bool            `json:"ephemeral,omitempty"`
	Wisp               bool            `json:"wisp,omitempty"`
	Pinned             bool            `json:"pinned,omitempty"`
	IsTemplate         bool            `json:"is_template,omitempty"`
	Metadata           json.RawMessage `json:"metadata,omitempty"`
	Labels             []string        `json:"labels,omitempty"`
	Dependencies       []bdDep         `json:"dependencies,omitempty"`
	Comments           []bdComment     `json:"comments,omitempty"`
}

// bdDep is an outgoing edge: IssueID depends on DependsOnID. bd writes
// its metadata as a JSON string holding JSON.
type bdDep struct {
	IssueID     string          `json:"issue_id"`
	DependsOnID string          `json:"depends_on_id"`
	Type        string          `json:"type"`
	CreatedAt   time.Time       `json:"created_at"`
	CreatedBy   string          `json:"created_by,omitempty"`
	Metadata    json.RawMessage `json:"metadata,omitempty"`
	ThreadID    string          `json:"thread_id,omitempty"`
}

// bdComment is a comment. Before bd 1.0 the ID was a number.
type bdComment struct {
	ID        json.RawMessage `json:"id"`
	IssueID   string          `json:"issue_id"`
	Author    string          `json:"author"`
	Text      string          `json:"text"`
	CreatedAt time.Time       `json:"created_at"`
	Session   string          `json:"session,omitempty"`
}

// handled are the issue keys this package reads.
var handled = map[string]bool{
	"_type": true, "id": true, "title": true, "description": true, "design": true,
	"acceptance_criteria": true, "notes": true, "status": true, "priority": true,
	"issue_type": true, "assignee": true, "owner": true, "created_at": true,
	"created_by": true, "updated_at": true, "closed_at": true, "close_reason": true,
	"due_at": true, "defer_until": true, "expires_at": true, "ephemeral": true,
	"wisp": true, "pinned": true, "is_template": true, "metadata": true,
	"labels": true, "dependencies": true, "comments": true,
}

// derived are keys bd computes on export; they carry nothing to keep.
var derived = map[string]bool{
	"is_blocked": true, "dependency_count": true, "dependent_count": true,
	"comment_count": true, "parent": true, "comments_omitted": true,
}

// unheld lists the keys of an issue line that hold a value the store has
// no place for, sorted.
func unheld(raw map[string]json.RawMessage) []string {
	var out []string
	for k, v := range raw {
		if handled[k] || derived[k] || empty(v) {
			continue
		}
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// empty reports whether v holds nothing worth keeping: nothing, null, or
// a zero value.
func empty(v json.RawMessage) bool {
	switch string(bytes.TrimSpace(v)) {
	case "", "null", `""`, "0", "false", "[]", "{}":
		return true
	}
	return false
}

// decodeMetadata accepts metadata as a JSON value or, as bd writes it for
// dependencies, a JSON string holding JSON. Empty and null mean none; "{}"
// on a dependency (bd's empty value) means none too.
func decodeMetadata(raw json.RawMessage, emptyObjectIsNone bool) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("metadata: %w", err)
		}
		raw = json.RawMessage(strings.TrimSpace(s))
		if len(raw) == 0 {
			return nil, nil
		}
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("metadata is not valid JSON")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if emptyObjectIsNone && buf.String() == "{}" {
		return nil, nil
	}
	return json.RawMessage(buf.Bytes()), nil
}

// coreStatus maps the statuses bd and starfix share; mapIssue maps the
// rest.
var coreStatus = map[string]store.Status{
	"": store.StatusOpen, "open": store.StatusOpen, "in_progress": store.StatusInProgress,
	"blocked": store.StatusBlocked, "deferred": store.StatusDeferred, "closed": store.StatusClosed,
}

// coreType maps the issue types bd and starfix share. mapIssue stores
// any other as a task, labeled "bd-type:<name>" when that is a valid label.
var coreType = map[string]store.IssueType{
	"": store.TypeTask, "bug": store.TypeBug, "feature": store.TypeFeature,
	"task": store.TypeTask, "epic": store.TypeEpic, "chore": store.TypeChore,
}

// commentIDShape is the form starfix gives comment IDs.
var commentIDShape = regexp.MustCompile(`^[a-z2-7]{16}$`)

// idEncoding spells comment IDs in commentIDShape's alphabet.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// commentID maps a bd comment ID to a starfix one. bd IDs are UUIDs (once
// integers, unique only per database), longer than the column, so they are
// hashed with the issue ID: the same comment always maps to the same ID.
func commentID(issue store.IssueID, raw json.RawMessage, c bdComment) string {
	var id string
	if err := json.Unmarshal(raw, &id); err != nil {
		var n json.Number
		if json.Unmarshal(raw, &n) == nil {
			id = n.String()
		}
	}
	if commentIDShape.MatchString(id) {
		return id
	}
	if id == "" {
		// No ID at all: derive one from the content, which is what makes
		// a comment the same comment.
		id = c.Author + "\x00" + c.CreatedAt.UTC().Format(time.RFC3339Nano) + "\x00" + c.Text
	}
	sum := sha256.Sum256([]byte("bd-comment\x00" + string(issue) + "\x00" + id))
	return idEncoding.EncodeToString(sum[:10])
}

// line is one parsed issue line, mapped to the store's types.
type line struct {
	n     int // the line number
	issue store.Issue
	// parents are its parent-child targets; the first is used.
	parents []store.IssueID
	// deps are its other outgoing dependencies.
	deps     []depRef
	comments []store.Comment
}

// depRef is a dependency with the type bd gave it, which messages name
// when the store's differs or the store has none.
type depRef struct {
	dep    store.Dep
	bdType string
}

// mapIssue converts a bd issue. warn records a mapping the caller reports.
func mapIssue(b bdIssue, n int, principal string, warn func(kind, detail string)) (line, error) {
	id := store.IssueID(b.ID)
	if b.ID == "" {
		return line{}, fmt.Errorf("issue has no id")
	}
	if err := id.Validate(); err != nil {
		return line{}, fmt.Errorf("id %q is not a valid starfix id (lowercase prefix, hyphen, lowercase suffix, optional .N parts)", b.ID)
	}
	is := store.Issue{
		ID: id, Title: b.Title, Body: b.Description, Design: b.Design,
		Acceptance: b.AcceptanceCriteria, Notes: b.Notes, Assignee: b.Assignee, Owner: b.Owner,
		CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt, UpdatedAt: b.UpdatedAt, ClosedAt: b.ClosedAt,
		CloseReason: b.CloseReason, DueAt: b.DueAt, DeferUntil: b.DeferUntil, ExpiresAt: b.ExpiresAt,
		Ephemeral: b.Ephemeral || b.Wisp, Pinned: b.Pinned, Template: b.IsTemplate,
		Labels: slices.Clone(b.Labels),
	}
	if is.CreatedBy == "" {
		is.CreatedBy = principal
	}
	if is.CreatedAt.IsZero() {
		if b.UpdatedAt.IsZero() {
			return line{}, fmt.Errorf("issue %s has neither created_at nor updated_at", id)
		}
		is.CreatedAt = b.UpdatedAt
		warn("no-created-at", "")
	}
	if is.UpdatedAt.IsZero() {
		is.UpdatedAt = is.CreatedAt
	}

	switch st, ok := coreStatus[b.Status]; {
	case ok:
		is.Status = st
	case b.Status == "pinned":
		is.Status, is.Pinned = store.StatusOpen, true
		warn("status", "pinned")
	case b.Status == "hooked":
		is.Status = store.StatusInProgress
		is.Labels = append(is.Labels, "bd-status:hooked")
		warn("status", b.Status)
	default:
		is.Status = store.StatusOpen
		if b.ClosedAt != nil {
			is.Status = store.StatusClosed
		}
		if l := "bd-status:" + b.Status; validLabel(l) {
			is.Labels = append(is.Labels, l)
		}
		warn("status", b.Status)
	}

	if t, ok := coreType[b.IssueType]; ok {
		is.Type = t
	} else {
		is.Type = store.TypeTask
		if l := "bd-type:" + b.IssueType; validLabel(l) {
			is.Labels = append(is.Labels, l)
		}
		warn("type", b.IssueType)
	}

	is.Priority = store.P2
	if b.Priority != nil {
		p := store.Priority(*b.Priority)
		switch {
		case p < store.P0:
			p = store.P0
			warn("priority", strconv.Itoa(*b.Priority))
		case p > store.P4:
			p = store.P4
			warn("priority", strconv.Itoa(*b.Priority))
		}
		is.Priority = p
	}

	meta, err := decodeMetadata(b.Metadata, false)
	if err != nil {
		return line{}, fmt.Errorf("issue %s: %w", id, err)
	}
	is.Metadata = meta

	var keep []string
	for _, l := range is.Labels {
		if validLabel(l) {
			keep = append(keep, l)
		} else {
			warn("label", l)
		}
	}
	is.Labels = keep

	cleanFields(&is, warn)
	out := line{n: n, issue: is}
	for _, d := range b.Dependencies {
		from := store.IssueID(d.IssueID)
		if from == "" {
			from = id
		}
		if d.Type == "parent-child" && from == id {
			out.parents = append(out.parents, store.IssueID(d.DependsOnID))
			continue
		}
		if d.ThreadID != "" {
			warn("thread-id", "")
		}
		dm, err := decodeMetadata(d.Metadata, true)
		if err != nil {
			warn("dep-metadata", "")
			dm = nil
		}
		by := cleanLine("dependency created_by", d.CreatedBy, warn)
		if by == "" {
			by = principal
		}
		at := d.CreatedAt
		if at.IsZero() {
			at = is.CreatedAt
		}
		out.deps = append(out.deps, depRef{bdType: d.Type, dep: store.Dep{
			From: from, To: store.IssueID(d.DependsOnID), Type: store.DepType(d.Type),
			CreatedBy: by, CreatedAt: at, Metadata: dm,
		}})
	}
	for _, c := range b.Comments {
		author := cleanLine("comment author", c.Author, warn)
		if author == "" {
			author = principal
		}
		at := c.CreatedAt
		if at.IsZero() {
			at = is.CreatedAt
		}
		out.comments = append(out.comments, store.Comment{
			ID: commentID(id, c.ID, c), Issue: id, Author: author, Session: cleanLine("comment session", c.Session, warn),
			Body: cleanText("comment text", c.Text, warn), CreatedAt: at,
		})
	}
	return out, nil
}

// validLabel is the store's label rule, so a bad label is reported as a
// warning rather than failing the whole issue.
func validLabel(l string) bool { return store.ValidLabel(l) }

// The store refuses control and bidirectional characters (safetext), which
// bd allows. Import cleans them out, with a warning naming the field, so
// one such issue does not fail; the cleaning is the same every run, so a
// reimport finds the issue unchanged.

// cleanFields cleans an issue's text fields, in bd's names.
func cleanFields(is *store.Issue, warn func(kind, detail string)) {
	for _, f := range []struct {
		name string
		v    *string
	}{{"title", &is.Title}, {"assignee", &is.Assignee}, {"owner", &is.Owner},
		{"created_by", &is.CreatedBy}, {"close_reason", &is.CloseReason}} {
		*f.v = cleanLine(f.name, *f.v, warn)
	}
	for _, f := range []struct {
		name string
		v    *string
	}{{"description", &is.Body}, {"design", &is.Design}, {"acceptance_criteria", &is.Acceptance}, {"notes", &is.Notes}} {
		*f.v = cleanText(f.name, *f.v, warn)
	}
}

// cleanLine returns [safetext.CleanLine] of v, and warns, naming field,
// when that changes it.
func cleanLine(field, v string, warn func(kind, detail string)) string {
	c := safetext.CleanLine(v)
	if c != v {
		warn("text", field)
	}
	return c
}

// cleanText is cleanLine for multi-line text, with [safetext.CleanText].
func cleanText(field, v string, warn func(kind, detail string)) string {
	c := safetext.CleanText(v)
	if c != v {
		warn("text", field)
	}
	return c
}
