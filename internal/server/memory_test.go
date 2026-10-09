package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/store"
)

func memKeys(ms []proto.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Scope + "/" + m.Key
	}
	return out
}

func TestDispatchMemory(t *testing.T) {
	s := newServer(t)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work", Labels: []string{"api"}})
	pin := true
	w := mustCall[proto.WriteResult](t, s, alice, proto.OpRemember, proto.RememberArgs{Key: "deploy", Body: "weekday mornings",
		Tags: []string{"ops"}, Issue: is.ID, Pinned: &pin})
	if w.ID == "" || w.Rev != 1 {
		t.Fatalf("remember = %+v, want an id at rev 1", w)
	}
	mustCall[proto.WriteResult](t, s, alice, proto.OpRemember, proto.RememberArgs{Scope: "user", Key: "style", Body: "tabs"})
	got := mustCall[proto.RecallResult](t, s, bob, proto.OpRecall, proto.RecallArgs{})
	if len(got.Memories) != 1 {
		t.Fatalf("bob recalls %+v, want only the project memory", got.Memories)
	}
	m := got.Memories[0]
	if m.ID != w.ID || m.Scope != "project" || m.Key != "deploy" || m.Body != "weekday mornings" || !slices.Equal(m.Tags, []string{"ops"}) ||
		m.Issue != is.ID || !m.Pinned || m.Author != "alice" || m.Rev != 1 || m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Errorf("recalled %+v", m)
	}
	mine := mustCall[proto.RecallResult](t, s, alice, proto.OpRecall, proto.RecallArgs{Scope: "user"})
	if got := memKeys(mine.Memories); !slices.Equal(got, []string{"user/style"}) {
		t.Errorf("alice recalls her user scope as %q", got)
	}
	r := mustCall[proto.WriteResult](t, s, bob, proto.OpRemember, proto.RememberArgs{Key: "deploy", Body: "any time", Rev: 1})
	if r.ID != w.ID || r.Rev != 2 {
		t.Errorf("replace = %+v, want %s at rev 2", r, w.ID)
	}
	up := mustCall[proto.WriteResult](t, s, bob, proto.OpPin, proto.PinArgs{Key: "deploy", Pinned: false})
	if up.Rev != 3 {
		t.Errorf("unpin = %+v, want rev 3", up)
	}
	f := mustCall[proto.WriteResult](t, s, alice, proto.OpForget, proto.ForgetArgs{Key: "deploy", Rev: 3})
	if f.ID != w.ID || f.Rev != 3 {
		t.Errorf("forget = %+v, want %s at rev 3", f, w.ID)
	}
	if left := mustCall[proto.RecallResult](t, s, bob, proto.OpRecall, proto.RecallArgs{}); len(left.Memories) != 0 {
		t.Errorf("after forget, bob recalls %+v", left.Memories)
	}
}

// Identity is the connection's: no memory argument names an author, an
// owner or a principal.
func TestDispatchMemoryIdentityIsImplicit(t *testing.T) {
	s := newServer(t)
	for _, args := range []string{
		`{"key":"k","body":"b","author":"bob"}`,
		`{"key":"k","body":"b","owner":"bob","scope":"user"}`,
		`{"key":"k","body":"b","principal":"bob"}`,
	} {
		_, perr := s.Dispatch(t.Context(), alice, proto.OpRemember, json.RawMessage(args))
		if perr == nil || perr.Code != proto.CodeInvalid || !strings.Contains(perr.Message, "unknown field") {
			t.Errorf("remember %s = %+v, want an unknown field refusal", args, perr)
		}
	}
}

