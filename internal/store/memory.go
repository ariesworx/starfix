package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/safetext"
	"github.com/ariesworx/starfix/internal/secretscan"
)

// Memory (design §6). A memory is a keyed note that agents and people keep
// across sessions: a fact about the repository, a team convention, or one
// person's preference. Its scope says who sees it, and a key names one
// record per scope. A user-scope memory is its author's alone, on every
// machine and agent, and the store enforces that: every read and write is
// for the actor's own user scope, so another principal's user memories
// are not there to find. starfixd serves one project, so team and project
// memories reach the same principals and differ in intent: a project
// memory is a fact about the repository, a team memory how the people
// work.

// Scope says who a memory is for.
type Scope string

// Scopes.
const (
	// ScopeTeam is for everyone on the server: conventions, decisions.
	ScopeTeam Scope = "team"
	// ScopeProject is for the project's members: repository facts. It is
	// the default.
	ScopeProject Scope = "project"
	// ScopeUser is for the author alone: preferences, working notes.
	ScopeUser Scope = "user"
)

// Valid reports whether s is a known scope.
func (s Scope) Valid() bool { return s == ScopeTeam || s == ScopeProject || s == ScopeUser }

// orDefault returns s, or ScopeProject when it is empty.
func (s Scope) orDefault() Scope {
	if s == "" {
		return ScopeProject
	}
	return s
}

// checkScope refuses, with ErrInvalid, a scope that is set and unknown.
func checkScope(s Scope) error {
	if s != "" && !s.Valid() {
		return fmt.Errorf("%w: scope %q must be team, project or user", ErrInvalid, s)
	}
	return nil
}

// owner is the owner column for a memory in scope s written or read by
// principal: the principal for user scope, else empty.
func owner(s Scope, principal string) string {
	if s == ScopeUser {
		return principal
	}
	return ""
}

// Memory is one memory record as the store reads it back: times in UTC to
// the microsecond, and Tags sorted.
type Memory struct {
	ID    string   `json:"id"`
	Scope Scope    `json:"scope"`
	Key   string   `json:"key"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags,omitempty"`
	// Issue is the issue the memory is about, if any.
	Issue  IssueID `json:"issue,omitempty"`
	Pinned bool    `json:"pinned,omitempty"`
	// Author first remembered it; UpdatedBy wrote it last.
	Author    string    `json:"author"`
	UpdatedBy string    `json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is when its content last changed; pinning leaves it.
	UpdatedAt time.Time `json:"updated_at"`
	Rev       Rev       `json:"rev"`
	// Relevant is set by [Store.PrimeMemories] and [Store.IssueMemories]
	// on a memory linked to one of the issues they rank for, or tagged
	// with one of their labels.
	Relevant bool `json:"relevant,omitempty"`

	// forgotten marks a tombstone, which only loadMemory returns.
	forgotten bool
}

// NewMemory is the input to Remember.
type NewMemory struct {
	// Scope is empty for ScopeProject.
	Scope Scope
	Key   string
	Body  string
	// Tags sets the tags; nil leaves a stored memory's tags alone and
	// gives a new one none, and an empty slice clears them.
	Tags *[]string
	// Issue links the memory to an issue, which must exist; nil leaves a
	// stored memory's link alone and a new one unlinked, and "" unlinks.
	Issue *IssueID
	// Pinned sets the pin; nil leaves a stored memory's pin alone and
	// leaves a new one unpinned.
	Pinned *bool
	// Rev is 0 to create the key, refused when it exists; otherwise the
	// stored memory's revision, which the write replaces.
	Rev Rev
	// IdempotencyKey makes a retried remember return the first result.
	IdempotencyKey string
}

// MemoryQuery selects memories for Recall. Empty fields match everything.
type MemoryQuery struct {
	// Scope keeps one scope; empty keeps every scope the caller sees.
	Scope Scope
	// Key keeps the memories with exactly this key.
	Key string
	// Tag keeps the memories with this tag.
	Tag string
	// Text keeps the memories whose key or body holds it, in any case.
	Text string
	// Limit defaults to DefaultRecall and is capped at MaxPage.
	Limit int
}

// DefaultRecall is how many memories Recall returns when the query gives
// no limit.
const DefaultRecall = 20

// maxQueryText bounds a recall's text.
const maxQueryText = 500

// Memory event ops. Each targets [MemoryTarget] of the memory's id.
const (
	OpMemoryCreate Op = "memory.create"
	OpMemoryUpdate Op = "memory.update"
	OpMemoryForget Op = "memory.forget"
	OpMemoryPin    Op = "memory.pin"
	OpMemoryUnpin  Op = "memory.unpin"
	// OpMemoryImport records a memory written by an importer.
	OpMemoryImport Op = "memory.import"
)

// MemoryTargetPrefix starts the target of every memory event. The colon
// keeps it from ever being an issue id, so the live board, which pushes
// the events of issue-shaped targets, never takes it for one.
const MemoryTargetPrefix = "memory:"

// MemoryTarget is the event target for the memory id.
func MemoryTarget(id string) string { return MemoryTargetPrefix + id }

// memoryKeyPattern is what a key may hold. It starts with a letter or
// digit, so it cannot be read as an option.
var memoryKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]*$`)

