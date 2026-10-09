package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/secretscan"
)

func mustRemember(t *testing.T, s *Store, a Actor, in NewMemory) Memory {
	t.Helper()
	m, err := s.Remember(t.Context(), a, in)
	if err != nil {
		t.Fatalf("remember %s/%s as %s: %v", in.Scope, in.Key, a.Principal, err)
	}
	return m
}

// keys lists the memories' keys in order, each with its scope, so a
// failure shows the whole ranking.
func keys(ms []Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = string(m.Scope) + "/" + m.Key
	}
	return out
}

func recall(t *testing.T, s *Store, a Actor, q MemoryQuery) []string {
	t.Helper()
	ms, _, err := s.Recall(t.Context(), a, q)
	if err != nil {
		t.Fatalf("recall %+v as %s: %v", q, a.Principal, err)
	}
	return keys(ms)
}

func TestRememberCreatesARecord(t *testing.T) {
	s, clk := clockStore(t)
	is := mustCreate(t, s, NewIssue{Title: "work"})
	m := mustRemember(t, s, alice, NewMemory{Key: "deploy-window", Body: "Deploys go out on weekday mornings.",
		Tags: ptr([]string{"ops", "deploy", "ops"}), Issue: ptr(is.ID)})
	want := Memory{ID: m.ID, Scope: ScopeProject, Key: "deploy-window", Body: "Deploys go out on weekday mornings.",
		Tags: []string{"deploy", "ops"}, Issue: is.ID, Author: "alice", UpdatedBy: "alice",
		CreatedAt: clk.now(), UpdatedAt: clk.now(), Rev: 1}
	if m.ID == "" || !memoryEqual(m, want) {
		t.Fatalf("Remember = %+v\nwant       %+v", m, want)
	}
	got, _, err := s.Recall(t.Context(), bob, MemoryQuery{Key: "deploy-window"})
	if err != nil || len(got) != 1 || !memoryEqual(got[0], want) {
		t.Fatalf("Recall by key = %+v, %v; want %+v", got, err, want)
	}
}

func memoryEqual(a, b Memory) bool {
	return a.ID == b.ID && a.Scope == b.Scope && a.Key == b.Key && a.Body == b.Body && slices.Equal(a.Tags, b.Tags) &&
		a.Issue == b.Issue && a.Pinned == b.Pinned && a.Author == b.Author && a.UpdatedBy == b.UpdatedBy &&
		a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt) && a.Rev == b.Rev
}

// A key names one record per scope, and per user in user scope: the same
// key in each is a different record, and none conflicts with another.
func TestMemoryKeysAreScoped(t *testing.T) {
	s := newStore(t)
	for _, c := range []struct {
		a     Actor
		scope Scope
	}{{alice, ScopeTeam}, {alice, ScopeProject}, {alice, ScopeUser}, {bob, ScopeUser}} {
		m := mustRemember(t, s, c.a, NewMemory{Scope: c.scope, Key: "style", Body: c.a.Principal + " " + string(c.scope)})
		if m.Rev != 1 {
			t.Errorf("Remember(%s, %s) rev = %d, want 1: a new record", c.a.Principal, c.scope, m.Rev)
		}
	}
	bodies := func(a Actor) []string {
		ms, _, err := s.Recall(t.Context(), a, MemoryQuery{Key: "style"})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range ms {
			out = append(out, m.Body)
		}
		slices.Sort(out)
		return out
	}
	if got, want := bodies(alice), []string{"alice project", "alice team", "alice user"}; !slices.Equal(got, want) {
		t.Errorf("alice recalls %q, want %q", got, want)
	}
	if got, want := bodies(bob), []string{"alice project", "alice team", "bob user"}; !slices.Equal(got, want) {
		t.Errorf("bob recalls %q, want %q", got, want)
	}
}

// A user-scope memory is its author's alone: no other principal can
// read, replace, pin or forget it, and to them it does not exist.
func TestUserScopeIsTheAuthorsAlone(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	m := mustRemember(t, s, alice, NewMemory{Scope: ScopeUser, Key: "editor", Body: "alice likes tabs"})
	for _, q := range []MemoryQuery{{}, {Key: "editor"}, {Text: "tabs"}, {Scope: ScopeUser}} {
		if got := recall(t, s, bob, q); len(got) != 0 {
			t.Errorf("bob Recall(%+v) = %q, want nothing", q, got)
		}
	}
	if _, err := s.Remember(ctx, bob, NewMemory{Scope: ScopeUser, Key: "editor", Body: "mine", Rev: m.Rev}); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob replacing alice's user memory: %v, want ErrNotFound (his own scope has no such key)", err)
	}
	if _, err := s.PinMemory(ctx, bob, ScopeUser, "editor", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob pinning alice's user memory: %v, want ErrNotFound", err)
	}
	if _, err := s.Forget(ctx, bob, ScopeUser, "editor", 0, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("bob forgetting alice's user memory: %v, want ErrNotFound", err)
	}
	if got := recall(t, s, alice, MemoryQuery{Scope: ScopeUser}); !slices.Equal(got, []string{"user/editor"}) {
		t.Errorf("alice Recall(user) = %q, want her memory", got)
	}
}

