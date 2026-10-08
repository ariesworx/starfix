package bdimport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/store"
)

// exportIssue is one exported line: "_type" first, as bd writes it.
type exportIssue struct {
	Type string `json:"_type"`
	bdIssue
}

// Export writes every issue in st to w as bd JSONL, oldest first, with its
// labels, outgoing dependencies (the parent as a parent-child dependency)
// and comments, and returns the number of issues written. bd can import
// the result; so can Import, which reproduces the same store.
//
// The bd-type: and bd-status: labels Import adds are turned back into
// issue_type and status, so a backlog that came from bd goes back as it
// came. Fields bd has no place for (expires_at, a comment's session) are
// written under their own names, which bd ignores.
func Export(ctx context.Context, st *store.Store, w io.Writer) (int, error) {
	deps, err := st.AllDeps(ctx)
	if err != nil {
		return 0, err
	}
	byFrom := map[store.IssueID][]store.Dep{}
	for _, d := range deps {
		byFrom[d.From] = append(byFrom[d.From], d)
	}
	comments, err := st.AllComments(ctx)
	if err != nil {
		return 0, err
	}
	byIssue := map[store.IssueID][]store.Comment{}
	for _, c := range comments {
		byIssue[c.Issue] = append(byIssue[c.Issue], c)
	}

	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	n := 0
	var cur store.Cursor
	for {
		page, err := st.List(ctx, store.Filter{Limit: 500, Cursor: cur})
		if err != nil {
			return n, err
		}
		for _, is := range page.Issues {
			rec, err := toBD(is, byFrom[is.ID], byIssue[is.ID])
			if err != nil {
				return n, err
			}
			if err := enc.Encode(rec); err != nil {
				return n, fmt.Errorf("write %s: %w", is.ID, err)
			}
			n++
		}
		if page.Next == "" {
			break
		}
		cur = page.Next
	}
	if err := bw.Flush(); err != nil {
		return n, fmt.Errorf("write: %w", err)
	}
	return n, nil
}

// compact returns m without insignificant space, or nil when m is empty.
func compact(m json.RawMessage) (json.RawMessage, error) {
	if len(m) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, m); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// toBD converts is, with its outgoing deps and its comments, to one bd
// line, as Export describes.
func toBD(is store.Issue, deps []store.Dep, comments []store.Comment) (exportIssue, error) {
	p := int(is.Priority)
	b := bdIssue{
		ID: string(is.ID), Title: is.Title, Description: is.Body, Design: is.Design,
		AcceptanceCriteria: is.Acceptance, Notes: is.Notes, Status: string(is.Status), Priority: &p,
		IssueType: string(is.Type), Assignee: is.Assignee, Owner: is.Owner, CreatedAt: is.CreatedAt,
		CreatedBy: is.CreatedBy, UpdatedAt: is.UpdatedAt, ClosedAt: is.ClosedAt, CloseReason: is.CloseReason,
		DueAt: is.DueAt, DeferUntil: is.DeferUntil, ExpiresAt: is.ExpiresAt, Ephemeral: is.Ephemeral,
		Pinned: is.Pinned, IsTemplate: is.Template,
	}
	meta, err := compact(is.Metadata)
	if err != nil {
		return exportIssue{}, fmt.Errorf("issue %s: %w", is.ID, err)
	}
	b.Metadata = meta
	for _, l := range is.Labels {
		switch {
		case strings.HasPrefix(l, "bd-type:") && is.Type == store.TypeTask:
			b.IssueType = strings.TrimPrefix(l, "bd-type:")
		case strings.HasPrefix(l, "bd-status:"):
			b.Status = strings.TrimPrefix(l, "bd-status:")
		default:
			b.Labels = append(b.Labels, l)
		}
	}
	if is.ParentID != "" {
		b.Dependencies = append(b.Dependencies, bdDep{IssueID: string(is.ID), DependsOnID: string(is.ParentID),
			Type: "parent-child", CreatedAt: is.CreatedAt, CreatedBy: is.CreatedBy})
	}
	for _, d := range deps {
		bd := bdDep{IssueID: string(d.From), DependsOnID: string(d.To), Type: string(d.Type),
			CreatedAt: d.CreatedAt, CreatedBy: d.CreatedBy}
		m, err := compact(d.Metadata)
		if err != nil {
			return exportIssue{}, fmt.Errorf("dep %s → %s: %w", d.From, d.To, err)
		}
		if m != nil {
			// bd writes edge metadata as a JSON string.
			s, err := json.Marshal(string(m))
			if err != nil {
				return exportIssue{}, fmt.Errorf("dep %s → %s: %w", d.From, d.To, err)
			}
			bd.Metadata = s
		}
		b.Dependencies = append(b.Dependencies, bd)
	}
	for _, c := range comments {
		id, err := json.Marshal(c.ID)
		if err != nil {
			return exportIssue{}, fmt.Errorf("comment %s: %w", c.ID, err)
		}
		b.Comments = append(b.Comments, bdComment{ID: id, IssueID: string(c.Issue), Author: c.Author,
			Text: c.Body, CreatedAt: c.CreatedAt, Session: c.Session})
	}
	b.Labels = slices.Clip(b.Labels)
	return exportIssue{Type: "issue", bdIssue: b}, nil
}