// memoryTagPattern is what a tag may hold: what a label may, at any
// length the limits allow.
var memoryTagPattern = regexp.MustCompile(`^[^\s,\x00-\x1f]+$`)

// MemoryConflictError refuses a remember or forget that names a revision
// other than the stored one: Rev 0, to create a key that exists, or a
// stale one. Current is the stored revision and By who wrote it. It
// wraps ErrConflict.
type MemoryConflictError struct {
	Scope        Scope
	Key          string
	Rev, Current Rev
	By           string
}

func (e *MemoryConflictError) Error() string {
	if e.Rev == 0 {
		return fmt.Sprintf("memory %s exists in %s scope at rev %d, written by %s", e.Key, e.Scope, e.Current, e.By)
	}
	return fmt.Sprintf("memory %s in %s scope changed since rev %d (now rev %d by %s)", e.Key, e.Scope, e.Rev, e.Current, e.By)
}

// Unwrap makes errors.Is(err, ErrConflict) hold.
func (e *MemoryConflictError) Unwrap() error { return ErrConflict }

// SecretError refuses text that looks like a credential. Field names the
// field (key, body or tag) and Kind what it looks like; the text itself
// is never repeated. It wraps ErrInvalid.
type SecretError struct {
	Field string
	Kind  secretscan.Kind
}

func (e *SecretError) Error() string {
	return fmt.Sprintf("%v: the memory's %s looks like it holds a secret (%s)", ErrInvalid, e.Field, e.Kind)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *SecretError) Unwrap() error { return ErrInvalid }

// MemoryLimitError refuses a new memory past Limits.Memories, the most a
// principal may hold in one scope. It wraps ErrInvalid.
type MemoryLimitError struct {
	Principal string
	Scope     Scope
	Max       int
}

func (e *MemoryLimitError) Error() string {
	return fmt.Sprintf("%v: %s may hold at most %d memories in %s scope", ErrInvalid, e.Principal, e.Max, e.Scope)
}

// Unwrap makes errors.Is(err, ErrInvalid) hold.
func (e *MemoryLimitError) Unwrap() error { return ErrInvalid }

// checkMemoryKey refuses, with ErrInvalid, a key out of pattern or longer
// than most bytes.
func checkMemoryKey(key string, most int) error {
	if len(key) > most || !memoryKeyPattern.MatchString(key) {
		return fmt.Errorf("%w: memory key must be 1-%d bytes of letters, digits and ._:/@+-, starting with a letter or digit", ErrInvalid, most)
	}
	return nil
}

// normalize refuses, with ErrInvalid, a memory out of bounds or holding
// a secret, and gives it its default scope and sorted, distinct tags.
func (in *NewMemory) normalize(lim Limits) error {
	if err := checkScope(in.Scope); err != nil {
		return err
	}
	in.Scope = in.Scope.orDefault()
	if err := checkMemoryKey(in.Key, lim.MemoryKeyLength); err != nil {
		return err
	}
	if err := checkText("body", in.Body, lim.MemoryBody, true); err != nil {
		return err
	}
	if in.Tags != nil {
		tags := slices.Clone(*in.Tags)
		for _, tag := range tags {
			if len(tag) > lim.MemoryTagLength || !memoryTagPattern.MatchString(tag) || !safetext.ValidLine(tag) {
				return fmt.Errorf("%w: memory tag must be 1-%d bytes without spaces, commas, or control or bidirectional characters",
					ErrInvalid, lim.MemoryTagLength)
			}
		}
		slices.Sort(tags)
		tags = slices.Compact(tags)
		if len(tags) > lim.MemoryTags {
			return fmt.Errorf("%w: a memory has at most %d tags, not %d", ErrInvalid, lim.MemoryTags, len(tags))
		}
		in.Tags = &tags
	}
	if id := in.issue(); id != "" {
		if err := id.Validate(); err != nil {
			return err
		}
	}
	if in.Rev < 0 {
		return fmt.Errorf("%w: rev %d is negative", ErrInvalid, in.Rev)
	}
	if err := validIdem(in.IdempotencyKey); err != nil {
		return err
	}
	return in.secrets()
}

// tags is in's tags, or none when it leaves them alone.
func (in *NewMemory) tags() []string {
	if in.Tags == nil {
		return nil
	}
	return *in.Tags
}

// issue is in's issue link, or none when it leaves it alone.
func (in *NewMemory) issue() IssueID {
	if in.Issue == nil {
		return ""
	}
	return *in.Issue
}

