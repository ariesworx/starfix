package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Files to issues (design §12 item 3). An issue's paths are the files its
// work touched, which clients find in git and send on renew, finish and
// handoff (source commit), and the files a person or agent declares at
// create and update (source declared; a path ending in "/" is a directory
// prefix covering everything under it). Ready ranks down an issue whose
// paths overlap those of an issue another session holds, and IssueFiles
// lists an issue's likely files for show.
//
// Commit paths are recorded only on an issue the actor may change
// (wtx.guard): a finish or handoff checks it as before, and a renew
// records them only for the claims it renews, which are the actor's own.
// A new path is history, recorded as one issue.paths event per call; a
// call that only sees paths already recorded refreshes their last_at,
// which orders show's list, and is bookkeeping, so it writes quietly.

// OpIssuePaths records paths added to an issue, or its declared paths
// changed. The after state names the source, up to maxEventPaths of the
// paths added and removed, their counts when there were more, and how
// many old commit paths the cap dropped.
const OpIssuePaths Op = "issue.paths"

// PathSource says where one of an issue's paths came from. A later source,
// such as predictions from similar issues, is a new value in the same
// table.
type PathSource string

// Path sources.
const (
	PathCommit   PathSource = proto.PathCommit
	PathDeclared PathSource = proto.PathDeclared
)

// IssuePath is one of an issue's paths: where it came from, who last
// recorded it, when it was first and last recorded.
type IssuePath struct {
	Path    string
	Source  PathSource
	By      Actor
	FirstAt time.Time
	LastAt  time.Time
}

// Overlap is an issue another session holds whose paths overlap, and its
// holder.
type Overlap struct {
	Issue  IssueID
	Holder Actor
}

// Files are an issue's likely files, as IssueFiles lists them.
type Files struct {
	// Paths are declared paths, in path order, then commit paths, most
	// recently recorded first.
	Paths []IssuePath
	// More counts the paths left out.
	More int
	// Overlaps are the issues other sessions hold whose paths overlap
	// the issue's, in id order.
	Overlaps []Overlap
}

// ReadyIssue is an issue Ready lists, and the issues other sessions hold
// whose paths overlap its own.
type ReadyIssue struct {
	Issue
	Overlaps []IssueID
}

// maxEventPaths bounds the paths one issue.paths event lists, so the log
// grows by a bounded amount per call however many paths it carries.
const maxEventPaths = 20

// pathBatch bounds the placeholders in one statement over paths or ids.
const pathBatch = 500