// Replacing a memory is a compare-and-swap on its rev: rev 0 creates and
// is refused when the key exists, a stale rev is refused, and a rev on a
// key that is gone is not found.
func TestRememberCompareAndSwap(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	m := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "one"})

	_, err := s.Remember(ctx, bob, NewMemory{Key: "k", Body: "two"})
	var mc *MemoryConflictError
	if !errors.Is(err, ErrConflict) || !errors.As(err, &mc) || mc.Current != 1 || mc.Rev != 0 || mc.Key != "k" {
		t.Fatalf("Remember of an existing key without rev = %v (%+v), want a MemoryConflictError at rev 1", err, mc)
	}
	m2, err := s.Remember(ctx, bob, NewMemory{Key: "k", Body: "two", Rev: m.Rev})
	if err != nil || m2.Rev != 2 || m2.Body != "two" || m2.Author != "alice" || m2.UpdatedBy != "bob" || m2.ID != m.ID {
		t.Fatalf("Remember at rev 1 = %+v, %v; want rev 2 by bob, author alice, same id", m2, err)
	}
	_, err = s.Remember(ctx, alice, NewMemory{Key: "k", Body: "three", Rev: 1})
	if !errors.As(err, &mc) || mc.Rev != 1 || mc.Current != 2 || mc.By != "bob" {
		t.Fatalf("Remember at a stale rev = %v (%+v), want a conflict naming rev 2 by bob", err, mc)
	}
	if _, err := s.Remember(ctx, alice, NewMemory{Key: "nope", Body: "x", Rev: 3}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remember at a rev of a missing key = %v, want ErrNotFound", err)
	}
	// The same content again changes nothing.
	m3, err := s.Remember(ctx, bob, NewMemory{Key: "k", Body: "two", Rev: 2})
	if err != nil || m3.Rev != 2 {
		t.Errorf("Remember of what is stored = %+v, %v; want rev 2 unchanged", m3, err)
	}
}

// A replace leaves what it omits as it is, as it does the pin: tags and
// the issue link change only when it sets them.
func TestRememberReplaceKeepsOmitted(t *testing.T) {
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "work"})
	m := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "one", Tags: ptr([]string{"ops"}), Issue: ptr(is.ID), Pinned: ptr(true)})
	got := mustRemember(t, s, bob, NewMemory{Key: "k", Body: "two", Rev: m.Rev})
	if got.Body != "two" || !slices.Equal(got.Tags, []string{"ops"}) || got.Issue != is.ID || !got.Pinned {
		t.Errorf("replace of the body alone = %+v, want body two with tags [ops], issue %s and the pin kept", got, is.ID)
	}
	got = mustRemember(t, s, bob, NewMemory{Key: "k", Body: "two", Tags: ptr([]string{"db"}), Rev: got.Rev})
	if !slices.Equal(got.Tags, []string{"db"}) || got.Issue != is.ID || got.Rev != m.Rev+2 {
		t.Errorf("replace of the tags = %+v, want tags [db], issue %s, rev %d", got, is.ID, m.Rev+2)
	}
	got = mustRemember(t, s, bob, NewMemory{Key: "k", Body: "two", Tags: ptr([]string{}), Issue: ptr(IssueID("")), Rev: got.Rev})
	if len(got.Tags) != 0 || got.Issue != "" || !got.Pinned {
		t.Errorf("replace with no tags and no issue = %+v, want both cleared and the pin kept", got)
	}
	stored, _, err := s.Recall(t.Context(), alice, MemoryQuery{Key: "k"})
	if err != nil || len(stored) != 1 || !memoryEqual(stored[0], got) {
		t.Errorf("Recall = %+v, %v; want %+v", stored, err, got)
	}
}

// Concurrent edits of one key from the same rev, in one store or two
// overlapping transactions, have exactly one winner.
func TestRememberConcurrentEditsConflict(t *testing.T) {
	t.Run("one store", func(t *testing.T) {
		s := newStore(t)
		m := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "start"})
		const n = 10
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				_, errs[i] = s.Remember(t.Context(), alice, NewMemory{Key: "k", Body: fmt.Sprintf("writer %d", i), Rev: m.Rev})
			})
		}
		wg.Wait()
		assertOneWinner(t, errs)
	})
	t.Run("overlapping transactions", func(t *testing.T) {
		dsn := newDSN(t)
		s1, s2 := openStore(t, dsn, Options{}), openStore(t, dsn, Options{})
		m := mustRemember(t, s1, alice, NewMemory{Key: "k", Body: "start"})
		b := newBarrier(2)
		s1.beforeCommit, s2.beforeCommit = b.hook, b.hook
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Go(func() { _, errs[0] = s1.Remember(t.Context(), alice, NewMemory{Key: "k", Body: "s1", Rev: m.Rev}) })
		wg.Go(func() { _, errs[1] = s2.Remember(t.Context(), bob, NewMemory{Key: "k", Body: "s2", Rev: m.Rev}) })
		wg.Wait()
		assertOneWinner(t, errs)
		assertGapless(t, s1)
	})
	t.Run("two creates of one new key", func(t *testing.T) {
		dsn := newDSN(t)
		s1, s2 := openStore(t, dsn, Options{}), openStore(t, dsn, Options{})
		b := newBarrier(2)
		s1.beforeCommit, s2.beforeCommit = b.hook, b.hook
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Go(func() { _, errs[0] = s1.Remember(t.Context(), alice, NewMemory{Key: "new", Body: "s1"}) })
		wg.Go(func() { _, errs[1] = s2.Remember(t.Context(), bob, NewMemory{Key: "new", Body: "s2"}) })
		wg.Wait()
		assertOneWinner(t, errs)
	})
}