// secrets refuses a memory whose key, body or tags look like they hold a
// credential (design §6), with a [*SecretError].
func (in *NewMemory) secrets() error {
	fields := []struct{ name, v string }{{"key", in.Key}, {"body", in.Body}}
	for _, tag := range in.tags() {
		fields = append(fields, struct{ name, v string }{"tag", tag})
	}
	for _, f := range fields {
		if found, ok := secretscan.Find(f.v); ok {
			return &SecretError{Field: f.name, Kind: found.Kind}
		}
	}
	return nil
}

// memoryCols are the columns scanMemory reads, in its order, from
// memories aliased m.
const memoryCols = `m.id, m.scope, m.mem_key, m.body, m.issue_id, m.pinned, m.author, m.updated_by,
  m.created_at, m.updated_at, m.rev, m.deleted`

// memoryVisible is the condition on memories aliased m that keeps what
// its one argument, the reader's principal, may see: no tombstone, and
// no other principal's user memory.
const memoryVisible = `(NOT m.deleted AND (m.scope <> 'user' OR m.owner = ?))`

// scanMemory scans memoryCols into a Memory; Tags are left for
// attachTags.
func scanMemory(sc scanner) (Memory, error) {
	var m Memory
	var issue sql.NullString
	if err := sc.Scan(&m.ID, &m.Scope, &m.Key, &m.Body, &issue, &m.Pinned, &m.Author, &m.UpdatedBy,
		&m.CreatedAt, &m.UpdatedAt, &m.Rev, &m.forgotten); err != nil {
		return Memory{}, err
	}
	m.Issue = IssueID(issue.String)
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

// queryMemories reads the memories the query, which selects memoryCols,
// returns, with their tags.
func queryMemories(ctx context.Context, q querier, query string, args ...any) ([]Memory, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("memories: %w", err)
	}
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("memories: %w", err)
		}
		out = append(out, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("memories: %w", err)
	}
	return out, attachTags(ctx, q, out)
}