func TestDispatchMemoryRefusals(t *testing.T) {
	s := newServerWith(t, Limits{Limits: store.Limits{Memories: 1}})
	mustCall[proto.WriteResult](t, s, alice, proto.OpRemember, proto.RememberArgs{Key: "k", Body: "one"})
	mustCall[proto.WriteResult](t, s, alice, proto.OpRemember, proto.RememberArgs{Key: "k", Body: "two", Rev: 1})
	key := "AKIA" + "FAKEFAKEFAKEFAKE"
	tests := []struct {
		name   string
		op     string
		args   any
		code   proto.Code
		msg    string
		phrase string
		fix    string
	}{
		{"exists", proto.OpRemember, proto.RememberArgs{Key: "k", Body: "three"}, proto.CodeConflict,
			"memory k exists in project scope at rev 2", proto.FixRecall, "--rev 2"},
		{"stale", proto.OpRemember, proto.RememberArgs{Key: "k", Body: "three", Rev: 1}, proto.CodeConflict,
			"changed since rev 1 (now rev 2 by alice)", proto.FixRecall, "sfx recall --key k --scope project"},
		{"stale forget", proto.OpForget, proto.ForgetArgs{Key: "k", Rev: 1}, proto.CodeConflict, "changed since rev 1", proto.FixRecall, ""},
		{"missing", proto.OpForget, proto.ForgetArgs{Key: "nope", Scope: "team"}, proto.CodeNotFound,
			"memory nope not found in team scope", proto.FixFindMemory, "sfx memories --scope team"},
		{"missing pin", proto.OpPin, proto.PinArgs{Key: "nope", Pinned: true}, proto.CodeNotFound, "memory nope not found", proto.FixFindMemory, ""},
		{"missing replace", proto.OpRemember, proto.RememberArgs{Key: "nope", Body: "x", Rev: 4}, proto.CodeNotFound, "memory nope not found",
			proto.FixFindMemory, ""},
		{"secret", proto.OpRemember, proto.RememberArgs{Scope: "user", Key: "aws", Body: "key " + key}, proto.CodeInvalid,
			"the memory's body looks like it holds a secret (AWS access key id)", proto.FixSecret, "where it is kept"},
		{"cap", proto.OpRemember, proto.RememberArgs{Key: "k2", Body: "x"}, proto.CodeInvalid,
			"alice may hold at most 1 memories in project scope", proto.FixForgetSome, "memories_per_scope"},
		{"bad scope", proto.OpRecall, proto.RecallArgs{Scope: "world"}, proto.CodeInvalid, "scope", "", "sfx recall -h"},
		{"prime with a filter", proto.OpRecall, proto.RecallArgs{Prime: true, Tag: "x"}, proto.CodeInvalid, "prime", "", ""},
		{"linked issue missing", proto.OpRemember, proto.RememberArgs{Key: "k3", Body: "x", Issue: "sf-zzzzzzzz"}, proto.CodeNotFound,
			"issue sf-zzzzzzzz not found", "", "sfx list"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, perr := call[proto.Empty](t, s, alice, tc.op, tc.args)
			if perr == nil || perr.Code != tc.code || !strings.Contains(perr.Message, tc.msg) || !hasPhrase(perr.Fix, tc.phrase) ||
				!strings.Contains(perr.Fix, tc.fix) {
				t.Fatalf("%s = %+v; want %s naming %q, a fix with %q and %q", tc.op, perr, tc.code, tc.msg, tc.phrase, tc.fix)
			}
			if strings.Contains(perr.Message+perr.Fix, key) {
				t.Errorf("refusal echoes the secret: %+v", perr)
			}
		})
	}
}

// start returns the memories relevant to the issue it takes, and prime's
// recall ranks them for the caller; neither shows another's user scope.
func TestDispatchMemoriesInStartAndPrime(t *testing.T) {
	s := newServer(t)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work", Labels: []string{"api"}})
	other := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "other"})
	for _, a := range []proto.RememberArgs{
		{Key: "linked", Body: "about the work", Issue: is.ID},
		{Key: "tagged", Body: "api notes", Tags: []string{"api"}},
		{Key: "unrelated", Body: "about the other", Issue: other.ID},
		{Scope: "user", Key: "bobs", Body: "bob's api notes", Tags: []string{"api"}},
	} {
		mustCall[proto.WriteResult](t, s, bob, proto.OpRemember, a)
	}
	started := mustCall[proto.StartResult](t, s, alice, proto.OpStart, proto.StartArgs{ID: is.ID})
	if got := memKeys(started.Memories); !slices.Equal(got, []string{"project/tagged", "project/linked"}) {
		t.Errorf("start memories = %q, want the tagged and linked ones, newest first", got)
	}
	primed := mustCall[proto.RecallResult](t, s, alice, proto.OpRecall, proto.RecallArgs{Prime: true, Limit: 10})
	if got, want := memKeys(primed.Memories), []string{"project/tagged", "project/linked", "project/unrelated"}; !slices.Equal(got, want) {
		t.Errorf("prime memories = %q, want %q", got, want)
	}
	for _, m := range primed.Memories {
		if m.Relevant != (m.Key != "unrelated") {
			t.Errorf("%s relevant = %v", m.Key, m.Relevant)
		}
	}
}

// recall writes nothing, so the write rate does not count it.
func TestRecallIsARead(t *testing.T) {
	if !readOps[proto.OpRecall] {
		t.Error("recall is not a read op")
	}
	for _, op := range []string{proto.OpRemember, proto.OpForget, proto.OpPin} {
		if readOps[op] {
			t.Errorf("%s is a read op", op)
		}
	}
}

// A protocol 3 client, as v0.3.0 speaks, still works against this server:
// it knows no memory op, and decodes start's result without its memories.
func TestProtocol3ClientStillServed(t *testing.T) {
	s := newServer(t)
	is := mustCall[proto.WriteResult](t, s, alice, proto.OpCreate, proto.CreateArgs{Title: "work"})
	mustCall[proto.WriteResult](t, s, bob, proto.OpRemember, proto.RememberArgs{Key: "k", Body: "about it", Issue: is.ID})
	w, c := handshakeProto(t, s, alice, 3)
	if w.T != proto.FrameWelcome || w.Err != nil {
		t.Fatalf("a protocol 3 hello = %+v, want a welcome", w)
	}
	id := c.send(proto.OpStart, json.RawMessage(`{"id":"`+string(is.ID)+`","lease":"15m"}`))
	f := c.read()
	if f.T != proto.FrameRes || f.ID != id || f.Err != nil {
		t.Fatalf("a protocol 3 start = %+v", f)
	}
	// What v0.3.0's client decodes: StartResult as it was, without
	// memories; its decoder ignores the field it does not know.
	var old struct {
		Issue proto.Issue  `json:"issue"`
		Claim *proto.Claim `json:"claim"`
	}
	if err := json.Unmarshal(f.OK, &old); err != nil || old.Issue.ID != is.ID || old.Claim == nil {
		t.Errorf("protocol 3 start result %s: %+v, %v", f.OK, old, err)
	}
}
