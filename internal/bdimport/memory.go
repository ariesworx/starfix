package bdimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/safetext"
	"github.com/ariesworx/starfix/internal/store"
)

// bdMemory is a bd memory record, one entry of bd's kv.memory.* store as
// its export writes it: {"_type":"memory","key":…,"value":…}.
type bdMemory struct {
	Key   string  `json:"key"`
	Value *string `json:"value"`
}

// bdMemoryPrefix starts the keys of bd's memories in its kv store. Its
// export writes the key without it; a key that keeps it is read the same.
const bdMemoryPrefix = "kv.memory."

// memoryFields are the fields of a memory record Import reads.
var memoryFields = map[string]bool{"_type": true, "key": true, "value": true}

// memLine is a memory to import and the line it came from.
type memLine struct {
	n   int
	mem store.NewMemory
}

// parseMemory maps a memory record, the text b of line n whose fields are
// raw. It returns false for a record it cannot map, having reported it.
func parseMemory(b []byte, raw map[string]json.RawMessage, n int, rep *Report) (memLine, bool) {
	var bm bdMemory
	if err := json.Unmarshal(b, &bm); err != nil {
		rep.Memories.Failed++
		rep.fail("invalid", n, fixReexport, "line %d: memory record: %v", n, err)
		return memLine{}, false
	}
	key := safetext.CleanLine(strings.TrimPrefix(bm.Key, bdMemoryPrefix))
	if bm.Value == nil {
		rep.Memories.Failed++
		rep.fail("invalid", n, fixReexport, "line %d: memory %s has no value", n, key).IDs = []string{key}
		return memLine{}, false
	}
	var extra []string
	for k, v := range raw {
		if !memoryFields[k] && !empty(v) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	for _, f := range extra {
		rep.warn("memory-field:"+f, "field", key, n, "keep bd's export if you need it; a memory has no such field",
			fmt.Sprintf("memory field %s is not stored", f))
	}
	body := safetext.CleanText(*bm.Value)
	if body != *bm.Value {
		rep.warn("memory-text", "text", key, n, fixNone, "memory value had control or bidirectional characters; they were removed")
	}
	return memLine{n: n, mem: store.NewMemory{Scope: store.ScopeProject, Key: key, Body: body}}, true
}

// memories imports the memory records, each a project memory authored
// by the importer. A key the store already holds is kept as it is
// (store.ImportMemory).
func (im *importer) memories(ctx context.Context, mems []memLine) error {
	for _, m := range mems {
		key := m.mem.Key
		out, err := im.st.PlanImportMemory(ctx, m.mem)
		if err == nil && !im.opts.DryRun {
			out, err = im.st.ImportMemory(ctx, im.opts.Actor, m.mem)
		}
		if se, ok := errors.AsType[*store.SecretError](err); ok {
			im.rep.Memories.Failed++
			fix := "remove the secret from the memory in bd, naming where it is kept instead, and import again"
			if se.Field != "body" {
				// The key is the secret: the report names the line alone.
				im.rep.fail("secret", m.n, fix, "a memory's %s looks like it holds a secret (%s); not imported", se.Field, se.Kind)
				continue
			}
			im.rep.fail("secret", m.n, fix, "memory %s looks like it holds a secret (%s); not imported", key, se.Kind).IDs = []string{key}
			continue
		}
		if err != nil {
			if !recordError(err) {
				return fmt.Errorf("import memory %s: %w", key, err)
			}
			im.rep.Memories.Failed++
			im.rep.fail("invalid", m.n, fixReexport, "memory %s: %v", key, err).IDs = []string{key}
			continue
		}
		count(&im.rep.Memories, out)
		if out == store.ImportStale {
			im.rep.warn("stale-memory", "stale", key, m.n, fixStale, "the store's memory with this key has another value; kept it")
		}
	}
	return nil
}
