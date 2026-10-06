package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/dolttest"
)

var (
	dolt    *dolttest.Server
	doltErr error
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "starfixd-admin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	dolt, doltErr = dolttest.Start(ctx, dir)
	cancel()
	if dolt != nil {
		defer func() { _ = dolt.Stop() }()
	}
	return m.Run()
}

// newDSN returns an empty database's DSN in $STARFIXD_DSN form.
func newDSN(t *testing.T) string {
	t.Helper()
	if errors.Is(doltErr, dolttest.ErrNoDolt) {
		t.Skip("dolt is not on PATH: install dolt to run the admin command tests")
	}
	if doltErr != nil {
		t.Fatalf("dolt sql-server: %v", doltErr)
	}
	dsn, err := dolt.NewDatabase(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

type result struct {
	stdout, stderr string
	err            error
}

// admin runs an admin command in process against dsn, with no config file.
func admin(t *testing.T, dsn string, stdin string, cmd func(context.Context, adminEnv, []string) error, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	cfg := filepath.Join(t.TempDir(), "starfixd.yaml")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := adminEnv{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errb, getenv: func(k string) string {
		if k == "STARFIXD_DSN" {
			return dsn
		}
		return ""
	}}
	err := cmd(t.Context(), env, append([]string{"--config", cfg}, args...))
	return result{out.String(), errb.String(), err}
}

const backlog = "../../internal/bdimport/testdata/backlog.jsonl"

func TestImportBDCommand(t *testing.T) {
	dsn := newDSN(t)

	dry := admin(t, dsn, "", importBD, "--dry-run", backlog)
	if dry.err != nil || !strings.HasPrefix(dry.stdout, "would import 10 issues (10 created)") || strings.Count(dry.stdout, "\n") != 1 {
		t.Fatalf("dry run: %+v", dry)
	}

	got := admin(t, dsn, "", importBD, backlog)
	if got.err != nil {
		t.Fatalf("import: %v\n%s", got.err, got.stderr)
	}
	if want := "imported 10 issues (10 created), 8 deps (8 created), 3 comments (3 created); 0 errors, 17 warnings\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
	if n, f := strings.Count(got.stderr, "warning: "), strings.Count(got.stderr, "\nfix: "); n != 17 || f != 17 {
		t.Errorf("stderr has %d warnings and %d fix lines:\n%s", n, f, got.stderr)
	}

	again := admin(t, dsn, "", importBD, "--json", backlog)
	if again.err != nil || again.stderr != "" {
		t.Fatalf("json import: %+v", again)
	}
	var doc struct {
		Summary string `json:"summary"`
		Issues  struct {
			Unchanged int `json:"unchanged"`
		} `json:"issues"`
		Problems []struct {
			Level, Kind, Fix string
			IDs              []string
		} `json:"problems"`
	}
	dec := json.NewDecoder(strings.NewReader(again.stdout))
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, again.stdout)
	}
	if dec.More() {
		t.Error("more than one JSON document")
	}
	if doc.Issues.Unchanged != 10 || len(doc.Problems) != 17 || !strings.HasPrefix(doc.Summary, "imported 10 issues (10 unchanged)") {
		t.Errorf("json report = %+v", doc)
	}
}

func TestImportBDFailures(t *testing.T) {
	dsn := newDSN(t)
	cases := []struct {
		name   string
		stdin  string
		args   []string
		code   bool // exits 1 after printing the report
		stderr string
		usage  bool
	}{
		{name: "no file", args: nil, usage: true},
		{name: "two files", args: []string{"a", "b"}, usage: true},
		{name: "unknown flag", args: []string{"--colour", "x"}, usage: true},
		{name: "missing file", args: []string{"/nonexistent/bd.jsonl"}, stderr: "fix: give the path of bd's export"},
		{name: "bad line from stdin", args: []string{"-"}, stdin: "{oops\n", code: true,
			stderr: "error: line 1 is not a JSON object"},
		{name: "dangling dependency", args: []string{"-"}, code: true,
			stdin:  `{"id":"ex-a1","title":"A","created_at":"2026-02-01T00:00:00Z","dependencies":[{"depends_on_id":"ex-zz","type":"blocks"}]}`,
			stderr: "error: ex-a1 → ex-zz (blocks): ex-zz is not in the file or the store; skipped (ex-a1, ex-zz)\nfix: import ex-zz too, or remove the dependency in bd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := admin(t, dsn, tc.stdin, importBD, tc.args...)
			var ue usageError
			switch {
			case tc.usage:
				if !errors.As(r.err, &ue) {
					t.Errorf("err = %v, want a usage error", r.err)
				}
			case tc.code:
				if code, ok := r.err.(exitError); !ok || code != 1 { //nolint:errorlint // the bare value is the contract
					t.Errorf("err = %v, want exit 1", r.err)
				}
				if !strings.Contains(r.stderr, tc.stderr) {
					t.Errorf("stderr = %q, want %q", r.stderr, tc.stderr)
				}
				if strings.Count(r.stdout, "\n") != 1 {
					t.Errorf("stdout is not one summary line: %q", r.stdout)
				}
			default:
				if r.err == nil || !strings.Contains(r.err.Error(), tc.stderr) {
					t.Errorf("err = %v, want %q", r.err, tc.stderr)
				}
			}
		})
	}

	none := admin(t, "", "", importBD, backlog)
	if none.err == nil || !strings.Contains(none.err.Error(), "fix: set $STARFIXD_DSN") {
		t.Errorf("no DSN: %v", none.err)
	}
}

func TestExportBDCommand(t *testing.T) {
	src := newDSN(t)
	if r := admin(t, src, "", importBD, backlog); r.err != nil {
		t.Fatal(r.err)
	}
	stdout := admin(t, src, "", exportBD)
	if stdout.err != nil || strings.Count(stdout.stdout, "\n") != 10 {
		t.Fatalf("export to stdout: %+v", stdout)
	}
	file := filepath.Join(t.TempDir(), "out.jsonl")
	r := admin(t, src, "", exportBD, "-o", file)
	if r.err != nil || r.stderr != "exported 10 issues to "+file+"\n" || r.stdout != "" {
		t.Fatalf("export to file: %+v", r)
	}
	b, err := os.ReadFile(file) //nolint:gosec // test temp file
	if err != nil || string(b) != stdout.stdout {
		t.Fatalf("file differs from stdout export: %v", err)
	}

	// The export imports into an empty store with nothing to report but
	// the fields bd has and starfix does not; then it exports the same.
	dst := newDSN(t)
	in := admin(t, dst, "", importBD, file)
	if in.err != nil || !strings.HasPrefix(in.stdout, "imported 10 issues (10 created), 8 deps (8 created), 3 comments (3 created); 0 errors") {
		t.Fatalf("import of the export: %+v", in)
	}
	if again := admin(t, dst, "", exportBD); again.stdout != stdout.stdout {
		t.Errorf("round trip changed the export:\n%s\n%s", stdout.stdout, again.stdout)
	}

	if r := admin(t, src, "", exportBD, "extra"); r.err == nil {
		t.Error("export-bd accepted an argument")
	}
}