// New keys never conflict, however they overlap: each round, two stores'
// transactions (one writer each) insert different keys, one with tags,
// and both commit.
func TestRememberNewKeysMerge(t *testing.T) {
	dsn := newDSN(t)
	s1, s2 := openStore(t, dsn, Options{}), openStore(t, dsn, Options{})
	const rounds = 4
	for i := range rounds {
		b := newBarrier(2)
		s1.beforeCommit, s2.beforeCommit = b.hook, b.hook
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Go(func() { _, errs[0] = s1.Remember(t.Context(), alice, NewMemory{Key: fmt.Sprintf("a%d", i), Body: "x"}) })
		wg.Go(func() {
			_, errs[1] = s2.Remember(t.Context(), bob, NewMemory{Key: fmt.Sprintf("b%d", i), Body: "y", Tags: ptr([]string{"t"})})
		})
		wg.Wait()
		for j, err := range errs {
			if err != nil {
				t.Errorf("round %d, store %d, a new key: %v, want success", i, j+1, err)
			}
		}
	}
	if got := recall(t, s1, alice, MemoryQuery{Limit: 100}); len(got) != 2*rounds {
		t.Errorf("recall after concurrent new keys = %q, want %d", got, 2*rounds)
	}
	assertGapless(t, s1)
}

// Recall searches by key, tag or text, in the scopes the caller can see,
// pinned first and then newest first.
func TestRecallSearchAndOrder(t *testing.T) {
	s, clk := clockStore(t)
	ctx := t.Context()
	rem := func(a Actor, scope Scope, key, body string, tags ...string) Memory {
		clk.add(time.Minute)
		return mustRemember(t, s, a, NewMemory{Scope: scope, Key: key, Body: body, Tags: ptr(tags)})
	}
	rem(alice, ScopeProject, "a-oldest", "Builds need Go 1.27", "build")
	rem(bob, ScopeTeam, "b-pinned-later", "Reviews within a day", "process")
	rem(alice, ScopeUser, "c-mine", "I prefer table tests", "style", "go")
	rem(bob, ScopeUser, "d-bobs", "bob prefers tabs", "style")
	rem(alice, ScopeProject, "e-newest", "The DOLT pin lives in internal/version", "build", "dolt")
	clk.add(time.Minute)
	if _, err := s.PinMemory(ctx, alice, ScopeTeam, "b-pinned-later", true); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		q    MemoryQuery
		want []string
	}{
		{"all, pinned then newest", MemoryQuery{}, []string{"team/b-pinned-later", "project/e-newest", "user/c-mine", "project/a-oldest"}},
		{"by key", MemoryQuery{Key: "c-mine"}, []string{"user/c-mine"}},
		{"by tag", MemoryQuery{Tag: "build"}, []string{"project/e-newest", "project/a-oldest"}},
		{"by text in a body, any case", MemoryQuery{Text: "dolt"}, []string{"project/e-newest"}},
		{"by text in a key", MemoryQuery{Text: "OLDEST"}, []string{"project/a-oldest"}},
		{"text is literal, not a pattern", MemoryQuery{Text: "%"}, nil},
		{"by scope", MemoryQuery{Scope: ScopeProject}, []string{"project/e-newest", "project/a-oldest"}},
		{"tag and text", MemoryQuery{Tag: "style", Text: "table"}, []string{"user/c-mine"}},
		{"limit", MemoryQuery{Limit: 2}, []string{"team/b-pinned-later", "project/e-newest"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := recall(t, s, alice, tc.q); !slices.Equal(got, tc.want) {
				t.Errorf("Recall(%+v) = %q, want %q", tc.q, got, tc.want)
			}
		})
	}
	_, more, err := s.Recall(ctx, alice, MemoryQuery{Limit: 1})
	if err != nil || more != 3 {
		t.Errorf("Recall(limit 1) more = %d, %v; want 3", more, err)
	}
	if _, _, err := s.Recall(ctx, alice, MemoryQuery{Scope: "everyone"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Recall(scope everyone) = %v, want ErrInvalid", err)
	}
}

func TestForgetMemory(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	m := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "one", Tags: ptr([]string{"t"})})
	m = mustRemember(t, s, bob, NewMemory{Key: "k", Body: "two", Rev: m.Rev})
	var mc *MemoryConflictError
	if _, err := s.Forget(ctx, alice, ScopeProject, "k", 1, ""); !errors.As(err, &mc) || mc.Current != 2 {
		t.Fatalf("Forget at a stale rev = %v, want a conflict at rev 2", err)
	}
	got, err := s.Forget(ctx, alice, ScopeProject, "k", m.Rev, "")
	if err != nil || got.ID != m.ID || got.Rev != m.Rev+1 || got.Body != "two" {
		t.Fatalf("Forget = %+v, %v; want the forgotten memory, at rev %d", got, err, m.Rev+1)
	}
	if left := recall(t, s, alice, MemoryQuery{}); len(left) != 0 {
		t.Errorf("after forget, recall = %q", left)
	}
	if left := recall(t, s, alice, MemoryQuery{Tag: "t"}); len(left) != 0 {
		t.Errorf("after forget, its tags still match: %q", left)
	}
	if _, err := s.Forget(ctx, alice, ScopeProject, "k", 0, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("Forget of a forgotten key = %v, want ErrNotFound", err)
	}
	// A forgotten key is new again.
	if again := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "fresh"}); again.Body != "fresh" {
		t.Errorf("Remember after forget = %+v", again)
	}
}