// attachTags fills Tags for each memory, sorted.
func attachTags(ctx context.Context, q querier, ms []Memory) error {
	if len(ms) == 0 {
		return nil
	}
	at := make(map[string]int, len(ms))
	args := make([]any, len(ms))
	for i, m := range ms {
		at[m.ID] = i
		args[i] = m.ID
	}
	rows, err := q.QueryContext(ctx, `SELECT memory_id, tag FROM memory_tags WHERE memory_id IN (`+
		placeholders(len(args))+`) ORDER BY memory_id, tag`, args...)
	if err != nil {
		return fmt.Errorf("memory tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return fmt.Errorf("memory tags: %w", err)
		}
		if i, ok := at[id]; ok {
			ms[i].Tags = append(ms[i].Tags, tag)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memory tags: %w", err)
	}
	return nil
}

// loadMemory reads the memory with key in scope for principal, and
// reports whether there is one. It may be a tombstone (forgotten): its
// rev is where the key's next memory continues.
func loadMemory(ctx context.Context, q querier, scope Scope, principal, key string) (Memory, bool, error) {
	ms, err := queryMemories(ctx, q, `SELECT `+memoryCols+` FROM memories m WHERE m.scope = ? AND m.owner = ? AND m.mem_key = ?`,
		string(scope), owner(scope, principal), key)
	if err != nil || len(ms) == 0 {
		return Memory{}, false, err
	}
	return ms[0], true, nil
}

// MemoryNotFoundError says the memory with Key in Scope is not there, or
// is another principal's. It wraps ErrNotFound.
type MemoryNotFoundError struct {
	Scope Scope
	Key   string
}

func (e *MemoryNotFoundError) Error() string {
	return fmt.Sprintf("memory %s not found in %s scope", e.Key, e.Scope)
}

// Unwrap makes errors.Is(err, ErrNotFound) hold.
func (e *MemoryNotFoundError) Unwrap() error { return ErrNotFound }

// errMemoryNotFound says the memory with key in scope is not there.
func errMemoryNotFound(scope Scope, key string) error {
	return &MemoryNotFoundError{Scope: scope, Key: key}
}

// memoryState is a memory as its events record it. A user-scope memory's
// event keeps only its scope, pin and revision: its key, body, tags and
// issue are its author's alone, and the event log is read more widely
// than the author's own memories.
func memoryState(m Memory) map[string]any {
	st := map[string]any{"scope": m.Scope, "rev": m.Rev}
	if m.Pinned {
		st["pinned"] = true
	}
	if m.Scope == ScopeUser {
		return st
	}
	st["key"], st["body"] = m.Key, m.Body
	if len(m.Tags) > 0 {
		st["tags"] = m.Tags
	}
	if m.Issue != "" {
		st["issue"] = m.Issue
	}
	return st
}

// newMemoryID returns a random 16-character memory id, in the form of a
// comment id.
func newMemoryID() string { return newCommentID() }

// Remember creates or replaces the actor's memory with in's key in its
// scope, and returns it. Rev 0 creates the key; any other Rev replaces the
// stored memory at that revision. Replacing it with what it holds changes
// nothing. A new key never conflicts with another, in any scope.
//
// Remember refuses invalid input with ErrInvalid, text that looks like a
// credential with a [*SecretError], a new memory past Limits.Memories with
// a [*MemoryLimitError], a Rev 0 on a key that exists or a stale Rev with
// a [*MemoryConflictError], a Rev on a key that is not there, or a link to
// an issue that is not, with ErrNotFound. With an IdempotencyKey, a
// repeat returns the first result's id and rev, with in's scope and key,
// and writes nothing; a replace that
// changed nothing records no key, and a repeat of it runs again.
func (s *Store) Remember(ctx context.Context, actor Actor, in NewMemory) (Memory, error) {
	if err := in.normalize(s.opts.Limits); err != nil {
		return Memory{}, err
	}
	var out Memory
	err := s.write(ctx, actor, func(w *wtx) error {
		if done, err := replayMemory(ctx, w, in.IdempotencyKey, "remember", in, in.Scope, in.Key, &out); done || err != nil {
			return err
		}
		cur, found, err := loadMemory(ctx, w.tx, in.Scope, w.actor.Principal, in.Key)
		if err != nil {
			return err
		}
		live := found && !cur.forgotten
		switch {
		case in.Rev == 0 && live:
			return &MemoryConflictError{Scope: in.Scope, Key: in.Key, Current: cur.Rev, By: cur.UpdatedBy}
		case in.Rev != 0 && !live:
			return errMemoryNotFound(in.Scope, in.Key)
		case in.Rev != 0 && cur.Rev != in.Rev:
			return &MemoryConflictError{Scope: in.Scope, Key: in.Key, Rev: in.Rev, Current: cur.Rev, By: cur.UpdatedBy}
		}
		if id := in.issue(); id != "" {
			if err := mustExist(ctx, w.tx, id); err != nil {
				return err
			}
		}
		switch {
		case in.Rev == 0 && found:
			out, err = w.reviveMemory(ctx, cur, in, OpMemoryCreate, true)
		case in.Rev == 0:
			out, err = w.insertMemory(ctx, in, OpMemoryCreate, true)
		default:
			out, err = w.replaceMemory(ctx, cur, in)
			if err == nil && out.Rev == cur.Rev {
				// Nothing changed and no event carries the key, so a
				// retry runs again rather than replaying.
				w.idem = nil
				return nil
			}
		}
		if err != nil {
			return err
		}
		return w.settle(memoryResult{out.ID, out.Rev})
	})
	if err != nil {
		return Memory{}, err
	}
	return out, nil
}

// checkCap refuses, with a [*MemoryLimitError], a new memory of w's
// actor in scope past Limits.Memories. Tombstones do not count.
func (w *wtx) checkCap(ctx context.Context, scope Scope) error {
	p := w.actor.Principal
	var n int
	if err := w.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE author = ? AND scope = ? AND NOT deleted`,
		p, string(scope)).Scan(&n); err != nil {
		return fmt.Errorf("count memories: %w", err)
	}
	if n >= w.lim.Memories {
		return &MemoryLimitError{Principal: p, Scope: scope, Max: w.lim.Memories}
	}
	return nil
}

// insertMemory writes in as a new memory of w's actor and records op.
// With capped set, it first refuses a memory past Limits.Memories.
func (w *wtx) insertMemory(ctx context.Context, in NewMemory, op Op, capped bool) (Memory, error) {
	p := w.actor.Principal
	if capped {
		if err := w.checkCap(ctx, in.Scope); err != nil {
			return Memory{}, err
		}
	}
	m := Memory{ID: newMemoryID(), Scope: in.Scope, Key: in.Key, Body: in.Body, Tags: in.tags(), Issue: in.issue(),
		Pinned: in.Pinned != nil && *in.Pinned, Author: p, UpdatedBy: p, CreatedAt: w.now, UpdatedAt: w.now, Rev: 1}
	if _, err := w.exec(ctx, `INSERT INTO memories
  (id, scope, owner, mem_key, body, issue_id, pinned, author, updated_by, created_at, updated_at, rev, write_id)
  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, string(m.Scope), owner(m.Scope, p), m.Key, m.Body, nullStr(m.Issue), m.Pinned, p, p, w.now, w.now, m.Rev,
		randomInt63()); err != nil {
		if isDuplicate(err) {
			// A concurrent remember of the same key committed first; the
			// rerun finds it and reports the conflict.
			return Memory{}, fmt.Errorf("%w: memory %s: %w", errRetry, m.Key, err)
		}
		return Memory{}, fmt.Errorf("insert memory: %w", err)
	}
	if err := w.insertTags(ctx, m.ID, m.Tags); err != nil {
		return Memory{}, err
	}
	return m, w.event(ctx, op, MemoryTarget(m.ID), nil, memoryState(m))
}

