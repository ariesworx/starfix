package bdimport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/store"
)

// Options configure Import.
type Options struct {
	// Actor is recorded on every event. Its principal also stands in as
	// the author of records that name none.
	Actor store.Actor
	// DryRun reports what would change and writes nothing.
	DryRun bool
}

// Import reads bd JSONL from r and writes it to st. Problems with
// individual records go in the report and the rest still imports; the
// error is for failures that stop the run (reading r, or the store
// failing). See the package documentation for the mapping.
func Import(ctx context.Context, st *store.Store, r io.Reader, opts Options) (*Report, error) {
	rep := &Report{DryRun: opts.DryRun, Problems: []Problem{}}
	lines, mems, err := parse(r, opts.Actor.Principal, rep)
	if err != nil {
		return rep, err
	}
	im := &importer{st: st, opts: opts, rep: rep, done: map[store.IssueID]bool{},
		inFile: map[store.IssueID]*line{}}
	for i := range lines {
		im.inFile[lines[i].issue.ID] = &lines[i]
	}
	if err := im.loadStore(ctx); err != nil {
		return rep, err
	}
	if err := im.issues(ctx, lines); err != nil {
		return rep, err
	}
	if err := im.deps(ctx, lines); err != nil {
		return rep, err
	}
	if err := im.comments(ctx, lines); err != nil {
		return rep, err
	}
	return rep, im.memories(ctx, mems)
}

// Fix lines shared by several kinds of problem.
const (
	fixReexport = "correct it in bd and export again, or delete the line, then import again"
	fixNone     = "none needed; this is informational"
	fixStale    = "none needed if starfix holds the right version; otherwise edit it in starfix"
)

// parse reads every line, maps the issues and memories, and reports what
// it cannot use. A later line with the same issue ID, or memory key,
// replaces an earlier one.
func parse(r io.Reader, principal string, rep *Report) ([]line, []memLine, error) {
	var out []line
	var mems []memLine
	index := map[store.IssueID]int{}
	memIndex := map[string]int{}
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		b, readErr := br.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, nil, fmt.Errorf("read line %d: %w", n, readErr)
		}
		if b = bytes.TrimSpace(b); len(b) > 0 {
			rep.Lines = n
			switch l, m, ok := parseLine(b, n, principal, rep); {
			case !ok:
			case m != nil:
				if i, dup := memIndex[m.mem.Key]; dup {
					rep.warn("memory-duplicate", "duplicate", m.mem.Key, n,
						"remove the earlier line if the last one is not the version to keep",
						"the same memory key appears on more than one line; the last one is used")
					mems[i] = *m
				} else {
					memIndex[m.mem.Key] = len(mems)
					mems = append(mems, *m)
				}
			default:
				if i, dup := index[l.issue.ID]; dup {
					rep.warn("duplicate", "duplicate", string(l.issue.ID), n,
						"remove the earlier line if the last one is not the version to keep",
						"the same id appears on more than one line; the last one is used")
					out[i] = l
				} else {
					index[l.issue.ID] = len(out)
					out = append(out, l)
				}
			}
		}
		if readErr != nil {
			return out, mems, nil
		}
	}
}

