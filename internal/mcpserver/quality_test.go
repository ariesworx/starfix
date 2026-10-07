package mcpserver

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

// Every create-type tool sends an idempotency key, the same one on the
// retry after a dropped connection; keys differ between calls.
func TestWritesRetryWithTheirKey(t *testing.T) {
	key := func(args any) string {
		switch a := args.(type) {
		case proto.CreateArgs:
			return a.Idem
		case proto.CommentArgs:
			return a.Idem
		case proto.HandoffArgs:
			return a.Idem
		case proto.FinishArgs:
			return a.Idem
		}
		return ""
	}
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"create", map[string]any{"title": "x"}},
		{"comment", map[string]any{"id": "sf-1", "body": "hi"}},
		{"handoff", map[string]any{"id": "sf-1", "note": "later"}},
		{"finish", map[string]any{"id": "sf-1"}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			var keys []string
			record := func(fail bool) *fakeConn {
				return &fakeConn{reply: func(_ string, args any) (any, error) {
					keys = append(keys, key(args))
					if fail {
						return nil, errLost
					}
					return proto.WriteResult{ID: "sf-1", Rev: 2}, nil
				}}
			}
			cs, d := connect(t, record(true), record(false))
			if res := callTool(t, cs, tc.tool, tc.args); res.IsError {
				t.Fatalf("%s after a drop: %s", tc.tool, text(t, res))
			}
			if res := callTool(t, cs, tc.tool, tc.args); res.IsError {
				t.Fatalf("%s again: %s", tc.tool, text(t, res))
			}
			if len(keys) != 3 || keys[0] != keys[1] || !strings.HasPrefix(keys[0], "mcp-") || keys[2] == keys[0] || d.n != 2 {
				t.Fatalf("keys %q over %d dials; want the first two equal, the third new", keys, d.n)
			}
		})
	}
}

// finish passes ticked and waived items; a waived key that is not an item
// number is refused before anything is sent.
func TestFinishAcceptance(t *testing.T) {
	f := &fakeConn{reply: func(string, any) (any, error) { return proto.FinishResult{ID: "sf-1", Rev: 3}, nil }}
	cs, d := connect(t, f)
	res := callTool(t, cs, "finish", map[string]any{"id": "sf-1", "ticked": []any{1, 3}, "waived": map[string]any{"2": "covered elsewhere"}})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	got := f.calls[0].args.(proto.FinishArgs)
	if !slices.Equal(got.Ticked, []int{1, 3}) || !maps.Equal(got.Waived, map[int]string{2: "covered elsewhere"}) {
		t.Errorf("finish args %+v", got)
	}
	for _, bad := range []map[string]any{{"two": "x"}, {"0": "x"}, {"-1": "x"}} {
		if res := callTool(t, cs, "finish", map[string]any{"id": "sf-1", "waived": bad}); !res.IsError {
			t.Errorf("waived %v accepted", bad)
		}
	}
	if d.n != 1 || len(f.calls) != 1 {
		t.Errorf("refused calls reached the server: %d dials, %d calls", d.n, len(f.calls))
	}
}

// start and show list acceptance items in place of the prose, one line
// each; show and create name similar closed issues in one line.
func TestItemsAndSimilar(t *testing.T) {
	items := []proto.AcceptanceItem{{N: 1, Text: "survives restart", State: "ticked", By: "alice"},
		{N: 2, Text: "docs", State: "waived", Reason: "none needed"}, {N: 3, Text: "has tests"}}
	similar := []proto.Summary{{ID: "sf-old1", Title: "Login fails", Status: "closed"}, {ID: "sf-old2", Title: "Token expiry", Status: "closed"}}
	f := &fakeConn{reply: func(op string, _ any) (any, error) {
		is := proto.Issue{ID: "sf-1", Title: "Login", Type: "bug", Status: "in_progress", Acceptance: "- [x] survives restart\n- docs\n- has tests"}
		switch op {
		case proto.OpStart:
			return proto.StartResult{Issue: is, Items: items}, nil
		case proto.OpShow:
			return proto.ShowResult{Issue: is, Items: items, Similar: similar}, nil
		}
		return proto.CreateResult{ID: "sf-2", Rev: 1, Similar: similar}, nil
	}}
	cs, _ := connect(t, f)
	want := []string{"1 [x] survives restart", "2 [waived: none needed] docs", "3 [ ] has tests"}

	var st Started
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "start", map[string]any{"id": "sf-1"}))), &st); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(st.Items, want) || st.Acceptance != "" {
		t.Errorf("start items %q, acceptance %q", st.Items, st.Acceptance)
	}
	var sh Issue
	if err := json.Unmarshal([]byte(text(t, callTool(t, cs, "show", map[string]any{"id": "sf-1"}))), &sh); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sh.Items, want) || sh.Acceptance != "" || sh.Similar != "sf-old1 Login fails; sf-old2 Token expiry" {
		t.Errorf("show: items %q, acceptance %q, similar %q", sh.Items, sh.Acceptance, sh.Similar)
	}
	if got := text(t, callTool(t, cs, "create", map[string]any{"title": "Login again"})); got != `{"untrusted":"`+untrustedNote+`","id":"sf-2","rev":1,"similar":"sf-old1 Login fails; sf-old2 Token expiry"}` {
		t.Errorf("create = %s", got)
	}
}

func TestAcceptanceErrorNamesTheArguments(t *testing.T) {
	res := toolError(proto.Errf(proto.CodeAcceptance, "tick what is met with `sfx accept sf-1 2`, or waive an item with `sfx accept sf-1 N --waive REASON`",
		"issue sf-1 has acceptance items neither ticked nor waived: 2"))
	want := "acceptance: issue sf-1 has acceptance items neither ticked nor waived: 2\n" +
		"fix: call finish with ticked: [numbers met] and waived: {number: reason} for the rest; show lists the items"
	if got := text(t, res); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
