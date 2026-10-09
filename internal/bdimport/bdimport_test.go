package bdimport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
	"github.com/ariesworx/starfix/internal/store"
)

var (
	server    *dolttest.Server
	serverErr error
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "starfix-bdimport-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	server, serverErr = dolttest.Start(ctx, dir)
	cancel()
	if server != nil {
		defer func() { _ = server.Stop() }()
	}
	return m.Run()
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	if errors.Is(serverErr, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the import tests")
	}
	if serverErr != nil {
		t.Fatalf("dolt sql-server: %v", serverErr)
	}
	dsn, err := server.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.Context(), dsn, store.Options{Prefix: "sf", CommitInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return s
}

var admin = store.Actor{Principal: "admin", Session: "import-test", Machine: "server"}

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/backlog.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func importBytes(t *testing.T, s *store.Store, b []byte, dry bool) *Report {
	t.Helper()
	rep, err := Import(t.Context(), s, bytes.NewReader(b), Options{Actor: admin, DryRun: dry})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	return rep
}

// snapshot is everything the store holds for a backlog, minus revisions.
type snapshot struct {
	Issues   []store.Issue
	Deps     []store.Dep
	Comments []store.Comment
}

func snap(t *testing.T, s *store.Store) snapshot {
	t.Helper()
	ctx := t.Context()
	var out snapshot
	var cur store.Cursor
	for {
		page, err := s.List(ctx, store.Filter{Limit: 500, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, is := range page.Issues {
			is.Rev = 0
			is.Metadata = canon(t, is.Metadata)
			out.Issues = append(out.Issues, is)
		}
		if cur = page.Next; cur == "" {
			break
		}
	}
	var err error
	if out.Deps, err = s.AllDeps(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range out.Deps {
		out.Deps[i].Metadata = canon(t, out.Deps[i].Metadata)
	}
	if out.Comments, err = s.AllComments(ctx); err != nil {
		t.Fatal(err)
	}
	return out
}

func canon(t *testing.T, m json.RawMessage) json.RawMessage {
	t.Helper()
	if len(m) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(m, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func lastSeq(t *testing.T, s *store.Store) int64 {
	t.Helper()
	var seq int64
	for {
		evs, err := s.Events(t.Context(), seq, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if len(evs) == 0 {
			return seq
		}
		seq = evs[len(evs)-1].Seq
	}
}

func problemKeys(rep *Report, level string) []string {
	var out []string
	for _, p := range rep.Problems {
		if p.Level == level {
			out = append(out, p.Kind+": "+p.Message+" ["+strings.Join(p.IDs, ",")+"]")
		}
	}
	sort.Strings(out)
	return out
}

func TestImportBacklog(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	rep := importBytes(t, s, fixture(t), false)

	if rep.Errors() != 0 {
		t.Fatalf("errors:\n%s", strings.Join(problemKeys(rep, LevelError), "\n"))
	}
	wantCounts := []struct {
		name string
		got  Counts
		want Counts
	}{
		{"issues", rep.Issues, Counts{Created: 10}},
		{"deps", rep.Deps, Counts{Created: 8}},
		{"comments", rep.Comments, Counts{Created: 3}},
		{"memories", rep.Memories, Counts{Created: 1}},
	}
	for _, c := range wantCounts {
		if c.got != c.want {
			t.Errorf("%s = %+v, want %+v", c.name, c.got, c.want)
		}
	}
	if rep.Skipped != 2 || rep.LabelsAdded != 12 || rep.Lines != 13 {
		t.Errorf("skipped %d, labels %d, lines %d; want 2, 12, 13", rep.Skipped, rep.LabelsAdded, rep.Lines)
	}

	// Every field bd has and the store cannot hold is reported, with ids.
	wantWarnings := []string{
		`dep-type: dependency type "replies-to" is not supported; skipped [acme-w8n]`,
		`dep-type: dependency type "tracks" is not supported; skipped [acme-d0c]`,
		`dep-type: dependency type relates-to stored as related [acme-d0c]`,
		`dep-type: external: dependencies are not supported yet; skipped [acme-z1x]`,
		`field: dependency field thread_id is not stored [acme-w8n]`,
		`field: field closed_by_session is not stored [acme-q4m]`,
		`field: field estimated_minutes is not stored [acme-7k2.1]`,
		`field: field external_ref is not stored [acme-q4m]`,
		`field: field started_at is not stored [acme-7k2.1]`,
		`label: label "has space" is not a valid starfix label; skipped [acme-t3m]`,
		`status: status hooked stored as in_progress, labeled bd-status:hooked [acme-h7k]`,
		`status: status pinned stored as open with the pinned flag [acme-p1n]`,
		`tombstone: tombstones (issues deleted in bd) are skipped [acme-old]`,
		`type: type "decision" stored as task, labeled bd-type:decision [acme-p1n]`,
		`type: type "spike" stored as task, labeled bd-type:spike [acme-7k2.2]`,
		`type: type "story" stored as task, labeled bd-type:story [acme-t3m]`,
	}
	if got := problemKeys(rep, LevelWarning); !slices.Equal(got, wantWarnings) {
		t.Errorf("warnings =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(wantWarnings, "\n  "))
	}
	for _, p := range rep.Problems {
		if p.Fix == "" {
			t.Errorf("problem without a fix: %+v", p)
		}
	}

	// at also runs in subtests, through the checks below, and from there
	// may mark this test failed but not stop it.
	at := func(s string) time.Time {
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Error(err)
		}
		return tm.UTC()
	}
	// get takes the subtest's t: only its own goroutine may stop it.
	get := func(t *testing.T, id store.IssueID) store.Issue {
		t.Helper()
		is, err := s.GetIssue(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return is
	}
	issueCases := []struct {
		id    store.IssueID
		check func(store.Issue) string
	}{
		{"acme-7k2.1", func(is store.Issue) string {
			ok := is.ParentID == "acme-7k2" && is.Status == store.StatusInProgress && is.Priority == store.P1 &&
				is.Type == store.TypeFeature && is.Assignee == "sam" && is.CreatedBy == "riley" &&
				is.Body == "Validate postal codes before submit." && strings.HasPrefix(is.Design, "Client-side") &&
				strings.HasPrefix(is.Acceptance, "- invalid codes") && is.Notes == "Started with the US format." &&
				is.CreatedAt.Equal(at("2026-01-06T15:14:03.123456Z")) && is.UpdatedAt.Equal(at("2026-01-21T08:00:00Z")) &&
				slices.Equal(is.Labels, []string{"area:checkout", "frontend"})
			var m map[string]any
			if err := json.Unmarshal(is.Metadata, &m); err != nil || m["team"] != "web" || m["points"] != 3.0 {
				ok = false
			}
			return fmt.Sprint(ok)
		}},
		{"acme-q4m", func(is store.Issue) string {
			return fmt.Sprint(is.Status == store.StatusClosed && is.Priority == store.P0 && is.ClosedAt != nil &&
				is.ClosedAt.Equal(at("2026-01-09T17:20:00Z")) && is.CloseReason == "Fixed: empty bodies now return 400.")
		}},
		{"acme-z1x", func(is store.Issue) string {
			return fmt.Sprint(is.Status == store.StatusDeferred && is.DeferUntil != nil && is.DeferUntil.Equal(at("2026-02-01T09:00:00Z")))
		}},
		{"acme-w8n", func(is store.Issue) string {
			return fmt.Sprint(is.Status == store.StatusBlocked && is.DueAt != nil && is.DueAt.Equal(at("2026-03-31T00:00:00Z")))
		}},
		{"acme-p1n", func(is store.Issue) string {
			return fmt.Sprint(is.Status == store.StatusOpen && is.Pinned && is.Type == store.TypeTask &&
				slices.Equal(is.Labels, []string{"bd-type:decision", "process"}))
		}},
		{"acme-h7k", func(is store.Issue) string {
			return fmt.Sprint(is.Status == store.StatusInProgress && is.Ephemeral && is.CreatedBy == "admin" &&
				slices.Equal(is.Labels, []string{"bd-status:hooked"}))
		}},
		{"acme-t3m", func(is store.Issue) string {
			return fmt.Sprint(is.Template && slices.Equal(is.Labels, []string{"bd-type:story", "template"}))
		}},
		{"acme-7k2.2", func(is store.Issue) string {
			return fmt.Sprint(is.ParentID == "acme-7k2" && is.Type == store.TypeTask)
		}},
	}
	for _, tc := range issueCases {
		t.Run(string(tc.id), func(t *testing.T) {
			is := get(t, tc.id)
			if tc.check(is) != "true" {
				t.Errorf("stored as %s", mustJSON(t, is))
			}
		})
	}

	deps, err := s.AllDeps(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var edges []string
	for _, d := range deps {
		edges = append(edges, fmt.Sprintf("%s %s %s %s", d.From, d.Type, d.To, string(canon(t, d.Metadata))))
	}
	wantEdges := []string{
		"acme-7k2.1 blocks acme-q4m ",
		"acme-7k2.2 related acme-7k2.1 ",
		"acme-d0c related acme-7k2 ",
		"acme-d0c duplicates acme-w8n ",
		"acme-d0c supersedes acme-w8n ",
		"acme-w8n conditional-blocks acme-7k2.1 ",
		"acme-w8n discovered-from acme-q4m ",
		`acme-w8n waits-for acme-z1x {"gate":"any-children","spawner_id":"acme-z1x"}`,
	}
	if !slices.Equal(edges, wantEdges) {
		t.Errorf("edges =\n  %s\nwant\n  %s", strings.Join(edges, "\n  "), strings.Join(wantEdges, "\n  "))
	}
	for _, d := range deps {
		if d.From == "acme-7k2.1" && (d.CreatedBy != "sam" || !d.CreatedAt.Equal(at("2026-01-07T10:00:00Z"))) {
			t.Errorf("dep author or time not kept: %+v", d)
		}
	}

	cs, err := s.Comments(ctx, "acme-7k2.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].Author != "sam" || cs[1].Author != "riley" ||
		!cs[0].CreatedAt.Equal(at("2026-01-11T16:45:00Z")) || cs[0].Body != "Postal code library picked; see the design." {
		t.Errorf("comments = %s", mustJSON(t, cs))
	}

	// Ready and blocked see the imported graph: acme-7k2.1 waits on a
	// closed bug, so it is not blocked by it.
	ready, err := s.Ready(ctx, store.Actor{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	var readyIDs []store.IssueID
	for _, is := range ready {
		readyIDs = append(readyIDs, is.ID)
	}
	if want := []store.IssueID{"acme-7k2", "acme-p1n", "acme-7k2.2"}; !slices.Equal(readyIDs, want) {
		t.Errorf("ready = %v, want %v", readyIDs, want)
	}
}

func TestImportIsIdempotent(t *testing.T) {
	s := newStore(t)
	b := fixture(t)
	first := importBytes(t, s, b, false)
	before, seq := snap(t, s), lastSeq(t, s)

	second := importBytes(t, s, b, false)
	if after := snap(t, s); mustJSON(t, after) != mustJSON(t, before) {
		t.Errorf("second import changed the store")
	}
	if got := lastSeq(t, s); got != seq {
		t.Errorf("second import wrote %d events", got-seq)
	}
	want := []struct {
		name string
		got  Counts
		n    int
	}{{"issues", second.Issues, first.Issues.Created}, {"deps", second.Deps, first.Deps.Created}, {"comments", second.Comments, first.Comments.Created},
		{"memories", second.Memories, first.Memories.Created}}
	for _, w := range want {
		if w.got != (Counts{Unchanged: w.n}) {
			t.Errorf("second import %s = %+v, want %d unchanged", w.name, w.got, w.n)
		}
	}
	if second.LabelsAdded != 0 || second.Errors() != 0 {
		t.Errorf("second import added %d labels with %d errors", second.LabelsAdded, second.Errors())
	}
	if second.Summary() != "imported 10 issues (10 unchanged), 8 deps (8 unchanged), 3 comments (3 unchanged), 1 memory (1 unchanged); 0 errors, 16 warnings" {
		t.Errorf("summary = %q", second.Summary())
	}
}

func TestDryRun(t *testing.T) {
	s := newStore(t)
	b := fixture(t)
	dry := importBytes(t, s, b, true)
	if got := snap(t, s); len(got.Issues)+len(got.Deps)+len(got.Comments) != 0 || lastSeq(t, s) != 0 {
		t.Fatalf("dry run wrote: %s", mustJSON(t, got))
	}
	if !strings.HasPrefix(dry.Summary(), "would import 10 issues (10 created), 8 deps (8 created), 3 comments (3 created), 1 memory (1 created)") {
		t.Errorf("dry summary = %q", dry.Summary())
	}
	done := importBytes(t, s, b, false)
	dry.DryRun = false
	if mustJSON(t, dry) != mustJSON(t, done) {
		t.Errorf("dry run report differs from the real one:\n%s\n%s", mustJSON(t, dry), mustJSON(t, done))
	}
	again := importBytes(t, s, b, true)
	if again.Issues != (Counts{Unchanged: 10}) || again.Deps != (Counts{Unchanged: 8}) {
		t.Errorf("dry run after import = %+v %+v", again.Issues, again.Deps)
	}
}

func TestRoundTrip(t *testing.T) {
	ctx := t.Context()
	s1 := newStore(t)
	importBytes(t, s1, fixture(t), false)
	var e1 bytes.Buffer
	n, err := Export(ctx, s1, &e1)
	if err != nil || n != 10 {
		t.Fatalf("export = %d, %v", n, err)
	}

	// Import the export into an empty store: it must hold the same data.
	s2 := newStore(t)
	rep := importBytes(t, s2, e1.Bytes(), false)
	if rep.Errors() != 0 || rep.Issues != (Counts{Created: 10}) || rep.Deps != (Counts{Created: 8}) || rep.Comments != (Counts{Created: 3}) {
		t.Fatalf("re-import of the export: %s", mustJSON(t, rep))
	}
	if a, b := mustJSON(t, snap(t, s1)), mustJSON(t, snap(t, s2)); a != b {
		t.Errorf("import → export → import changed the store:\nfirst  %s\nsecond %s", a, b)
	}
	var e2 bytes.Buffer
	if _, err := Export(ctx, s2, &e2); err != nil {
		t.Fatal(err)
	}
	if e1.String() != e2.String() {
		t.Errorf("exports differ:\n%s\n%s", e1.String(), e2.String())
	}

	// Importing its own export back changes nothing.
	same := importBytes(t, s1, e1.Bytes(), false)
	if same.Issues != (Counts{Unchanged: 10}) || same.Deps != (Counts{Unchanged: 8}) || same.Comments != (Counts{Unchanged: 3}) || same.LabelsAdded != 0 {
		t.Errorf("re-import into the source store: %s", mustJSON(t, same))
	}

	// The export is bd's format: bd's own types and statuses come back,
	// the parent is a parent-child dependency, edge metadata is a string.
	byID := map[string]map[string]json.RawMessage{}
	for l := range strings.SplitSeq(strings.TrimSpace(e1.String()), "\n") {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		var id string
		if err := json.Unmarshal(m["id"], &id); err != nil {
			t.Fatal(err)
		}
		byID[id] = m
	}
	lineCases := []struct {
		id, key, want string
	}{
		{"acme-7k2.2", "issue_type", `"spike"`},
		{"acme-7k2.2", "labels", ``},
		{"acme-h7k", "status", `"hooked"`},
		{"acme-p1n", "issue_type", `"decision"`},
		{"acme-p1n", "pinned", `true`},
		{"acme-7k2.1", "description", `"Validate postal codes before submit."`},
		{"acme-7k2.1", "acceptance_criteria", `"- invalid codes show an inline error\n- valid codes submit"`},
		{"acme-q4m", "_type", `"issue"`},
		{"acme-q4m", "priority", `0`},
		{"acme-q4m", "close_reason", `"Fixed: empty bodies now return 400."`},
		{"acme-t3m", "is_template", `true`},
		{"acme-h7k", "ephemeral", `true`},
	}
	for _, tc := range lineCases {
		if got := string(byID[tc.id][tc.key]); got != tc.want {
			t.Errorf("%s %s = %s, want %s", tc.id, tc.key, got, tc.want)
		}
	}
	var child bdIssue
	if err := json.Unmarshal([]byte(mustJSON(t, byID["acme-7k2.1"])), &child); err != nil {
		t.Fatal(err)
	}
	if len(child.Dependencies) == 0 || child.Dependencies[0].Type != "parent-child" || child.Dependencies[0].DependsOnID != "acme-7k2" {
		t.Errorf("parent not exported as parent-child: %+v", child.Dependencies)
	}
	var w8n bdIssue
	if err := json.Unmarshal([]byte(mustJSON(t, byID["acme-w8n"])), &w8n); err != nil {
		t.Fatal(err)
	}
	for _, d := range w8n.Dependencies {
		if d.Type == "waits-for" && string(d.Metadata) != `"{\"gate\":\"any-children\",\"spawner_id\":\"acme-z1x\"}"` {
			t.Errorf("waits-for metadata = %s", d.Metadata)
		}
	}
}

func TestReimportNewerAndOlder(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	base := `{"id":"ex-a1","title":"Original","status":"open","priority":2,"issue_type":"task","created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-02T00:00:00Z"}`
	importBytes(t, s, []byte(base), false)
	cases := []struct {
		name, line, title string
		want              Counts
		warn              bool
	}{
		{"older edit kept out", `{"id":"ex-a1","title":"Older","status":"open","priority":2,"issue_type":"task","created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-01T12:00:00Z"}`,
			"Original", Counts{Stale: 1}, true},
		{"newer edit applied", `{"id":"ex-a1","title":"Newer","status":"closed","closed_at":"2026-02-03T00:00:00Z","priority":2,"issue_type":"task","created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-03T00:00:00Z"}`,
			"Newer", Counts{Updated: 1}, false},
		{"same again", `{"id":"ex-a1","title":"Newer","status":"closed","closed_at":"2026-02-03T00:00:00Z","priority":2,"issue_type":"task","created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-03T00:00:00Z"}`,
			"Newer", Counts{Unchanged: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := importBytes(t, s, []byte(tc.line), false)
			if rep.Issues != tc.want {
				t.Errorf("issues = %+v, want %+v", rep.Issues, tc.want)
			}
			if got := len(problemKeys(rep, LevelWarning)) > 0; got != tc.warn {
				t.Errorf("warnings = %v", problemKeys(rep, LevelWarning))
			}
			is, err := s.GetIssue(ctx, "ex-a1")
			if err != nil || is.Title != tc.title {
				t.Errorf("title = %q, %v; want %q", is.Title, err, tc.title)
			}
		})
	}
}

func TestImportProblems(t *testing.T) {
	issue := func(id string, extra string) string {
		return fmt.Sprintf(`{"id":%q,"title":"Issue %s","status":"open","priority":2,"issue_type":"task","created_at":"2026-02-01T00:00:00Z","updated_at":"2026-02-01T00:00:00Z"%s}`, id, id, extra)
	}
	dep := func(from, to, typ string) string {
		return fmt.Sprintf(`{"issue_id":%q,"depends_on_id":%q,"type":%q,"created_at":"2026-02-01T00:00:00Z","created_by":"sam"}`, from, to, typ)
	}
	deps := func(ds ...string) string { return `,"dependencies":[` + strings.Join(ds, ",") + `]` }

	cases := []struct {
		name   string
		lines  []string
		errors []string
		issues Counts
		deps   Counts
	}{
		{
			name:   "not json",
			lines:  []string{`{"id":`, issue("ex-a1", "")},
			errors: []string{"parse []"},
			issues: Counts{Created: 1},
		},
		{
			name:   "invalid id",
			lines:  []string{issue("Ex A1", ""), issue("ex-b1", "")},
			errors: []string{"invalid [Ex A1]"},
			issues: Counts{Created: 1, Failed: 1},
		},
		{
			name:   "title too long",
			lines:  []string{fmt.Sprintf(`{"id":"ex-a1","title":%q,"created_at":"2026-02-01T00:00:00Z"}`, strings.Repeat("t", 501))},
			errors: []string{"invalid [ex-a1]"},
			issues: Counts{Failed: 1},
		},
		{
			name:   "dangling dependency",
			lines:  []string{issue("ex-a1", deps(dep("ex-a1", "ex-gone", "blocks")))},
			errors: []string{"dangling [ex-a1,ex-gone]"},
			issues: Counts{Created: 1},
			deps:   Counts{Failed: 1},
		},
		{
			name: "dependency on a failed issue",
			lines: []string{issue("ex-a1", deps(dep("ex-a1", "ex-b1", "blocks"))),
				`{"id":"ex-b1","title":"","created_at":"2026-02-01T00:00:00Z"}`},
			errors: []string{"dangling [ex-a1,ex-b1]", "invalid [ex-b1]"},
			issues: Counts{Created: 1, Failed: 1},
			deps:   Counts{Failed: 1},
		},
		{
			name: "blocking cycle",
			lines: []string{
				issue("ex-a1", deps(dep("ex-a1", "ex-b1", "blocks"))),
				issue("ex-b1", deps(dep("ex-b1", "ex-c1", "waits-for"))),
				issue("ex-c1", deps(dep("ex-c1", "ex-a1", "conditional-blocks"), dep("ex-c1", "ex-a1", "related"))),
			},
			errors: []string{"cycle [ex-c1,ex-a1,ex-b1]"},
			issues: Counts{Created: 3},
			deps:   Counts{Created: 3, Failed: 1},
		},
		{
			name: "cycle through a parent",
			lines: []string{
				issue("ex-p1", deps(dep("ex-p1", "ex-k1", "blocks"))),
				issue("ex-k1", deps(dep("ex-k1", "ex-p1", "parent-child"))),
			},
			errors: []string{"cycle [ex-p1,ex-k1]"},
			issues: Counts{Created: 2},
			deps:   Counts{Failed: 1},
		},
		{
			name: "parent cycle",
			lines: []string{
				issue("ex-a1", deps(dep("ex-a1", "ex-b1", "parent-child"))),
				issue("ex-b1", deps(dep("ex-b1", "ex-a1", "parent-child"))),
			},
			errors: []string{"cycle [ex-a1,ex-b1]"},
			issues: Counts{Created: 2},
		},
		{
			name:   "dangling parent",
			lines:  []string{issue("ex-a1", deps(dep("ex-a1", "ex-gone", "parent-child")))},
			errors: []string{"dangling [ex-a1,ex-gone]"},
			issues: Counts{Created: 1},
		},
		{
			name:   "self dependency",
			lines:  []string{issue("ex-a1", deps(dep("ex-a1", "ex-a1", "related")))},
			errors: []string{"cycle [ex-a1]"},
			issues: Counts{Created: 1},
			deps:   Counts{Failed: 1},
		},
		{
			name:   "bad metadata",
			lines:  []string{issue("ex-a1", `,"metadata":"{not json"`)},
			errors: []string{"invalid [ex-a1]"},
			issues: Counts{Failed: 1},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			rep := importBytes(t, s, []byte(strings.Join(tc.lines, "\n")+"\n"), false)
			var got []string
			for _, p := range rep.Problems {
				if p.Level != LevelError {
					continue
				}
				got = append(got, p.Kind+" ["+strings.Join(p.IDs, ",")+"]")
				if p.Fix == "" || !strings.Contains(p.Text(), "\nfix: ") {
					t.Errorf("error without a fix: %+v", p)
				}
			}
			sort.Strings(got)
			if !slices.Equal(got, tc.errors) {
				t.Errorf("errors = %v, want %v\n%s", got, tc.errors, mustJSON(t, rep.Problems))
			}
			if rep.Issues != tc.issues || rep.Deps != tc.deps {
				t.Errorf("issues %+v deps %+v, want %+v %+v", rep.Issues, rep.Deps, tc.issues, tc.deps)
			}
			// A dry run on a fresh store finds the same problems.
			dry := importBytes(t, newStore(t), []byte(strings.Join(tc.lines, "\n")), true)
			if a, b := problemKeys(dry, LevelError), problemKeys(rep, LevelError); !slices.Equal(a, b) {
				t.Errorf("dry run errors differ:\n%v\n%v", a, b)
			}
		})
	}
}

func TestCommentIDs(t *testing.T) {
	c := bdComment{Author: "sam", Text: "hi", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	cases := []struct {
		name string
		raw  string
		keep bool
	}{
		{"uuid", `"9b2f6c1e-1d4a-5e8b-9c3d-2a1b0f9e8d7c"`, false},
		{"legacy integer", `17`, false},
		{"missing", ``, false},
		{"starfix form", `"abcdefghijklmnop"`, true},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := commentID("ex-a1", json.RawMessage(tc.raw), c)
			if again := commentID("ex-a1", json.RawMessage(tc.raw), c); again != got {
				t.Errorf("not deterministic: %s, %s", got, again)
			}
			if !commentIDShape.MatchString(got) {
				t.Errorf("id %q is not in starfix form", got)
			}
			if tc.keep && got != strings.Trim(tc.raw, `"`) {
				t.Errorf("starfix id changed to %s", got)
			}
			if other := commentID("ex-b1", json.RawMessage(tc.raw), c); !tc.keep && other == got {
				t.Errorf("same id on another issue")
			}
			if seen[got] {
				t.Errorf("id %s collides", got)
			}
			seen[got] = true
		})
	}
}