// parseLine maps b, the text of line n, to an issue, or to a memory when
// the memory it returns is not nil. It returns false for a line it skips
// or cannot map, having recorded that in rep.
func parseLine(b []byte, n int, principal string, rep *Report) (line, *memLine, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		rep.fail("parse", n, "repair or delete the line, then import again", "line %d is not a JSON object: %v", n, err)
		return line{}, nil, false
	}
	if _, ok := raw["_schema"]; ok {
		rep.Skipped++
		return line{}, nil, false
	}
	var typ string
	if t, ok := raw["_type"]; ok {
		_ = json.Unmarshal(t, &typ)
	}
	switch typ {
	case "", "issue":
	case "memory":
		m, ok := parseMemory(b, raw, n, rep)
		return line{}, &m, ok
	default:
		rep.Skipped++
		rep.warn("record:"+typ, "record", "", n, fixNone, fmt.Sprintf("records of _type %q are not imported", typ))
		return line{}, nil, false
	}
	var bi bdIssue
	if err := json.Unmarshal(b, &bi); err != nil {
		rep.Issues.Failed++
		rep.fail("invalid", n, fixReexport, "line %d: %v", n, err)
		return line{}, nil, false
	}
	if bi.Status == "tombstone" {
		rep.Skipped++
		rep.warn("tombstone", "tombstone", bi.ID, n, fixNone, "tombstones (issues deleted in bd) are skipped")
		return line{}, nil, false
	}
	warn := func(kind, detail string) { warnMapping(rep, kind, detail, bi.ID, n) }
	l, err := mapIssue(bi, n, principal, warn)
	if err != nil {
		rep.Issues.Failed++
		p := rep.fail("invalid", n, fixReexport, "line %d: %v", n, err)
		if bi.ID != "" {
			p.IDs = []string{bi.ID}
		}
		return line{}, nil, false
	}
	for _, f := range unheld(raw) {
		rep.warn("field:"+f, "field", bi.ID, n, "keep bd's export if you need it; the field has no starfix column yet",
			fmt.Sprintf("field %s is not stored", f))
	}
	return l, nil, true
}

// warnMapping reports a change mapIssue made to issue id, on line n, to
// fit the store. kind names the change and detail its subject, such as
// the bd status or the field.
func warnMapping(rep *Report, kind, detail, id string, n int) {
	key := kind + ":" + detail
	switch kind {
	case "status":
		msg := fmt.Sprintf("status %q stored as open (closed when closed_at is set), labeled bd-status:%s", detail, detail)
		switch detail {
		case "pinned":
			msg = "status pinned stored as open with the pinned flag"
		case "hooked":
			msg = "status hooked stored as in_progress, labeled bd-status:hooked"
		}
		rep.warn(key, "status", id, n, fixNone, msg)
	case "type":
		rep.warn(key, "type", id, n, fixNone,
			fmt.Sprintf("type %q stored as task, labeled bd-type:%s", detail, detail))
	case "priority":
		rep.warn(key, "priority", id, n, fixNone, fmt.Sprintf("priority %s is outside 0-4; clamped", detail))
	case "label":
		rep.warn(key, "label", id, n, "rename the label in bd (1-64 characters, no spaces or commas) and import again",
			fmt.Sprintf("label %q is not a valid starfix label; skipped", detail))
	case "text":
		rep.warn(key, "text", id, n, fixNone,
			detail+" had control or bidirectional characters; they were removed, and line breaks in a one-line field became spaces")
	case "no-created-at":
		rep.warn(key, "created_at", id, n, fixNone, "no created_at; updated_at used instead")
	case "thread-id":
		rep.warn(key, "field", id, n, "keep bd's export if you need it; the field has no starfix column yet",
			"dependency field thread_id is not stored")
	case "dep-metadata":
		rep.warn(key, "metadata", id, n, "repair the metadata in bd and import again",
			"dependency metadata is not valid JSON; the edge is imported without it")
	}
}

// importer carries state across the three passes.
type importer struct {
	st     *store.Store
	opts   Options
	rep    *Report
	inFile map[store.IssueID]*line
	// stored holds every issue in the store before the run; their parents
	// are in g.
	stored map[store.IssueID]bool
	// done holds issues this run imported (or found up to date).
	done map[store.IssueID]bool
	g    graph
}

// graph is the store's waits-on relation: parent links and blocking
// edges, kept up to date as the run adds to it, so cycles are found the
// same way in a dry run as in a real one.
type graph struct {
	parent map[store.IssueID]store.IssueID
	blocks map[store.IssueID][]store.IssueID
}

