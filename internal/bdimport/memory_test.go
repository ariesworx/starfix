package bdimport

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/store"
)

func memoryLine(key, value string) string {
	return `{"_type":"memory","key":` + quote(key) + `,"value":` + quote(value) + `}`
}

// quote is s as a JSON string.
func quote(s string) string {
	b, _ := json.Marshal(s) // a string always marshals
	return string(b)
}

// bd's memory records become project memories authored by the importer,
// keyed as bd keyed them without its kv.memory. prefix. A record that
// cannot be stored is reported with a fix, and the rest still import.
func TestImportMemories(t *testing.T) {
	token := "xox" + "b-" + strings.Repeat("0123456789", 2) // an obviously fake Slack token
	tests := []struct {
		name     string
		lines    []string
		memories Counts
		problems []string // problemKeys of every level
		stored   map[string]string
	}{
		{"plain", []string{memoryLine("deploy-window", "Deploys go out on weekday mornings.")},
			Counts{Created: 1}, nil, map[string]string{"deploy-window": "Deploys go out on weekday mornings."}},
		{"kv prefix", []string{memoryLine("kv.memory.build", "Run task check first.")},
			Counts{Created: 1}, nil, map[string]string{"build": "Run task check first."}},
		{"snake_case key", []string{memoryLine("kv.memory.release_day", "Tuesdays.")},
			Counts{Created: 1}, nil, map[string]string{"release_day": "Tuesdays."}},
		{"duplicate key, the last line wins", []string{memoryLine("k", "first"), memoryLine("kv.memory.k", "second")},
			Counts{Created: 1}, []string{"duplicate: the same memory key appears on more than one line; the last one is used [k]"},
			map[string]string{"k": "second"}},
		{"key starfix cannot hold", []string{memoryLine("has space", "x"), memoryLine("ok", "y")},
			Counts{Created: 1, Failed: 1}, []string{"invalid: memory has space: invalid input: memory key must be 1-128 bytes of letters, digits and ._:/@+-, starting with a letter or digit [has space]"},
			map[string]string{"ok": "y"}},
		{"secret", []string{memoryLine("slack", "the bot token is "+token)},
			Counts{Failed: 1}, []string{"secret: memory slack looks like it holds a secret (Slack token); not imported [slack]"}, nil},
		{"no value", []string{`{"_type":"memory","key":"empty"}`},
			Counts{Failed: 1}, []string{"invalid: line 1: memory empty has no value [empty]"}, nil},
		{"not an object's fields", []string{`{"_type":"memory","key":7,"value":"x"}`},
			Counts{Failed: 1}, []string{"invalid: line 1: memory record: json: cannot unmarshal number into Go struct field bdMemory.key of type string []"}, nil},
		{"unknown field", []string{`{"_type":"memory","key":"k","value":"v","created_at":"2026-01-01T00:00:00Z"}`},
			Counts{Created: 1}, []string{"field: memory field created_at is not stored [k]"}, map[string]string{"k": "v"}},
		{"control characters", []string{memoryLine("k", "a\x1b[2Jb\u202ec")},
			Counts{Created: 1}, []string{"text: memory value had control or bidirectional characters; they were removed [k]"},
			map[string]string{"k": "a[2Jbc"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)
			rep := importBytes(t, s, []byte(strings.Join(tc.lines, "\n")), false)
			if rep.Memories != tc.memories {
				t.Errorf("memories = %+v, want %+v", rep.Memories, tc.memories)
			}
			got := append(problemKeys(rep, LevelError), problemKeys(rep, LevelWarning)...)
			if !slices.Equal(got, tc.problems) {
				t.Errorf("problems =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(tc.problems, "\n  "))
			}
			for _, p := range rep.Problems {
				if p.Fix == "" || strings.Contains(p.Message+p.Fix, token) {
					t.Errorf("problem without a fix, or echoing the secret: %+v", p)
				}
			}
			ms, _, err := s.Recall(t.Context(), admin, store.MemoryQuery{})
			if err != nil {
				t.Fatal(err)
			}
			stored := map[string]string{}
			for _, m := range ms {
				stored[m.Key] = m.Body
				if m.Scope != store.ScopeProject || m.Author != admin.Principal {
					t.Errorf("%s stored in %s scope by %s, want project scope by %s", m.Key, m.Scope, m.Author, admin.Principal)
				}
			}
			if len(stored) != len(tc.stored) {
				t.Errorf("stored %v, want %v", stored, tc.stored)
			}
			for k, v := range tc.stored {
				if stored[k] != v {
					t.Errorf("stored %s = %q, want %q", k, stored[k], v)
				}
			}
		})
	}
}

// A key that looks like a secret is refused by its line alone, before
// any other check could name it: not in a message, an id or a warning.
func TestImportMemorySecretKey(t *testing.T) {
	key := "gh" + "p_" + strings.Repeat("FakeToken0", 4) // an obviously fake GitHub token
	lines := []string{
		memoryLine(key, "x"),
		`{"_type":"memory","key":` + quote("kv.memory."+key) + `}`,
		`{"_type":"memory","key":` + quote(key) + `,"value":"x","created_at":"2026-01-01T00:00:00Z"}`,
		memoryLine("ok", "y"),
	}
	rep := importBytes(t, newStore(t), []byte(strings.Join(lines, "\n")), false)
	if rep.Memories != (Counts{Created: 1, Failed: 3}) {
		t.Errorf("memories = %+v, want 1 created and 3 failed", rep.Memories)
	}
	want := []string{
		"secret: line 1: a memory's key looks like it holds a secret (GitHub token); not imported []",
		"secret: line 2: a memory's key looks like it holds a secret (GitHub token); not imported []",
		"secret: line 3: a memory's key looks like it holds a secret (GitHub token); not imported []",
	}
	if got := append(problemKeys(rep, LevelError), problemKeys(rep, LevelWarning)...); !slices.Equal(got, want) {
		t.Errorf("problems =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for _, p := range rep.Problems {
		if strings.Contains(p.Message+p.Fix+strings.Join(p.IDs, " "), key[4:]) {
			t.Errorf("problem echoes the key: %+v", p)
		}
	}
}

// A memory already in the store is kept, not overwritten, when the file
// holds another value for its key: bd records no time to say which is
// newer. A dry run counts the same and writes nothing.
func TestImportMemoryKeepsTheStoredOne(t *testing.T) {
	s := newStore(t)
	importBytes(t, s, []byte(memoryLine("k", "first")), false)
	seq := lastSeq(t, s)
	for _, dry := range []bool{true, false} {
		rep := importBytes(t, s, []byte(memoryLine("k", "second")), dry)
		if rep.Memories != (Counts{Stale: 1}) || !slices.Equal(problemKeys(rep, LevelWarning),
			[]string{"stale: the store's memory with this key has another value; kept it [k]"}) {
			t.Errorf("dry %v: memories %+v, warnings %v", dry, rep.Memories, problemKeys(rep, LevelWarning))
		}
	}
	if got := lastSeq(t, s); got != seq {
		t.Errorf("a kept memory wrote %d events", got-seq)
	}
	dry := importBytes(t, newStore(t), []byte(memoryLine("k", "first")), true)
	if dry.Memories != (Counts{Created: 1}) {
		t.Errorf("dry run on an empty store: %+v", dry.Memories)
	}
}