// A memory's revisions only rise, across a forget too: a key remembered
// again continues from the forgotten one's revision, so a rev read before
// the forget can never match the new memory and replace or forget it.
func TestForgetKeepsRevsMonotonic(t *testing.T) {
	s := openStore(t, newDSN(t), Options{Limits: Limits{Memories: 1}})
	ctx := t.Context()
	m := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "one", Pinned: new(true)})
	old := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "two", Rev: m.Rev}) // rev 2, read by a slow client
	gone, err := s.Forget(ctx, bob, ScopeProject, "k", old.Rev, "")
	if err != nil || gone.Rev != 3 {
		t.Fatalf("Forget = %+v, %v; want rev 3", gone, err)
	}
	if _, err := s.Remember(ctx, alice, NewMemory{Key: "k", Body: "stale", Rev: old.Rev}); !errors.Is(err, ErrNotFound) {
		t.Errorf("Remember at a pre-forget rev while forgotten = %v, want ErrNotFound", err)
	}
	for _, op := range []func() error{
		func() error { _, err := s.PinMemory(ctx, alice, ScopeProject, "k", true); return err },
		func() error { _, err := s.Forget(ctx, alice, ScopeProject, "k", 0, ""); return err },
	} {
		if err := op(); !errors.Is(err, ErrNotFound) {
			t.Errorf("pin or forget of a forgotten key = %v, want ErrNotFound", err)
		}
	}
	// The per-scope cap of 1 counts no forgotten memory.
	again := mustRemember(t, s, bob, NewMemory{Key: "k", Body: "fresh"})
	if again.Rev != 4 || again.Author != "bob" || again.Pinned || again.Body != "fresh" {
		t.Fatalf("Remember after forget = %+v; want a new memory by bob, unpinned, at rev 4", again)
	}
	var mc *MemoryConflictError
	for _, rev := range []Rev{1, old.Rev, gone.Rev} {
		if _, err := s.Remember(ctx, alice, NewMemory{Key: "k", Body: "stale", Rev: rev}); !errors.As(err, &mc) || mc.Current != again.Rev {
			t.Errorf("Remember at pre-forget rev %d = %v, want a conflict at rev %d", rev, err, again.Rev)
		}
		if _, err := s.Forget(ctx, alice, ScopeProject, "k", rev, ""); !errors.As(err, &mc) {
			t.Errorf("Forget at pre-forget rev %d = %v, want a conflict", rev, err)
		}
	}
	if got := recall(t, s, alice, MemoryQuery{}); !slices.Equal(got, []string{"project/k"}) {
		t.Errorf("recall = %q, want the one live memory", got)
	}
	// An import onto a forgotten key creates it, continuing its revisions.
	if _, err := s.Forget(ctx, bob, ScopeProject, "k", 0, ""); err != nil {
		t.Fatal(err)
	}
	imp := Actor{Principal: "import", Session: "bd", Machine: "m"}
	if plan, err := s.PlanImportMemory(ctx, NewMemory{Key: "k", Body: "from bd"}); err != nil || plan != ImportCreated {
		t.Errorf("PlanImportMemory onto a forgotten key = %q, %v; want created", plan, err)
	}
	if out, err := s.ImportMemory(ctx, imp, NewMemory{Key: "k", Body: "from bd"}); err != nil || out != ImportCreated {
		t.Fatalf("ImportMemory onto a forgotten key = %q, %v; want created", out, err)
	}
	ms, _, err := s.Recall(ctx, alice, MemoryQuery{Key: "k"})
	if err != nil || len(ms) != 1 || ms[0].Rev != 6 || ms[0].Body != "from bd" {
		t.Errorf("imported memory = %+v, %v; want it at rev 6", ms, err)
	}
}

func TestPinMemory(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	m := mustRemember(t, s, alice, NewMemory{Key: "k", Body: "one"})
	seq := lastSeq(t, s)
	p, err := s.PinMemory(ctx, bob, ScopeProject, "k", true)
	if err != nil || !p.Pinned || p.Rev != m.Rev+1 || !p.UpdatedAt.Equal(m.UpdatedAt) {
		t.Fatalf("PinMemory = %+v, %v; want pinned at rev %d, its updated time kept", p, err, m.Rev+1)
	}
	if again, err := s.PinMemory(ctx, bob, ScopeProject, "k", true); err != nil || again.Rev != p.Rev || lastSeq(t, s) != seq+1 {
		t.Errorf("pinning a pinned memory = %+v, %v, %d events; want no change", again, err, lastSeq(t, s)-seq)
	}
	if u, err := s.PinMemory(ctx, alice, ScopeProject, "k", false); err != nil || u.Pinned || u.Rev != p.Rev+1 {
		t.Errorf("unpin = %+v, %v", u, err)
	}
	if _, err := s.PinMemory(ctx, alice, ScopeTeam, "k", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("pin in a scope without the key = %v, want ErrNotFound", err)
	}
	// Remember can pin as it creates, and leaves the pin alone otherwise.
	n := mustRemember(t, s, alice, NewMemory{Key: "n", Body: "x", Pinned: ptr(true)})
	n = mustRemember(t, s, alice, NewMemory{Key: "n", Body: "y", Rev: n.Rev})
	if !n.Pinned {
		t.Errorf("a replace without Pinned unpinned the memory: %+v", n)
	}
}