// path reports whether an edge from → to would close a cycle: when to
// already reaches from, through parent links and blocking edges, it
// returns that chain, to first and from last; otherwise nil. When from
// and to are the same, the chain is that one issue.
func (g *graph) path(from, to store.IssueID) []store.IssueID {
	if from == to {
		return []store.IssueID{to}
	}
	prev := map[store.IssueID]store.IssueID{to: ""}
	queue := []store.IssueID{to}
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		next := slices.Clone(g.blocks[x])
		if p := g.parent[x]; p != "" {
			next = append(next, p)
		}
		for _, y := range next {
			if _, seen := prev[y]; seen {
				continue
			}
			prev[y] = x
			if y == from {
				var out []store.IssueID
				for z := y; z != ""; z = prev[z] {
					out = append(out, z)
				}
				slices.Reverse(out)
				return out
			}
			queue = append(queue, y)
		}
	}
	return nil
}

// chain joins ids with arrows, for messages.
func chain(ids []store.IssueID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, " → ")
}

// strs returns ids as strings, in order, each once.
func strs(ids []store.IssueID) []string {
	var out []string
	for _, id := range ids {
		if !slices.Contains(out, string(id)) {
			out = append(out, string(id))
		}
	}
	return out
}

// loadStore reads the issues the store already holds into im.stored, and
// their parent links and blocking edges into im.g.
func (im *importer) loadStore(ctx context.Context) error {
	im.stored = map[store.IssueID]bool{}
	im.g = graph{parent: map[store.IssueID]store.IssueID{}, blocks: map[store.IssueID][]store.IssueID{}}
	var cur store.Cursor
	for {
		page, err := im.st.List(ctx, store.Filter{Limit: 500, Cursor: cur})
		if err != nil {
			return fmt.Errorf("read store: %w", err)
		}
		for _, is := range page.Issues {
			im.stored[is.ID] = true
			if is.ParentID != "" {
				im.g.parent[is.ID] = is.ParentID
			}
		}
		if page.Next == "" {
			break
		}
		cur = page.Next
	}
	deps, err := im.st.AllDeps(ctx)
	if err != nil {
		return fmt.Errorf("read store: %w", err)
	}
	for _, d := range deps {
		if d.Type.Blocking() {
			im.g.blocks[d.From] = append(im.g.blocks[d.From], d.To)
		}
	}
	return nil
}