// reviveMemory writes in as a new memory of w's actor over the tombstone
// cur, keeping its id and continuing its rev, and records op. With
// capped set, it first refuses a memory past Limits.Memories.
func (w *wtx) reviveMemory(ctx context.Context, cur Memory, in NewMemory, op Op, capped bool) (Memory, error) {
	p := w.actor.Principal
	if capped {
		if err := w.checkCap(ctx, in.Scope); err != nil {
			return Memory{}, err
		}
	}
	m := Memory{ID: cur.ID, Scope: in.Scope, Key: in.Key, Body: in.Body, Tags: in.tags(), Issue: in.issue(),
		Pinned: in.Pinned != nil && *in.Pinned, Author: p, UpdatedBy: p, CreatedAt: w.now, UpdatedAt: w.now, Rev: cur.Rev + 1}
	if err := w.updateMemory(ctx, cur, `UPDATE memories SET deleted = FALSE, body = ?, issue_id = ?, pinned = ?, author = ?,
  updated_by = ?, created_at = ?, updated_at = ?, rev = rev + 1, write_id = ? WHERE id = ? AND rev = ?`,
		m.Body, nullStr(m.Issue), m.Pinned, p, p, w.now, w.now); err != nil {
		return Memory{}, err
	}
	if err := w.insertTags(ctx, m.ID, m.Tags); err != nil {
		return Memory{}, err
	}
	return m, w.event(ctx, op, MemoryTarget(m.ID), nil, memoryState(m))
}

// insertTags adds tags to the memory id.
func (w *wtx) insertTags(ctx context.Context, id string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	args := make([]any, 0, 2*len(tags))
	for _, tag := range tags {
		args = append(args, id, tag)
	}
	if _, err := w.exec(ctx, `INSERT INTO memory_tags (memory_id, tag) VALUES `+
		strings.TrimSuffix(strings.Repeat("(?, ?),", len(tags)), ","), args...); err != nil {
		return fmt.Errorf("insert memory tags: %w", err)
	}
	return nil
}

// replaceMemory writes in over cur, which is at in's Rev, and records an
// update; when nothing would change it returns cur and writes nothing.
// updated_at moves only when the body, tags or issue change.
func (w *wtx) replaceMemory(ctx context.Context, cur Memory, in NewMemory) (Memory, error) {
	m := cur
	m.Body = in.Body
	if in.Tags != nil {
		m.Tags = *in.Tags
	}
	if in.Issue != nil {
		m.Issue = *in.Issue
	}
	if in.Pinned != nil {
		m.Pinned = *in.Pinned
	}
	tagsChanged := !slices.Equal(cur.Tags, m.Tags)
	contentChanged := m.Body != cur.Body || m.Issue != cur.Issue || tagsChanged
	if !contentChanged && m.Pinned == cur.Pinned {
		return cur, nil
	}
	m.UpdatedBy, m.Rev = w.actor.Principal, cur.Rev+1
	if contentChanged {
		// A pin alone leaves updated_at, as PinMemory does.
		m.UpdatedAt = w.now
	}
	if err := w.updateMemory(ctx, cur, `UPDATE memories SET body = ?, issue_id = ?, pinned = ?, updated_by = ?, updated_at = ?,
  rev = rev + 1, write_id = ? WHERE id = ? AND rev = ?`, m.Body, nullStr(m.Issue), m.Pinned, m.UpdatedBy, m.UpdatedAt); err != nil {
		return Memory{}, err
	}
	if tagsChanged {
		if _, err := w.exec(ctx, `DELETE FROM memory_tags WHERE memory_id = ?`, m.ID); err != nil {
			return Memory{}, fmt.Errorf("replace memory tags: %w", err)
		}
		if err := w.insertTags(ctx, m.ID, m.Tags); err != nil {
			return Memory{}, err
		}
	}
	return m, w.event(ctx, OpMemoryUpdate, MemoryTarget(m.ID), memoryState(cur), memoryState(m))
}

// updateMemory runs query, an UPDATE of memories ending in "rev = rev +
// 1, write_id = ? WHERE id = ? AND rev = ?", on cur with args and then a
// fresh write_id: a compare-and-swap on cur.Rev that Dolt enforces.
func (w *wtx) updateMemory(ctx context.Context, cur Memory, query string, args ...any) error {
	n, err := w.exec(ctx, query, append(args, randomInt63(), cur.ID, cur.Rev)...)
	if err != nil {
		return fmt.Errorf("update memory: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: memory %s moved", errRetry, cur.Key)
	}
	return nil
}

// forgetArgs identify a forget for its idempotency hash.
type forgetArgs struct {
	Scope Scope  `json:"scope"`
	Key   string `json:"key"`
	Rev   Rev    `json:"rev"`
}