// checkPaths refuses, with ErrInvalid, a path proto.CheckPath refuses or,
// unless prefixes, a directory prefix; it returns ps without repeats, in
// their first order.
func checkPaths(ps []string, prefixes bool) ([]string, error) {
	seen := make(map[string]bool, len(ps))
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if err := proto.CheckPath(p); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		if !prefixes && proto.IsPathPrefix(p) {
			return nil, fmt.Errorf("%w: path %q ends in /; only declared paths may be directory prefixes", ErrInvalid, p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out, nil
}

// checkDeclared is checkPaths for declared paths, refusing more than
// limit of them.
func checkDeclared(ps []string, limit int) ([]string, error) {
	ps, err := checkPaths(ps, true)
	if err != nil {
		return nil, err
	}
	if len(ps) > limit {
		return nil, fmt.Errorf("%w: an issue has at most %d paths, not %d; declare a directory prefix such as internal/store/ instead", ErrInvalid, limit, len(ps))
	}
	return ps, nil
}

// pathRow is the part of an issue_paths row a write needs.
type pathRow struct {
	path   string
	source PathSource
	lastAt time.Time
}

// loadPathRows reads every path of id.
func loadPathRows(ctx context.Context, q querier, id IssueID) ([]pathRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT path, source, last_at FROM issue_paths WHERE issue_id = ?`, string(id))
	if err != nil {
		return nil, fmt.Errorf("paths of %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []pathRow
	for rows.Next() {
		var r pathRow
		if err := rows.Scan(&r.path, &r.source, &r.lastAt); err != nil {
			return nil, fmt.Errorf("paths of %s: %w", id, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("paths of %s: %w", id, err)
	}
	return out, nil
}

// recordCommitPaths records ps, checked and most recent first, as id's
// commit paths: new paths are inserted and one issue.paths event lists
// them; paths already recorded have their last_at refreshed, quietly when
// nothing else changed. Past the paths_per_issue limit, less the issue's
// declared paths, the request keeps its first paths and the issue its
// most recent. The caller has checked the actor may change id.
func recordCommitPaths(ctx context.Context, w *wtx, id IssueID, ps []string) error {
	if len(ps) == 0 {
		return nil
	}
	have, err := loadPathRows(ctx, w.tx, id)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	declared := 0
	for _, r := range have {
		if r.source == PathCommit {
			known[r.path] = true
		} else {
			declared++
		}
	}
	keep := w.lim.Paths - declared
	if keep <= 0 {
		return nil
	}
	ps = ps[:min(len(ps), keep)]
	var added, refreshed []string
	for _, p := range ps {
		if known[p] {
			refreshed = append(refreshed, p)
		} else {
			added = append(added, p)
		}
	}
	for chunk := range slices.Chunk(refreshed, pathBatch) {
		args := []any{w.now, w.actor.Principal, w.actor.Session, randomInt63(), string(id), string(PathCommit)}
		for _, p := range chunk {
			args = append(args, p)
		}
		if _, err := w.exec(ctx, `UPDATE issue_paths SET last_at = ?, principal = ?, session = ?, rev = rev + 1, write_id = ?
  WHERE issue_id = ? AND source = ? AND path IN (`+placeholders(len(chunk))+`)`, args...); err != nil {
			return fmt.Errorf("refresh paths of %s: %w", id, err)
		}
	}
	if err := insertPaths(ctx, w, id, PathCommit, added); err != nil {
		return err
	}
	if len(added) == 0 {
		w.quiet = true // only refreshed: bookkeeping, not history
		return nil
	}
	trimmed, err := trimCommitPaths(ctx, w, id, keep)
	if err != nil {
		return err
	}
	return w.event(ctx, OpIssuePaths, string(id), nil, pathsEvent(PathCommit, added, nil, trimmed))
}

// setDeclaredPaths replaces id's declared paths with ps, checked: it
// deletes those not in ps, inserts the new ones, and records one
// issue.paths event when anything changed. Commit paths past the
// paths_per_issue limit, less the declared ones, are dropped, oldest
// first. The caller has checked the actor may change id.
func setDeclaredPaths(ctx context.Context, w *wtx, id IssueID, ps []string) error {
	have, err := loadPathRows(ctx, w.tx, id)
	if err != nil {
		return err
	}
	want := map[string]bool{}
	for _, p := range ps {
		want[p] = true
	}
	old := map[string]bool{}
	var removed []string
	for _, r := range have {
		if r.source != PathDeclared {
			continue
		}
		old[r.path] = true
		if !want[r.path] {
			removed = append(removed, r.path)
		}
	}
	var added []string
	for _, p := range ps {
		if !old[p] {
			added = append(added, p)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	for chunk := range slices.Chunk(removed, pathBatch) {
		args := []any{string(id), string(PathDeclared)}
		for _, p := range chunk {
			args = append(args, p)
		}
		if _, err := w.exec(ctx, `DELETE FROM issue_paths WHERE issue_id = ? AND source = ? AND path IN (`+
			placeholders(len(chunk))+`)`, args...); err != nil {
			return fmt.Errorf("remove declared paths of %s: %w", id, err)
		}
	}
	if err := insertPaths(ctx, w, id, PathDeclared, added); err != nil {
		return err
	}
	trimmed, err := trimCommitPaths(ctx, w, id, w.lim.Paths-len(ps))
	if err != nil {
		return err
	}
	slices.Sort(removed)
	return w.event(ctx, OpIssuePaths, string(id), nil, pathsEvent(PathDeclared, added, removed, trimmed))
}

// insertPaths inserts new rows for id's paths ps from source.
func insertPaths(ctx context.Context, w *wtx, id IssueID, source PathSource, ps []string) error {
	for chunk := range slices.Chunk(ps, pathBatch/10) {
		var args []any
		for _, p := range chunk {
			args = append(args, string(id), p, string(source), w.actor.Principal, w.actor.Session, w.now, w.now, randomInt63())
		}
		vals := slices.Repeat([]string{"(?, ?, ?, ?, ?, ?, ?, 1, ?)"}, len(chunk))
		if _, err := w.exec(ctx, `INSERT INTO issue_paths
  (issue_id, path, source, principal, session, first_at, last_at, rev, write_id) VALUES `+strings.Join(vals, ", "), args...); err != nil {
			return fmt.Errorf("insert paths of %s: %w", id, err)
		}
	}
	return nil
}

// trimCommitPaths deletes id's commit paths past the keep most recently
// recorded, and returns how many it deleted.
func trimCommitPaths(ctx context.Context, w *wtx, id IssueID, keep int) (int, error) {
	have, err := loadPathRows(ctx, w.tx, id)
	if err != nil {
		return 0, err
	}
	var commits []pathRow
	for _, r := range have {
		if r.source == PathCommit {
			commits = append(commits, r)
		}
	}
	keep = max(keep, 0)
	if len(commits) <= keep {
		return 0, nil
	}
	slices.SortFunc(commits, func(a, b pathRow) int {
		return cmp.Or(b.lastAt.Compare(a.lastAt), cmp.Compare(a.path, b.path))
	})
	drop := commits[keep:]
	for chunk := range slices.Chunk(drop, pathBatch) {
		args := []any{string(id), string(PathCommit)}
		for _, r := range chunk {
			args = append(args, r.path)
		}
		if _, err := w.exec(ctx, `DELETE FROM issue_paths WHERE issue_id = ? AND source = ? AND path IN (`+
			placeholders(len(chunk))+`)`, args...); err != nil {
			return 0, fmt.Errorf("trim paths of %s: %w", id, err)
		}
	}
	return len(drop), nil
}

// pathsEvent is an issue.paths event's after state.
func pathsEvent(source PathSource, added, removed []string, trimmed int) map[string]any {
	ev := map[string]any{"source": string(source)}
	for _, l := range []struct {
		key string
		ps  []string
	}{{"added", added}, {"removed", removed}} {
		if len(l.ps) == 0 {
			continue
		}
		ev[l.key] = l.ps[:min(len(l.ps), maxEventPaths)]
		if len(l.ps) > maxEventPaths {
			ev[l.key+"_count"] = len(l.ps)
		}
	}
	if trimmed > 0 {
		ev["trimmed"] = trimmed
	}
	return ev
}

// heldPaths indexes the paths of the issues held under live claims by any
// session but one, so a candidate's paths can be matched against them
// without a query per issue.
type heldPaths struct {
	// exact maps each held path, file or prefix, to the issues holding it.
	exact map[string][]IssueID
	// under maps each proper ancestor directory of a held path, such as
	// "a/" and "a/b/" for "a/b/c.go", to the issues holding a path under
	// it.
	under map[string][]IssueID
	// holder is each held issue's holder.
	holder map[IssueID]Actor
}

// loadHeld reads the paths of the issues held at now by sessions other
// than except's. It returns nil when none of them has a path: the common
// case, which leaves Ready's order alone. The work is bounded by the live
// claims times paths_per_issue, read by the claims_expires index and the
// issue_paths key.
func loadHeld(ctx context.Context, q querier, except Actor, now time.Time) (*heldPaths, error) {
	rows, err := q.QueryContext(ctx, `SELECT issue_id, principal, session, machine FROM claims
  WHERE principal IS NOT NULL AND expires_at > ?`, now)
	if err != nil {
		return nil, fmt.Errorf("held paths: %w", err)
	}
	holder := map[IssueID]Actor{}
	for rows.Next() {
		var id IssueID
		var a Actor
		if err := rows.Scan(&id, &a.Principal, &a.Session, &a.Machine); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("held paths: %w", err)
		}
		if a.Principal != except.Principal || a.Session != except.Session {
			holder[id] = a
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("held paths: %w", err)
	}
	if len(holder) == 0 {
		return nil, nil
	}
	h := &heldPaths{exact: map[string][]IssueID{}, under: map[string][]IssueID{}, holder: holder}
	byIssue, err := pathsOf(ctx, q, slices.Sorted(maps.Keys(holder)))
	if err != nil {
		return nil, err
	}
	if len(byIssue) == 0 {
		return nil, nil
	}
	for id, ps := range byIssue {
		for _, p := range ps {
			h.exact[p] = append(h.exact[p], id)
			for a := range ancestors(p) {
				h.under[a] = append(h.under[a], id)
			}
		}
	}
	return h, nil
}

// pathsOf reads the paths of the issues ids, by source, then path.
func pathsOf(ctx context.Context, q querier, ids []IssueID) (map[IssueID][]string, error) {
	out := map[IssueID][]string{}
	for chunk := range slices.Chunk(ids, pathBatch) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = string(id)
		}
		rows, err := q.QueryContext(ctx, `SELECT issue_id, path FROM issue_paths WHERE issue_id IN (`+
			placeholders(len(chunk))+`) ORDER BY issue_id, source, path`, args...)
		if err != nil {
			return nil, fmt.Errorf("issue paths: %w", err)
		}
		for rows.Next() {
			var id IssueID
			var p string
			if err := rows.Scan(&id, &p); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("issue paths: %w", err)
			}
			out[id] = append(out[id], p)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("issue paths: %w", err)
		}
	}
	return out, nil
}

// ancestors yields p's proper ancestor directories, shortest first: "a/"
// and "a/b/" for "a/b/c.go" and for "a/b/c/".
func ancestors(p string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for i := range len(p) - 1 {
			if p[i] == '/' && !yield(p[:i+1]) {
				return
			}
		}
	}
}

// match returns the held issues, other than self, whose paths overlap ps,
// in id order. Two paths overlap when they are equal or one is a
// directory prefix covering the other.
func (h *heldPaths) match(ps []string, self IssueID) []IssueID {
	if h == nil {
		return nil
	}
	hit := map[IssueID]bool{}
	add := func(ids []IssueID) {
		for _, id := range ids {
			hit[id] = true
		}
	}
	for _, p := range ps {
		add(h.exact[p]) // equal, file or prefix
		for a := range ancestors(p) {
			add(h.exact[a]) // a held prefix covers p
		}
		if proto.IsPathPrefix(p) {
			add(h.under[p]) // p covers a held path
		}
	}
	delete(hit, self)
	return slices.Sorted(maps.Keys(hit))
}

// readyBatch is how many ready issues rankReady checks per query of their
// paths.
const readyBatch = 200

// rankReady returns the ids Ready lists for actor at now, best first, at
// most limit, and the overlaps of those that have any. While no other
// session holds an issue with paths, it is readyFilter's order. Otherwise
// an issue whose paths overlap a held issue's is ranked after every one
// that does not, keeping readyFilter's order within each group: it walks
// the ready ids in that order, reading their paths a batch at a time, and
// stops once it has limit without overlaps.
func rankReady(ctx context.Context, q querier, actor Actor, now time.Time, limit int) ([]IssueID, map[IssueID][]IssueID, error) {
	held, err := loadHeld(ctx, q, actor, now)
	if err != nil {
		return nil, nil, err
	}
	if held == nil {
		ids, err := readyIDList(ctx, q, now, limit)
		return ids, nil, err
	}
	all, err := readyIDList(ctx, q, now, 0)
	if err != nil {
		return nil, nil, err
	}
	var free, contested []IssueID
	overlaps := map[IssueID][]IssueID{}
	for batch := range slices.Chunk(all, readyBatch) {
		if len(free) >= limit {
			break
		}
		ps, err := pathsOf(ctx, q, batch)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range batch {
			if o := held.match(ps[id], id); len(o) > 0 {
				overlaps[id] = o
				contested = append(contested, id)
			} else {
				free = append(free, id)
			}
		}
	}
	out := slices.Concat(free, contested)
	out = out[:min(len(out), limit)]
	kept := map[IssueID][]IssueID{}
	for _, id := range out {
		if o, ok := overlaps[id]; ok {
			kept[id] = o
		}
	}
	return out, kept, nil
}

// readyIDList returns the ready ids in readyFilter's order, at most limit,
// or all of them when limit is 0.
func readyIDList(ctx context.Context, q querier, now time.Time, limit int) ([]IssueID, error) {
	query := blockedCTE + `SELECT i.id FROM issues i ` + readyFilter
	args := []any{now}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []IssueID
	for rows.Next() {
		var id IssueID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("ready: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	return out, nil
}

// IssueFiles returns an issue's likely files, from one snapshot: its
// declared paths, in path order, then its commit paths, most recently
// recorded first, at most n in all (0 means proto.MaxShowPaths), and the
// issues held by sessions other than actor's whose paths overlap them. A
// missing issue is ErrNotFound.
func (s *Store) IssueFiles(ctx context.Context, actor Actor, id IssueID, n int) (Files, error) {
	if err := id.Validate(); err != nil {
		return Files{}, err
	}
	n = clampLimit(n, proto.MaxShowPaths, 1000)
	tx, end, err := s.beginRead(ctx)
	if err != nil {
		return Files{}, fmt.Errorf("files of %s: %w", id, err)
	}
	defer end()
	if err := mustExist(ctx, tx, id); err != nil {
		return Files{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT path, source, principal, session, first_at, last_at
  FROM issue_paths WHERE issue_id = ?`, string(id))
	if err != nil {
		return Files{}, fmt.Errorf("files of %s: %w", id, err)
	}
	var all []IssuePath
	for rows.Next() {
		var p IssuePath
		if err := rows.Scan(&p.Path, &p.Source, &p.By.Principal, &p.By.Session, &p.FirstAt, &p.LastAt); err != nil {
			_ = rows.Close()
			return Files{}, fmt.Errorf("files of %s: %w", id, err)
		}
		p.FirstAt, p.LastAt = p.FirstAt.UTC(), p.LastAt.UTC()
		all = append(all, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Files{}, fmt.Errorf("files of %s: %w", id, err)
	}
	slices.SortFunc(all, func(a, b IssuePath) int {
		da, db := a.Source == PathDeclared, b.Source == PathDeclared
		switch {
		case da != db:
			if da {
				return -1
			}
			return 1
		case da:
			return cmp.Compare(a.Path, b.Path)
		}
		return cmp.Or(b.LastAt.Compare(a.LastAt), cmp.Compare(a.Path, b.Path))
	})
	held, err := loadHeld(ctx, tx, actor, s.now())
	if err != nil {
		return Files{}, err
	}
	ps := make([]string, len(all))
	for i, p := range all {
		ps[i] = p.Path
	}
	out := Files{Paths: all[:min(len(all), n)], More: max(len(all)-n, 0)}
	for _, o := range held.match(ps, id) {
		out.Overlaps = append(out.Overlaps, Overlap{Issue: o, Holder: held.holder[o]})
	}
	return out, nil
}
