package mcpserver

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func memories(n, bodyLen int) []proto.Memory {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	out := make([]proto.Memory, n)
	for i := range out {
		out[i] = proto.Memory{ID: fmt.Sprintf("m%d", i), Scope: "project", Key: fmt.Sprintf("key-%d", i),
			Body: strings.Repeat("b", bodyLen), Author: "bob", UpdatedBy: "bob", CreatedAt: at, UpdatedAt: at, Rev: 1}
	}
	return out
}

// remember sends what the agent gave, with one idempotency key per call,
// and returns {id, rev}; the schema refuses a scope that is none of the
// three before anything is sent.
func TestRemember(t *testing.T) {
	f := &fakeConn{reply: func(string, any) (any, error) { return proto.WriteResult{ID: "m1", Rev: 2}, nil }}
	cs, d := connect(t, f)
	res := callTool(t, cs, "remember", map[string]any{"key": "deploy", "body": "weekday mornings", "scope": "user",
		"tags": []any{"ops"}, "issue": "sf-a1b2", "rev": 1})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	if got := text(t, res); got != `{"id":"m1","rev":2}` {
		t.Errorf("remember = %s, want {id, rev}", got)
	}
	a, ok := f.calls[0].args.(proto.RememberArgs)
	if !ok || f.calls[0].op != proto.OpRemember || a.Key != "deploy" || a.Body != "weekday mornings" || a.Scope != "user" ||
		!slices.Equal(a.Tags, []string{"ops"}) || a.Issue != "sf-a1b2" || a.Rev != 1 || a.Pinned != nil ||
		!strings.HasPrefix(a.Idem, "mcp-") {
		t.Errorf("remember sent %s %+v", f.calls[0].op, f.calls[0].args)
	}
	for _, bad := range []map[string]any{
		{"key": "k", "body": "b", "scope": "world"},
		{"key": "k"},
		{"key": "k", "body": "b", "pinned": true},
		{"key": "k", "body": "b", "author": "bob"},
	} {
		if res := callTool(t, cs, "remember", bad); !res.IsError {
			t.Errorf("remember %v was accepted", bad)
		}
	}
	if d.n != 1 {
		t.Errorf("dialed %d times", d.n)
	}
}