// Forget deletes the actor's memory with key in scope (empty is
// ScopeProject) and returns it as it was, at the revision the forget
// gave it. Rev 0 deletes whatever is stored; any other must be the
// stored revision, or the forget is refused with a [*MemoryConflictError].
// A key that is not there is ErrNotFound. The row stays as a tombstone,
// its body, issue and tags cleared, so the key's revisions keep rising
// when it is remembered again. With an idempotency key idem, a repeat
// returns the first result's id and rev, with scope and key.
func (s *Store) Forget(ctx context.Context, actor Actor, scope Scope, key string, rev Rev, idem string) (Memory, error) {
	if err := checkScope(scope); err != nil {
		return Memory{}, err
	}
	scope = scope.orDefault()
	if err := checkMemoryKey(key, maxMemoryName); err != nil {
		return Memory{}, err
	}
	if rev < 0 {
		return Memory{}, fmt.Errorf("%w: rev %d is negative", ErrInvalid, rev)
	}
	if err := validIdem(idem); err != nil {
		return Memory{}, err
	}
	var out Memory
	err := s.write(ctx, actor, func(w *wtx) error {
		if done, err := replayMemory(ctx, w, idem, "forget", forgetArgs{scope, key, rev}, scope, key, &out); done || err != nil {
			return err
		}
		cur, found, err := loadMemory(ctx, w.tx, scope, w.actor.Principal, key)
		switch {
		case err != nil:
			return err
		case !found || cur.forgotten:
			return errMemoryNotFound(scope, key)
		case rev != 0 && cur.Rev != rev:
			return &MemoryConflictError{Scope: scope, Key: key, Rev: rev, Current: cur.Rev, By: cur.UpdatedBy}
		}
		if _, err := w.exec(ctx, `DELETE FROM memory_tags WHERE memory_id = ?`, cur.ID); err != nil {
			return fmt.Errorf("forget memory: %w", err)
		}
		out = cur
		out.UpdatedBy, out.UpdatedAt, out.Rev = w.actor.Principal, w.now, cur.Rev+1
		if err := w.updateMemory(ctx, cur, `UPDATE memories SET deleted = TRUE, body = '', issue_id = NULL, pinned = FALSE,
  updated_by = ?, updated_at = ?, rev = rev + 1, write_id = ? WHERE id = ? AND rev = ?`, out.UpdatedBy, out.UpdatedAt); err != nil {
			return err
		}
		if err := w.event(ctx, OpMemoryForget, MemoryTarget(cur.ID), memoryState(cur), nil); err != nil {
			return err
		}
		return w.settle(memoryResult{out.ID, out.Rev})
	})
	if err != nil {
		return Memory{}, err
	}
	return out, nil
}

// memoryResult is what a keyed remember or forget records for a repeat:
// the id and rev only. The whole memory would put a user memory's key,
// body and tags in the event log, which its events leave out
// (memoryState), and keep them there after it is forgotten.
type memoryResult struct {
	ID  string `json:"id"`
	Rev Rev    `json:"rev"`
}

// replayMemory is replay for remember and forget. A repeat sets *out to
// the first result's id and rev, with the scope and key of the request,
// and leaves every other field zero.
func replayMemory(ctx context.Context, w *wtx, key, op string, args any, scope Scope, memKey string, out *Memory) (bool, error) {
	var r memoryResult
	done, err := replay(ctx, w, key, op, args, &r)
	if done {
		*out = Memory{ID: r.ID, Scope: scope, Key: memKey, Rev: r.Rev}
	}
	return done, err
}

// PinMemory pins or unpins the actor's memory with key in scope (empty is
// ScopeProject) and returns it. A pinned memory comes first in recall and
// in every prime. Pinning raises its revision but not its updated time;
// pinning a pinned memory changes nothing. A key that is not there is
// ErrNotFound.
func (s *Store) PinMemory(ctx context.Context, actor Actor, scope Scope, key string, pinned bool) (Memory, error) {
	if err := checkScope(scope); err != nil {
		return Memory{}, err
	}
	scope = scope.orDefault()
	if err := checkMemoryKey(key, maxMemoryName); err != nil {
		return Memory{}, err
	}
	var out Memory
	err := s.write(ctx, actor, func(w *wtx) error {
		cur, found, err := loadMemory(ctx, w.tx, scope, w.actor.Principal, key)
		switch {
		case err != nil:
			return err
		case !found || cur.forgotten:
			return errMemoryNotFound(scope, key)
		case cur.Pinned == pinned:
			out = cur
			return nil
		}
		out = cur
		out.Pinned, out.UpdatedBy, out.Rev = pinned, w.actor.Principal, cur.Rev+1
		if err := w.updateMemory(ctx, cur, `UPDATE memories SET pinned = ?, updated_by = ?, rev = rev + 1, write_id = ? WHERE id = ? AND rev = ?`,
			pinned, out.UpdatedBy); err != nil {
			return err
		}
		op := OpMemoryPin
		if !pinned {
			op = OpMemoryUnpin
		}
		return w.event(ctx, op, MemoryTarget(cur.ID), memoryState(cur), memoryState(out))
	})
	if err != nil {
		return Memory{}, err
	}
	return out, nil
}