// recordError reports whether err concerns one record, as opposed to the
// store or the connection failing.
func recordError(err error) bool {
	for _, e := range []error{store.ErrInvalid, store.ErrNotFound, store.ErrCycle, store.ErrExists, store.ErrConflict} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// count adds outcome o to c.
func count(c *Counts, o store.ImportOutcome) {
	switch o {
	case store.ImportCreated:
		c.Created++
	case store.ImportUpdated:
		c.Updated++
	case store.ImportUnchanged:
		c.Unchanged++
	case store.ImportStale:
		c.Stale++
	}
}

// order returns the lines parents first, and picks each issue's parent,
// reporting extra, dangling and cyclic parent links.
func (im *importer) order(lines []line) []*line {
	state := map[store.IssueID]int{} // 1 visiting, 2 done
	var out []*line
	var stack []store.IssueID
	var visit func(l *line)
	visit = func(l *line) {
		id := l.issue.ID
		state[id] = 1
		stack = append(stack, id)
		if len(l.parents) > 1 {
			im.rep.warn("parents", "parent", string(id), l.n, "remove the extra parent-child dependencies in bd",
				"more than one parent-child dependency; the first is used")
		}
		if len(l.parents) > 0 {
			p := l.parents[0]
			pl, inFile := im.inFile[p]
			switch {
			case inFile && state[p] == 1:
				i := slices.Index(stack, p)
				cyc := append(slices.Clone(stack[i:]), p)
				im.rep.fail("cycle", l.n, "remove one of the parent-child links in bd and import again",
					"parent links form a cycle %s; %s is imported without a parent", chain(cyc), id).IDs = strs(cyc)
			case inFile:
				if state[p] == 0 {
					visit(pl)
				}
				l.issue.ParentID = p
			case im.stored[p]:
				l.issue.ParentID = p
			default:
				im.rep.fail("dangling", l.n, "import "+string(p)+" too, or set the parent in starfix after the import",
					"%s has parent %s, which is not in the file or the store; imported without a parent", id, p).IDs = []string{string(id), string(p)}
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		out = append(out, l)
	}
	for i := range lines {
		if state[lines[i].issue.ID] == 0 {
			visit(&lines[i])
		}
	}
	return out
}

// issues imports the issues, parents first. A parent that is dangling, or
// would close a cycle, is reported and dropped, and the issue imported
// without it. Like deps and comments, it reports a problem with one record
// and goes on; it returns only an error that stops the run.
func (im *importer) issues(ctx context.Context, lines []line) error {
	for _, l := range im.order(lines) {
		is := l.issue
		if is.ParentID != "" && !im.done[is.ParentID] && !im.stored[is.ParentID] {
			// The parent's own line failed.
			im.rep.fail("dangling", l.n, "fix the parent's problem above and import again",
				"%s has parent %s, which was not imported; imported without a parent", is.ID, is.ParentID).IDs = strs([]store.IssueID{is.ID, is.ParentID})
			is.ParentID = ""
		}
		plan, err := im.st.PlanImportIssue(ctx, is)
		if err == nil && plan.Outcome != store.ImportStale && is.ParentID != "" {
			old := im.g.parent[is.ID]
			delete(im.g.parent, is.ID)
			if p := im.g.path(is.ID, is.ParentID); p != nil {
				cyc := append([]store.IssueID{is.ID}, p...)
				im.rep.fail("cycle", l.n, "remove the parent link or one of the blocking edges in bd and import again",
					"parent %s would make a cycle %s; %s is imported without a parent", is.ParentID, chain(cyc), is.ID).IDs = strs(cyc)
				is.ParentID = ""
				plan, err = im.st.PlanImportIssue(ctx, is)
			}
			if old != "" {
				im.g.parent[is.ID] = old
			}
		}
		res := plan
		if err == nil && !im.opts.DryRun {
			res, err = im.st.ImportIssue(ctx, im.opts.Actor, is)
		}
		if err != nil {
			if !recordError(err) {
				return fmt.Errorf("import %s: %w", is.ID, err)
			}
			im.rep.Issues.Failed++
			im.rep.fail("invalid", l.n, fixReexport, "%v", err).IDs = []string{string(is.ID)}
			continue
		}
		im.done[is.ID] = true
		count(&im.rep.Issues, res.Outcome)
		im.rep.LabelsAdded += res.LabelsAdded
		switch res.Outcome {
		case store.ImportStale:
			im.rep.warn("stale-issue", "stale", string(is.ID), l.n, fixStale,
				"the store's copy differs and is as new or newer; kept it")
		default:
			if is.ParentID != "" {
				im.g.parent[is.ID] = is.ParentID
			} else {
				delete(im.g.parent, is.ID)
			}
		}
	}
	return nil
}

// exists reports whether id was in the store before the run, or this run
// imported it (in a dry run, would have).
func (im *importer) exists(id store.IssueID) bool { return im.done[id] || im.stored[id] }

// deps imports every line's dependencies, once all the issues are in.
func (im *importer) deps(ctx context.Context, lines []line) error {
	for i := range lines {
		l := &lines[i]
		for _, ref := range l.deps {
			if err := im.dep(ctx, l, ref); err != nil {
				return err
			}
		}
	}
	return nil
}

// dep imports one dependency of line l. An unsupported type, a missing
// end, a self-dependency or a blocking edge that would close a cycle is
// reported and skipped.
func (im *importer) dep(ctx context.Context, l *line, ref depRef) error {
	d := ref.dep
	edge := fmt.Sprintf("%s → %s (%s)", d.From, d.To, ref.bdType)
	ids := []string{string(d.From), string(d.To)}
	if ref.bdType == "relates-to" {
		d.Type = store.DepRelated
		im.rep.warn("relates-to", "dep-type", string(d.From), l.n, fixNone, "dependency type relates-to stored as related")
	}
	switch {
	case strings.HasPrefix(string(d.To), "external:"):
		im.rep.warn("external", "dep-type", string(d.From), l.n,
			"keep the external dependency in bd; cross-project edges arrive in stage 6",
			"external: dependencies are not supported yet; skipped")
		return nil
	case !d.Type.Valid():
		im.rep.warn("dep-type:"+ref.bdType, "dep-type", string(d.From), l.n,
			"keep bd's export if you need these edges; the store has no such type",
			fmt.Sprintf("dependency type %q is not supported; skipped", ref.bdType))
		return nil
	}
	for _, id := range []store.IssueID{d.From, d.To} {
		if err := id.Validate(); err != nil || !im.exists(id) {
			im.rep.Deps.Failed++
			why := "is not in the file or the store"
			if _, ok := im.inFile[id]; ok {
				why = "was not imported"
			}
			im.rep.fail("dangling", l.n, "import "+string(id)+" too, or remove the dependency in bd",
				"%s: %s %s; skipped", edge, id, why).IDs = ids
			return nil
		}
	}
	if d.From == d.To {
		im.rep.Deps.Failed++
		im.rep.fail("cycle", l.n, "remove the dependency in bd", "%s: an issue cannot depend on itself; skipped", edge).IDs = ids[:1]
		return nil
	}
	if d.Type.Blocking() {
		if p := im.g.path(d.From, d.To); p != nil {
			im.rep.Deps.Failed++
			cyc := append([]store.IssueID{d.From}, p...)
			im.rep.fail("cycle", l.n, "remove one of the edges in bd and import again",
				"%s would make a cycle %s; skipped", edge, chain(cyc)).IDs = strs(cyc)
			return nil
		}
	}
	out, err := im.st.PlanImportDep(ctx, d)
	if err == nil && !im.opts.DryRun {
		out, err = im.st.ImportDep(ctx, im.opts.Actor, d)
	}
	if err != nil {
		if !recordError(err) {
			return fmt.Errorf("import %s: %w", edge, err)
		}
		im.rep.Deps.Failed++
		im.rep.fail("invalid", l.n, fixReexport, "%s: %v", edge, err).IDs = ids
		return nil
	}
	count(&im.rep.Deps, out)
	switch out {
	case store.ImportStale:
		im.rep.warn("stale-dep", "stale", string(d.From), l.n, fixStale,
			"a stored edge of the same type has other metadata; kept it")
	case store.ImportCreated:
		if d.Type.Blocking() {
			im.g.blocks[d.From] = append(im.g.blocks[d.From], d.To)
		}
	}
	return nil
}

// comments imports the comments of the issues that did not fail, and
// counts those of the issues that did as failed.
func (im *importer) comments(ctx context.Context, lines []line) error {
	for i := range lines {
		l := &lines[i]
		for _, c := range l.comments {
			if !im.done[c.Issue] {
				// The issue failed and was reported; its comments go with it.
				im.rep.Comments.Failed++
				continue
			}
			out, err := im.st.PlanImportComment(ctx, c)
			if err == nil && !im.opts.DryRun {
				out, err = im.st.ImportComment(ctx, im.opts.Actor, c)
			}
			if err != nil {
				if !recordError(err) {
					return fmt.Errorf("import comment on %s: %w", c.Issue, err)
				}
				im.rep.Comments.Failed++
				im.rep.fail("invalid", l.n, fixReexport, "comment on %s: %v", c.Issue, err).IDs = []string{string(c.Issue)}
				continue
			}
			count(&im.rep.Comments, out)
			if out == store.ImportStale {
				im.rep.warn("stale-comment", "stale", string(c.Issue), l.n, fixStale,
					"a stored comment with the same id has other content; kept it")
			}
		}
	}
	return nil
}