// recall passes its filters through and returns compact memories marked
// as others' text, newest first as the server sent them; long bodies are
// cut and the list held under MaxResultTokens, counting what it left out.
func TestRecall(t *testing.T) {
	var sent proto.RecallArgs
	f := &fakeConn{reply: func(_ string, args any) (any, error) {
		sent = args.(proto.RecallArgs)
		ms := memories(40, 3000)
		ms[0].Pinned, ms[0].Tags, ms[0].Issue = true, []string{"ops"}, "sf-a1b2"
		return proto.RecallResult{Memories: ms, More: 5}, nil
	}}
	cs, _ := connect(t, f)
	res := callTool(t, cs, "recall", map[string]any{"text": "deploy", "tag": "ops", "key": "k", "scope": "team"})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	if sent != (proto.RecallArgs{Text: "deploy", Tag: "ops", Key: "k", Scope: "team"}) {
		t.Errorf("recall sent %+v", sent)
	}
	var got Memories
	if err := json.Unmarshal([]byte(text(t, res)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Untrusted != untrustedNote || len(got.Memories) == 0 || len(got.Memories)+got.More != 45 || !got.Truncated {
		t.Fatalf("recall: %d memories, more %d, truncated %v, untrusted %q", len(got.Memories), got.More, got.Truncated, got.Untrusted)
	}
	m := got.Memories[0]
	if m.Key != "key-0" || m.Scope != "project" || !m.Pinned || !slices.Equal(m.Tags, []string{"ops"}) || m.Issue != "sf-a1b2" ||
		m.Rev != 1 || m.By != "bob" || m.At != "2026-10-09T08:00Z" || len(m.Body) > maxMemoryText+len("…") {
		t.Errorf("first memory %+v", m)
	}
	if n := Tokens([]byte(text(t, res))); n > MaxResultTokens {
		t.Errorf("recall is %d tokens, cap %d", n, MaxResultTokens)
	}
	if res := callTool(t, cs, "recall", map[string]any{"scope": "everyone"}); !res.IsError {
		t.Error("recall with a scope that is none of the three was accepted")
	}
}

// An empty recall is a list, not null, and carries no untrusted note.
func TestRecallNothing(t *testing.T) {
	f := &fakeConn{reply: func(string, any) (any, error) { return proto.RecallResult{}, nil }}
	cs, _ := connect(t, f)
	if got := text(t, callTool(t, cs, "recall", nil)); got != `{"memories":[]}` {
		t.Errorf("empty recall = %s", got)
	}
}

func TestForget(t *testing.T) {
	f := &fakeConn{reply: func(string, any) (any, error) { return proto.WriteResult{ID: "m1", Rev: 3}, nil }}
	cs, _ := connect(t, f)
	res := callTool(t, cs, "forget", map[string]any{"key": "deploy", "scope": "team", "rev": 3})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	a, ok := f.calls[0].args.(proto.ForgetArgs)
	if !ok || a.Key != "deploy" || a.Scope != "team" || a.Rev != 3 || !strings.HasPrefix(a.Idem, "mcp-") {
		t.Errorf("forget sent %s %+v", f.calls[0].op, f.calls[0].args)
	}
}

// prime asks the server to rank memories for the session and shows them
// inside the data fence, pinned ones marked.
func TestPrimeMemories(t *testing.T) {
	var sent proto.RecallArgs
	f := &fakeConn{who: "alice", reply: func(op string, args any) (any, error) {
		if op == proto.OpRecall {
			sent = args.(proto.RecallArgs)
			ms := memories(2, 40)
			ms[0].Pinned = true
			ms[1].Relevant, ms[1].Scope = true, "user"
			return proto.RecallResult{Memories: ms}, nil
		}
		return proto.ListResult{Issues: summaries(1, 20)}, nil
	}}
	p, err := BuildPrime(t.Context(), f, "v0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if !sent.Prime || sent.Limit != primeMemories || sent.Scope != "" || sent.Text != "" {
		t.Errorf("prime's recall sent %+v", sent)
	}
	if len(p.Memories) != 2 || !p.Memories[0].Pinned || !p.Memories[1].Relevant {
		t.Fatalf("prime memories %+v", p.Memories)
	}
	txt := p.Text()
	data := txt[strings.Index(txt, primeDataBegin):]
	want := "memories:\n  key-0 (project, pinned) \"" + strings.Repeat("b", 40) + "\"\n  key-1 (user, relevant) \""
	if !strings.Contains(data, want) {
		t.Errorf("prime text lacks %q inside the data fence:\n%s", want, txt)
	}
	if !strings.Contains(txt, "memories below") {
		t.Errorf("prime's data note does not name memories:\n%s", txt)
	}
}

// fit cuts memory bodies, then drops unpinned memories before anything
// else, and pinned ones only after the ready and inbox lists.
func TestPrimeFitMemories(t *testing.T) {
	pinned := func(n int) []Memory {
		out := make([]Memory, n)
		for i := range out {
			out[i] = Memory{Key: fmt.Sprintf("pin-%d", i), Scope: "project", Body: strings.Repeat("p", 2000), Pinned: true}
		}
		return out
	}
	loose := make([]Memory, 20)
	for i := range loose {
		loose[i] = Memory{Key: fmt.Sprintf("loose-%d", i), Scope: "project", Body: strings.Repeat("l", 2000)}
	}
	p := &Prime{Project: "example", Session: "s", Working: summaries(2, 50), Ready: summaries(5, 50),
		Memories: append(pinned(2), loose...)}
	p.fit()
	if b, _ := json.Marshal(p); Tokens(b) > MaxPrimeTokens || Tokens([]byte(p.Text())) > MaxPrimeTokens {
		t.Fatalf("prime is %d tokens", Tokens(b))
	}
	var kept []string
	for _, m := range p.Memories {
		kept = append(kept, m.Key)
	}
	if len(kept) < 2 || kept[0] != "pin-0" || kept[1] != "pin-1" || len(p.Ready) != 5 || !p.More {
		t.Fatalf("after fit: memories %q, %d ready, more %v; want both pinned kept and every ready issue", kept, len(p.Ready), p.More)
	}
	for _, m := range p.Memories {
		if len(m.Body) > primeMemoryLen+len("…") {
			t.Errorf("%s body is %d bytes", m.Key, len(m.Body))
		}
	}

	// With nothing unpinned to drop, ready goes before pinned memories.
	p = &Prime{Project: "example", Session: "s", Ready: summaries(50, 100), Memories: pinned(3)}
	p.fit()
	if len(p.Memories) != 3 || len(p.Ready) == 50 {
		t.Fatalf("after fit: %d pinned memories, %d ready", len(p.Memories), len(p.Ready))
	}
}

// start returns the memories relevant to the issue, compact, and drops
// them before cutting the issue's own text.
func TestStartMemories(t *testing.T) {
	f := &fakeConn{reply: func(string, any) (any, error) {
		ms := memories(30, 400)
		ms[0].Relevant = true
		return proto.StartResult{Issue: proto.Issue{ID: "sf-a1b2", Rev: 3, Title: "t", Type: "bug", Body: strings.Repeat("x", 3000)},
			Memories: ms}, nil
	}}
	cs, _ := connect(t, f)
	res := callTool(t, cs, "start", map[string]any{"id": "sf-a1b2"})
	if res.IsError {
		t.Fatal(text(t, res))
	}
	var got Started
	if err := json.Unmarshal([]byte(text(t, res)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Memories) == 0 || len(got.Memories) == 30 || got.Memories[0].Key != "key-0" || !got.Memories[0].Relevant {
		t.Fatalf("start memories: %d, first %+v", len(got.Memories), got.Memories[0])
	}
	if got.Body != strings.Repeat("x", 3000) || got.Truncated {
		t.Errorf("start cut the issue's body (%d bytes) before dropping memories", len(got.Body))
	}
	if n := Tokens([]byte(text(t, res))); n > MaxResultTokens {
		t.Errorf("start is %d tokens", n)
	}
}

// Memory refusals name the agent's next step in tools.
func TestMemoryErrorsNameTheNextStep(t *testing.T) {
	tests := []struct {
		name string
		err  *proto.Error
		want string
	}{
		{"exists", proto.Errf(proto.CodeConflict, "recall it with `sfx recall --key k --scope project`, merge, and retry with --rev 2",
			"memory k exists in project scope at rev 2, written by bob"),
			"call recall with the key and scope for its body and rev, merge your change into it, then remember again with that rev"},
		{"missing", proto.Errf(proto.CodeNotFound, "find the key with `sfx memories --scope team`; remember it with no rev to create it",
			"memory k not found in team scope"),
			"call recall to find the key and its scope; remember without rev creates it"},
		{"secret", proto.Errf(proto.CodeInvalid, "remove the secret and remember the rest", "the memory's body looks like it holds a secret (JSON web token)"),
			"never store a secret: remember the rest, naming where the secret is kept (a vault path or an environment variable) instead"},
		{"cap", proto.Errf(proto.CodeInvalid, "forget memories no longer needed with `sfx forget KEY --scope project`",
			"alice may hold at most 1000 memories in project scope"),
			"call forget on memories that no longer hold, or tell the user, quoting the server: \"forget memories no longer needed with `sfx forget KEY --scope project`\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := explain(tc.err).Fix; got != tc.want {
				t.Errorf("fix = %q\nwant  %q", got, tc.want)
			}
		})
	}
}