// Recall returns the memories the actor can see that match q, pinned
// first and then newest first, and how many more matched past q's limit.
// It refuses an unknown scope or an over-long query with ErrInvalid.
func (s *Store) Recall(ctx context.Context, actor Actor, q MemoryQuery) ([]Memory, int, error) {
	if err := checkScope(q.Scope); err != nil {
		return nil, 0, err
	}
	for _, f := range []struct{ name, v string }{{"key", q.Key}, {"tag", q.Tag}, {"text", q.Text}} {
		if err := checkLine(f.name, f.v, maxQueryText, false); err != nil {
			return nil, 0, err
		}
	}
	where, args := `WHERE `+memoryVisible, []any{actor.Principal}
	if q.Scope != "" {
		where += ` AND m.scope = ?`
		args = append(args, string(q.Scope))
	}
	if q.Key != "" {
		where += ` AND m.mem_key = ?`
		args = append(args, q.Key)
	}
	if q.Tag != "" {
		where += ` AND m.id IN (SELECT memory_id FROM memory_tags WHERE tag = ?)`
		args = append(args, q.Tag)
	}
	if q.Text != "" {
		like := "%" + likeEscaper.Replace(strings.ToLower(q.Text)) + "%"
		where += ` AND (LOWER(m.mem_key) LIKE ? OR LOWER(m.body) LIKE ?)`
		args = append(args, like, like)
	}
	limit := clampLimit(q.Limit, DefaultRecall, MaxPage)
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("recall: %w", err)
	}
	defer end()
	var total int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories m `+where, args...).Scan(&total); err != nil { //nolint:gosec // constant clauses; values are placeholders
		return nil, 0, fmt.Errorf("recall: %w", err)
	}
	ms, err := queryMemories(ctx, tx, `SELECT `+memoryCols+` FROM memories m `+where+ //nolint:gosec // constant clauses; values are placeholders
		` ORDER BY m.pinned DESC, m.updated_at DESC, m.id LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, 0, err
	}
	return ms, max(total-len(ms), 0), nil
}

// likeEscaper escapes LIKE's wildcards and its escape character, so a
// recall's text matches literally.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// primeIssues bounds the in-progress issues PrimeMemories ranks for.
const primeIssues = 50

// PrimeMemories returns up to limit memories the actor can see, ranked
// for a new session (design §6): pinned first, then those relevant to
// the actor's in-progress issues (linked to one, or tagged with one of
// their labels), then the rest; newest first within each, and never by
// key. limit 0 takes DefaultRecall; it is capped at MaxPage.
func (s *Store) PrimeMemories(ctx context.Context, actor Actor, limit int) ([]Memory, error) {
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("prime memories: %w", err)
	}
	defer end()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM issues WHERE status = ? AND assignee = ? ORDER BY updated_at DESC, id LIMIT ?`,
		string(StatusInProgress), actor.Principal, primeIssues)
	if err != nil {
		return nil, fmt.Errorf("prime memories: %w", err)
	}
	var ids []IssueID
	for rows.Next() {
		var id IssueID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("prime memories: %w", err)
		}
		ids = append(ids, id)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("prime memories: %w", err)
	}
	return rankMemories(ctx, tx, actor.Principal, ids, true, limit)
}

// IssueMemories returns up to limit memories the actor can see that are
// relevant to the issue id, linked to it or tagged with one of its
// labels: pinned first, then newest first. start returns them.
func (s *Store) IssueMemories(ctx context.Context, actor Actor, id IssueID, limit int) ([]Memory, error) {
	if err := id.Validate(); err != nil {
		return nil, err
	}
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return nil, fmt.Errorf("issue memories: %w", err)
	}
	defer end()
	return rankMemories(ctx, tx, actor.Principal, []IssueID{id}, false, limit)
}

// rankMemories returns up to limit memories principal can see: those
// relevant to issues (linked to one, or tagged with one of their
// labels), and with all set also every pinned memory and then the rest.
// They are ordered pinned first, then relevant, then newest, then by id.
// Each query reads at most what the final list can hold of its kind, so
// the cost does not grow with the number of memories.
func rankMemories(ctx context.Context, q querier, principal string, issues []IssueID, all bool, limit int) ([]Memory, error) {
	limit = clampLimit(limit, DefaultRecall, MaxPage)
	ids := make([]any, len(issues))
	for i, id := range issues {
		ids[i] = string(id)
	}
	labels := map[string]bool{}
	if len(ids) > 0 {
		ls, err := issueLabels(ctx, q, ids)
		if err != nil {
			return nil, err
		}
		for _, l := range ls {
			labels[l] = true
		}
	}
	relevant := func(m Memory) bool {
		return (m.Issue != "" && slices.Contains(issues, m.Issue)) || slices.ContainsFunc(m.Tags, func(t string) bool { return labels[t] })
	}
	const sel = `SELECT ` + memoryCols + ` FROM memories m WHERE ` + memoryVisible
	var chosen []Memory
	seen := map[string]bool{}
	add := func(ms []Memory) {
		for _, m := range ms {
			if !seen[m.ID] {
				seen[m.ID] = true
				m.Relevant = relevant(m)
				chosen = append(chosen, m)
			}
		}
	}
	if len(ids) > 0 {
		cond, args := `m.issue_id IN (`+placeholders(len(ids))+`)`, append([]any{principal}, ids...)
		if len(labels) > 0 {
			cond += ` OR m.id IN (SELECT memory_id FROM memory_tags WHERE tag IN (` + placeholders(len(labels)) + `))`
			for l := range labels {
				args = append(args, l)
			}
		}
		ms, err := queryMemories(ctx, q, sel+` AND (`+cond+`) ORDER BY m.pinned DESC, m.updated_at DESC, m.id LIMIT ?`, append(args, limit)...) //nolint:gosec // placeholders only; values are arguments
		if err != nil {
			return nil, err
		}
		add(ms)
	}
	if all {
		ms, err := queryMemories(ctx, q, sel+` AND m.pinned ORDER BY m.updated_at DESC, m.id LIMIT ?`, principal, limit)
		if err != nil {
			return nil, err
		}
		add(ms)
	}
	slices.SortStableFunc(chosen, func(a, b Memory) int {
		switch {
		case a.Pinned != b.Pinned:
			return boolOrder(a.Pinned)
		case a.Relevant != b.Relevant:
			return boolOrder(a.Relevant)
		case !a.UpdatedAt.Equal(b.UpdatedAt):
			return b.UpdatedAt.Compare(a.UpdatedAt)
		}
		return strings.Compare(a.ID, b.ID)
	})
	if all && len(chosen) < limit {
		// The rest, newest first: past the ones already chosen, which
		// may come among them, there are enough to fill the list.
		ms, err := queryMemories(ctx, q, sel+` ORDER BY m.updated_at DESC, m.id LIMIT ?`, principal, limit+len(chosen))
		if err != nil {
			return nil, err
		}
		add(ms)
	}
	return chosen[:min(len(chosen), limit)], nil
}

// boolOrder sorts true before false: -1 for true, 1 for false.
func boolOrder(first bool) int {
	if first {
		return -1
	}
	return 1
}

// issueLabels returns the distinct labels of the issues ids.
func issueLabels(ctx context.Context, q querier, ids []any) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT label FROM labels WHERE issue_id IN (`+placeholders(len(ids))+`)`, ids...) //nolint:gosec // placeholders only; values are arguments
	if err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("labels: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("labels: %w", err)
	}
	return out, nil
}