// Every mutation appends an event whose target is the memory, in a form
// no issue id can take, so the live board never takes it for an issue
// event. A user-scope memory's event holds neither its key nor its body.
func TestMemoryEvents(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	seq := lastSeq(t, s)
	shared := mustRemember(t, s, alice, NewMemory{Key: "shared-key", Body: "shared body", Tags: ptr([]string{"t"})})
	shared = mustRemember(t, s, bob, NewMemory{Key: "shared-key", Body: "shared edit", Rev: shared.Rev})
	if _, err := s.PinMemory(ctx, alice, ScopeProject, "shared-key", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PinMemory(ctx, alice, ScopeProject, "shared-key", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ctx, alice, ScopeProject, "shared-key", 0, ""); err != nil {
		t.Fatal(err)
	}
	private := mustRemember(t, s, alice, NewMemory{Scope: ScopeUser, Key: "private-key", Body: "private body", Tags: ptr([]string{"secretive"})})
	private = mustRemember(t, s, alice, NewMemory{Scope: ScopeUser, Key: "private-key", Body: "private edit", Rev: private.Rev})
	if _, err := s.PinMemory(ctx, alice, ScopeUser, "private-key", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Forget(ctx, alice, ScopeUser, "private-key", 0, ""); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(ctx, seq, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ops []Op
	for _, e := range evs {
		ops = append(ops, e.Op)
		if IssueID(e.Target).Validate() == nil || !strings.HasPrefix(e.Target, MemoryTargetPrefix) {
			t.Errorf("event %s target %q: want %q and an id, never issue-shaped", e.Op, e.Target, MemoryTargetPrefix)
		}
	}
	want := []Op{OpMemoryCreate, OpMemoryUpdate, OpMemoryPin, OpMemoryUnpin, OpMemoryForget,
		OpMemoryCreate, OpMemoryUpdate, OpMemoryPin, OpMemoryForget}
	if !slices.Equal(ops, want) {
		t.Fatalf("memory events = %v, want %v", ops, want)
	}
	if evs[0].Target != MemoryTarget(shared.ID) || evs[5].Target != MemoryTarget(private.ID) {
		t.Errorf("targets %q, %q; want each memory's own", evs[0].Target, evs[5].Target)
	}
	if !strings.Contains(string(evs[1].After), "shared edit") || !strings.Contains(string(evs[1].Before), "shared body") {
		t.Errorf("a shared memory's update event %s → %s, want both bodies", evs[1].Before, evs[1].After)
	}
	for _, e := range evs[5:] {
		for _, leak := range []string{"private-key", "private body", "private edit", "secretive"} {
			if strings.Contains(string(e.Before)+string(e.After), leak) {
				t.Errorf("user-scope event %s holds %q: %s → %s", e.Op, leak, e.Before, e.After)
			}
		}
		var st map[string]any
		if err := json.Unmarshal(firstNonNull(e.After, e.Before), &st); err != nil || st["scope"] != "user" {
			t.Errorf("user-scope event %s state %s, want its scope", e.Op, firstNonNull(e.After, e.Before))
		}
	}
}

// A keyed write of a user memory stores no more in the event log than
// its events do: no column of any event holds its key, body or tags,
// and a repeat still returns the first result's id and rev.
func TestUserMemoryIdemResult(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	seq := lastSeq(t, s)
	in := NewMemory{Scope: ScopeUser, Key: "private-key", Body: "private body", Tags: ptr([]string{"secretive"}), IdempotencyKey: "u-1"}
	m := mustRemember(t, s, alice, in)
	edit := NewMemory{Scope: ScopeUser, Key: "private-key", Body: "private edit", Rev: m.Rev, IdempotencyKey: "u-2"}
	m2 := mustRemember(t, s, alice, edit)
	gone, err := s.Forget(ctx, alice, ScopeUser, "private-key", 0, "u-3")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		do   func() (Memory, error)
		want Memory
	}{
		{"create", func() (Memory, error) { return s.Remember(ctx, alice, in) }, m},
		{"replace", func() (Memory, error) { return s.Remember(ctx, alice, edit) }, m2},
		{"forget", func() (Memory, error) { return s.Forget(ctx, alice, ScopeUser, "private-key", 0, "u-3") }, gone},
	} {
		got, err := tc.do()
		if err != nil || got.ID != tc.want.ID || got.Rev != tc.want.Rev || got.Scope != ScopeUser || got.Key != "private-key" {
			t.Errorf("repeated %s = %+v, %v; want id %s, rev %d, scope and key", tc.name, got, err, tc.want.ID, tc.want.Rev)
		}
	}
	rows, err := s.r.QueryContext(ctx, `SELECT * FROM events WHERE seq > ?`, seq)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		n++
		vals := make([]sql.RawBytes, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		for i, v := range vals {
			for _, leak := range []string{"private-key", "private body", "private edit", "secretive"} {
				if strings.Contains(string(v), leak) {
					t.Errorf("event column %s holds %q: %s", cols[i], leak, v)
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("%d events after a keyed create, replace and forget, want 3", n)
	}
}

func firstNonNull(a, b json.RawMessage) json.RawMessage {
	if len(a) > 0 {
		return a
	}
	return b
}

// Memory events are not issue events: an event watch is offered none.
func TestMemoryEventsAreNotPushed(t *testing.T) {
	s := newStore(t)
	we := s.WatchEvents("bob", "sess-b")
	defer we.Close()
	mustRemember(t, s, alice, NewMemory{Key: "k", Body: "b"})
	mustRemember(t, s, alice, NewMemory{Scope: ScopeUser, Key: "k", Body: "b"})
	if _, err := s.Forget(t.Context(), alice, ScopeProject, "k", 0, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-we.Ready():
		_, evs, _ := we.Take()
		t.Errorf("an event watch was offered memory events %+v", evs)
	default:
	}
}

func TestRememberIdempotent(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	in := NewMemory{Key: "k", Body: "b", IdempotencyKey: "mcp-1"}
	first := mustRemember(t, s, alice, in)
	seq := lastSeq(t, s)
	again, err := s.Remember(ctx, alice, in)
	if want := (Memory{ID: first.ID, Scope: ScopeProject, Key: "k", Rev: first.Rev}); err != nil || !memoryEqual(again, want) || lastSeq(t, s) != seq {
		t.Fatalf("a repeated remember = %+v, %v, %d new events; want %+v and nothing written", again, err, lastSeq(t, s)-seq, want)
	}
	var ie *IdemError
	if _, err := s.Remember(ctx, alice, NewMemory{Key: "k2", Body: "b", IdempotencyKey: "mcp-1"}); !errors.As(err, &ie) {
		t.Errorf("the key reused for another remember = %v, want an IdemError", err)
	}
	// A keyed replace that changes nothing writes nothing, and succeeds.
	same := NewMemory{Key: "k", Body: "b", Rev: first.Rev, IdempotencyKey: "mcp-3"}
	for range 2 {
		got, err := s.Remember(ctx, alice, same)
		if err != nil || got.Rev != first.Rev || lastSeq(t, s) != seq {
			t.Fatalf("Remember(%+v) of what is stored = rev %d, %v, %d new events; want rev %d and nothing written",
				same, got.Rev, err, lastSeq(t, s)-seq, first.Rev)
		}
	}
	gone, err := s.Forget(ctx, alice, ScopeProject, "k", 0, "mcp-2")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := s.Forget(ctx, alice, ScopeProject, "k", 0, "mcp-2"); err != nil || again.ID != gone.ID {
		t.Errorf("a repeated forget = %+v, %v; want the first result", again, err)
	}
}

// The secrets lint refuses anything that looks like a credential, in
// every scope and every field, naming what it saw and never the value.
func TestRememberRefusesSecrets(t *testing.T) {
	s := newStore(t)
	key := "AKIA" + "FAKEFAKEFAKEFAKE"
	for _, scope := range []Scope{ScopeTeam, ScopeProject, ScopeUser} {
		for _, tc := range []struct {
			field string
			in    NewMemory
			kind  secretscan.Kind
		}{
			{"body", NewMemory{Key: "aws", Body: "the deploy key is " + key}, secretscan.KindAWSAccessKey},
			{"key", NewMemory{Key: key, Body: "x"}, secretscan.KindAWSAccessKey},
			{"tag", NewMemory{Key: "aws", Body: "x", Tags: ptr([]string{key})}, secretscan.KindAWSAccessKey},
			{"body", NewMemory{Key: "db", Body: "password=" + "hunter22"}, secretscan.KindAssignment},
		} {
			t.Run(string(scope)+" "+tc.field+" "+string(tc.kind), func(t *testing.T) {
				tc.in.Scope = scope
				_, err := s.Remember(t.Context(), alice, tc.in)
				var se *SecretError
				if !errors.Is(err, ErrInvalid) || !errors.As(err, &se) || se.Field != tc.field || se.Kind != tc.kind {
					t.Fatalf("Remember with a %s in its %s = %v (%+v), want a SecretError naming both", tc.kind, tc.field, err, se)
				}
				if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "hunter22") {
					t.Errorf("refusal %q echoes the secret", err)
				}
			})
		}
	}
	if got := recall(t, s, alice, MemoryQuery{}); len(got) != 0 {
		t.Errorf("refused memories were stored: %q", got)
	}
	// A pointer to where a secret lives is fine.
	mustRemember(t, s, alice, NewMemory{Key: "db", Body: "the db password is in the vault at secret/db"})
}

func TestRememberValidates(t *testing.T) {
	s := newStore(t)
	is := mustCreate(t, s, NewIssue{Title: "x"})
	tests := []struct {
		name string
		in   NewMemory
		want error
	}{
		{"no key", NewMemory{Body: "x"}, ErrInvalid},
		{"key with a space", NewMemory{Key: "a b", Body: "x"}, ErrInvalid},
		{"key starting with a dash", NewMemory{Key: "-rf", Body: "x"}, ErrInvalid},
		{"key with a newline", NewMemory{Key: "a\nb", Body: "x"}, ErrInvalid},
		{"no body", NewMemory{Key: "k"}, ErrInvalid},
		{"body with a control character", NewMemory{Key: "k", Body: "a\x1b[2Jb"}, ErrInvalid},
		{"tag with a space", NewMemory{Key: "k", Body: "x", Tags: ptr([]string{"a b"})}, ErrInvalid},
		{"tag with a comma", NewMemory{Key: "k", Body: "x", Tags: ptr([]string{"a,b"})}, ErrInvalid},
		{"empty tag", NewMemory{Key: "k", Body: "x", Tags: ptr([]string{""})}, ErrInvalid},
		{"bad scope", NewMemory{Scope: "world", Key: "k", Body: "x"}, ErrInvalid},
		{"bad issue id", NewMemory{Key: "k", Body: "x", Issue: ptr(IssueID("NOT AN ID"))}, ErrInvalid},
		{"missing issue", NewMemory{Key: "k", Body: "x", Issue: ptr(IssueID("tst-zzzzzzzz"))}, ErrNotFound},
		{"bad idempotency key", NewMemory{Key: "k", Body: "x", IdempotencyKey: "a b"}, ErrInvalid},
		{"negative rev", NewMemory{Key: "k", Body: "x", Rev: -1}, ErrInvalid},
		{"valid", NewMemory{Key: "Team.Style:go/tests@v2+x_y-z", Body: "x\n\ttabbed", Tags: ptr([]string{"go", "área"}), Issue: ptr(is.ID)}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Remember(t.Context(), alice, tc.in)
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Errorf("Remember(%+v) = %v, want %v", tc.in, err, tc.want)
			}
		})
	}
}

// Rule 18: the body, the tags, each tag, the key and how many memories a
// principal holds in a scope are capped.
func TestMemoryLimits(t *testing.T) {
	s := openStore(t, newDSN(t), Options{Limits: Limits{MemoryBody: 10, MemoryTags: 2, MemoryTagLength: 4, Memories: 2, MemoryKeyLength: 5}})
	ctx := t.Context()
	mustRemember(t, s, alice, NewMemory{Key: "full1", Body: "x"})
	mustRemember(t, s, alice, NewMemory{Key: "full2", Body: "x"})
	tests := []struct {
		name string
		a    Actor
		in   NewMemory
		want string
	}{
		{"body over", alice, NewMemory{Scope: ScopeTeam, Key: "k", Body: strings.Repeat("b", 11)}, "body must be 1-10 bytes"},
		{"body at the cap", alice, NewMemory{Scope: ScopeTeam, Key: "k1", Body: strings.Repeat("b", 10)}, ""},
		{"too many tags", alice, NewMemory{Scope: ScopeTeam, Key: "k2", Body: "x", Tags: ptr([]string{"a", "b", "c"})}, "at most 2 tags"},
		{"repeated tags count once", bob, NewMemory{Key: "k3", Body: "x", Tags: ptr([]string{"a", "a", "b"})}, ""},
		{"tag too long", bob, NewMemory{Key: "k4", Body: "x", Tags: ptr([]string{"abcde"})}, "tag must be 1-4 bytes"},
		{"key too long", bob, NewMemory{Scope: ScopeUser, Key: "abcdef", Body: "x"}, "key must be 1-5"},
		{"third in a scope", alice, NewMemory{Key: "k5", Body: "x"}, "at most 2 memories in project scope"},
		{"another scope has room", alice, NewMemory{Scope: ScopeUser, Key: "k6", Body: "x"}, ""},
		{"another principal has room", bob, NewMemory{Key: "k7", Body: "x"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Remember(ctx, tc.a, tc.in)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("Remember = %v, want success", err)
			case tc.want != "" && (!errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("Remember = %v, want ErrInvalid naming %q", err, tc.want)
			}
		})
	}
	var ml *MemoryLimitError
	if _, err := s.Remember(ctx, alice, NewMemory{Key: "k8", Body: "x"}); !errors.As(err, &ml) || ml.Max != 2 || ml.Scope != ScopeProject {
		t.Errorf("past the per-scope cap = %v, want a MemoryLimitError", err)
	}
	// Replacing one of a full scope's memories is no new memory.
	full, _, err := s.Recall(ctx, alice, MemoryQuery{Key: "full1"})
	if err != nil || len(full) != 1 {
		t.Fatal(err)
	}
	if _, err := s.Remember(ctx, alice, NewMemory{Key: "full1", Body: "y", Rev: full[0].Rev}); err != nil {
		t.Errorf("replacing in a full scope = %v, want success", err)
	}
}

// rankWorld is a project whose memories rank differently by key, by time
// and by relevance, so a ranking that sorts by any one of them alone
// fails. alice works w, labeled api and db; x is someone else's.
func rankWorld(t *testing.T) (s *Store, w, x Issue) {
	t.Helper()
	s, clk := clockStore(t)
	w = mustCreate(t, s, NewIssue{Title: "alice's work", Labels: []string{"api", "db"}})
	x = mustCreate(t, s, NewIssue{Title: "other work", Labels: []string{"ui"}})
	mustStart(t, s, alice, w.ID)
	for _, m := range []struct {
		a  Actor
		in NewMemory
	}{
		{alice, NewMemory{Key: "zeta-old-rest", Body: "oldest, unrelated"}},
		{alice, NewMemory{Key: "gamma-pinned-rel", Body: "pinned, tagged api", Tags: ptr([]string{"api"}), Pinned: ptr(true)}},
		{bob, NewMemory{Key: "alpha-tag", Body: "tagged db", Tags: ptr([]string{"db", "misc"})}},
		{bob, NewMemory{Scope: ScopeTeam, Key: "mid-pinned", Body: "pinned, unrelated", Pinned: ptr(true)}},
		{bob, NewMemory{Key: "beta-link", Body: "linked to w", Issue: ptr(w.ID)}},
		{bob, NewMemory{Key: "omega-other", Body: "linked to x", Issue: ptr(x.ID), Tags: ptr([]string{"ui"})}},
		{alice, NewMemory{Scope: ScopeUser, Key: "delta-mine", Body: "mine, tagged api", Tags: ptr([]string{"api"})}},
		{bob, NewMemory{Scope: ScopeUser, Key: "bobs-own", Body: "bob's, tagged api", Tags: ptr([]string{"api"})}},
		{alice, NewMemory{Key: "kappa-new-rest", Body: "newest, unrelated"}},
	} {
		clk.add(time.Minute)
		mustRemember(t, s, m.a, m.in)
	}
	return s, w, x
}

// Prime ranks pinned memories first, then those linked to the caller's
// in-progress issues or tagged with their labels, then the rest; newest
// first within each, never by key.
func TestPrimeMemoriesRanking(t *testing.T) {
	s, _, _ := rankWorld(t)
	want := []string{"project/gamma-pinned-rel", "team/mid-pinned", "user/delta-mine", "project/beta-link",
		"project/alpha-tag", "project/kappa-new-rest", "project/omega-other", "project/zeta-old-rest"}
	ms, err := s.PrimeMemories(t.Context(), alice, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := keys(ms); !slices.Equal(got, want) {
		t.Fatalf("PrimeMemories(alice) = %q\nwant                    %q", got, want)
	}
	for _, m := range ms {
		if rel := slices.Contains([]string{"gamma-pinned-rel", "delta-mine", "beta-link", "alpha-tag"}, m.Key); m.Relevant != rel {
			t.Errorf("%s relevant = %v, want %v", m.Key, m.Relevant, rel)
		}
	}
	short, err := s.PrimeMemories(t.Context(), alice, 4)
	if err != nil || !slices.Equal(keys(short), want[:4]) {
		t.Errorf("PrimeMemories(alice, 4) = %q, %v; want %q", keys(short), err, want[:4])
	}
	// Bob holds nothing, so only pins rank; and he never sees alice's own.
	ms, err = s.PrimeMemories(t.Context(), bob, 20)
	if err != nil {
		t.Fatal(err)
	}
	wantBob := []string{"team/mid-pinned", "project/gamma-pinned-rel", "project/kappa-new-rest", "user/bobs-own",
		"project/omega-other", "project/beta-link", "project/alpha-tag", "project/zeta-old-rest"}
	if got := keys(ms); !slices.Equal(got, wantBob) {
		t.Errorf("PrimeMemories(bob) = %q\nwant                  %q", got, wantBob)
	}
}

// Start returns the memories relevant to the issue taken, pinned first,
// then newest: those linked to it or tagged with its labels, and no
// others.
func TestIssueMemories(t *testing.T) {
	s, w, x := rankWorld(t)
	tests := []struct {
		a    Actor
		id   IssueID
		want []string
	}{
		{alice, w.ID, []string{"project/gamma-pinned-rel", "user/delta-mine", "project/beta-link", "project/alpha-tag"}},
		{alice, x.ID, []string{"project/omega-other"}},
		{bob, w.ID, []string{"project/gamma-pinned-rel", "user/bobs-own", "project/beta-link", "project/alpha-tag"}},
	}
	for _, tc := range tests {
		ms, err := s.IssueMemories(t.Context(), tc.a, tc.id, 20)
		if err != nil {
			t.Fatal(err)
		}
		if got := keys(ms); !slices.Equal(got, tc.want) {
			t.Errorf("IssueMemories(%s, %s) = %q, want %q", tc.a.Principal, tc.id, got, tc.want)
		}
	}
	if ms, err := s.IssueMemories(t.Context(), alice, w.ID, 2); err != nil || len(ms) != 2 {
		t.Errorf("IssueMemories(limit 2) = %d, %v", len(ms), err)
	}
}

// bd's memories are imported once, as project memories: a key already
// stored is kept, whatever the file says, so importing again changes
// nothing and never undoes an edit made in starfix.
func TestImportMemory(t *testing.T) {
	s := openStore(t, newDSN(t), Options{Limits: Limits{Memories: 1}})
	ctx := t.Context()
	imp := Actor{Principal: "import", Session: "bd", Machine: "m"}
	in := NewMemory{Key: "deploy-window", Body: "Deploys go out on weekday mornings."}
	for _, tc := range []struct {
		name string
		in   NewMemory
		want ImportOutcome
	}{
		{"new key", in, ImportCreated},
		{"again", in, ImportUnchanged},
		{"another body", NewMemory{Key: in.Key, Body: "Deploys go out any time."}, ImportStale},
		{"past the per-scope cap", NewMemory{Key: "second", Body: "the operator's import is not capped"}, ImportCreated},
	} {
		plan, err := s.PlanImportMemory(ctx, tc.in)
		if err != nil || plan != tc.want {
			t.Errorf("PlanImportMemory(%s) = %q, %v; want %q", tc.name, plan, err, tc.want)
		}
		got, err := s.ImportMemory(ctx, imp, tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ImportMemory(%s) = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	ms, _, err := s.Recall(ctx, alice, MemoryQuery{Key: in.Key})
	if err != nil || len(ms) != 1 || ms[0].Body != in.Body || ms[0].Scope != ScopeProject || ms[0].Author != "import" {
		t.Fatalf("imported memory = %+v, %v; want the first body, project scope, by import", ms, err)
	}
	var se *SecretError
	if _, err := s.ImportMemory(ctx, imp, NewMemory{Key: "aws", Body: "AKIA" + "FAKEFAKEFAKEFAKE"}); !errors.As(err, &se) {
		t.Errorf("ImportMemory of a secret = %v, want a SecretError", err)
	}
	evs, err := s.Events(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Op == OpMemoryImport {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d memory.import events, want 2: one per memory created", n)
	}
}