// normalizeImport checks a memory to import: as Remember would, in team
// or project scope (an importer has no user scope to write for anyone).
func (in *NewMemory) normalizeImport(lim Limits) error {
	if err := in.normalize(lim); err != nil {
		return err
	}
	if in.Scope == ScopeUser {
		return fmt.Errorf("%w: an import writes team or project memories, not user ones", ErrInvalid)
	}
	return nil
}

// planMemory is what importing in does given what is stored: a key not
// there, or forgotten, is created, one holding the same body, tags and
// issue is unchanged, and any other is stale and kept as it is, since an
// import carries no time to tell which side is newer.
func planMemory(cur Memory, found bool, in NewMemory) ImportOutcome {
	switch {
	case !found || cur.forgotten:
		return ImportCreated
	case cur.Body == in.Body && slices.Equal(cur.Tags, in.tags()) && cur.Issue == in.issue():
		return ImportUnchanged
	}
	return ImportStale
}

// PlanImportMemory reports what ImportMemory would do with in, and
// writes nothing.
func (s *Store) PlanImportMemory(ctx context.Context, in NewMemory) (ImportOutcome, error) {
	if err := in.normalizeImport(s.opts.Limits); err != nil {
		return "", err
	}
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return "", fmt.Errorf("plan memory import: %w", err)
	}
	defer end()
	cur, found, err := loadMemory(ctx, tx, in.Scope, "", in.Key)
	if err != nil {
		return "", err
	}
	return planMemory(cur, found, in), nil
}

// ImportMemory writes a memory from another tracker (bd's kv.memory.*),
// authored by actor, when its key is new in its scope, recording
// memory.import; a key already there is left as it is (planMemory). It is
// the operator's command, so the per-scope cap does not apply; the other
// checks Remember makes, the secrets lint among them, do.
func (s *Store) ImportMemory(ctx context.Context, actor Actor, in NewMemory) (ImportOutcome, error) {
	if err := in.normalizeImport(s.opts.Limits); err != nil {
		return "", err
	}
	in.Pinned, in.Rev = nil, 0
	var out ImportOutcome
	err := s.write(ctx, actor, func(w *wtx) error {
		cur, found, err := loadMemory(ctx, w.tx, in.Scope, "", in.Key)
		if err != nil {
			return err
		}
		if out = planMemory(cur, found, in); out != ImportCreated {
			return nil
		}
		if id := in.issue(); id != "" {
			if err := mustExist(ctx, w.tx, id); err != nil {
				return err
			}
		}
		if found {
			_, err = w.reviveMemory(ctx, cur, in, OpMemoryImport, false)
		} else {
			_, err = w.insertMemory(ctx, in, OpMemoryImport, false)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	return out, nil
}
