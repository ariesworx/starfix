package mcpserver

import (
	"context"

	"github.com/ariesworx/starfix/internal/proto"
)

// scopes are the memory scopes, the default first.
var scopes = []any{"project", "user", "team"}

// rememberDesc steers the scope choice: project unless the memory is the
// person's own preference, and team only when someone chose it.
const rememberDesc = "Save a fact for later sessions. scope: project (default); user for personal preferences " +
	"(ask if unsure); team only if asked. rev replaces."

// RememberIn writes a memory. Pinning is left to people (sfx pin).
type RememberIn struct {
	Key   string   `json:"key"`
	Body  string   `json:"body"`
	Scope string   `json:"scope,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Issue *string  `json:"issue,omitempty" jsonschema:"empty unlinks"`
	Rev   int64    `json:"rev,omitempty"`

	idem string
}

// RecallIn searches memories.
type RecallIn struct {
	Text  string `json:"text,omitempty" jsonschema:"in key or body"`
	Tag   string `json:"tag,omitempty"`
	Key   string `json:"key,omitempty"`
	Scope string `json:"scope,omitempty"`
}

// ForgetIn deletes a memory.
type ForgetIn struct {
	Key   string `json:"key"`
	Scope string `json:"scope,omitempty"`
	Rev   int64  `json:"rev,omitempty"`

	idem string
}

func (in *RememberIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

func (in *ForgetIn) prepare() error {
	in.idem = proto.NewIdem("mcp")
	return nil
}

// Memory is one memory, compact: By wrote it last, At is when.
type Memory struct {
	Key      string   `json:"key"`
	Scope    string   `json:"scope"`
	Body     string   `json:"body"`
	Tags     []string `json:"tags,omitempty"`
	Issue    string   `json:"issue,omitempty"`
	Pinned   bool     `json:"pinned,omitempty"`
	Relevant bool     `json:"relevant,omitempty"`
	Rev      int64    `json:"rev,omitempty"`
	By       string   `json:"by,omitempty"`
	At       string   `json:"at,omitempty"`
}

// Memories is recall's result. More counts the matches left out;
// Truncated says a body was cut.
type Memories struct {
	untrusted
	Memories  []Memory `json:"memories"`
	More      int      `json:"more,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (r *Memories) mark() { r.set(len(r.Memories) > 0) }

// How much of each memory's body recall and start give.
const (
	maxMemoryText   = 1000
	startMemoryText = 300
)

// compactMemories converts ms, each body cut to n bytes, and reports
// whether any was cut.
func compactMemories(ms []proto.Memory, n int) ([]Memory, bool) {
	out := make([]Memory, len(ms))
	cutAny := false
	for i, m := range ms {
		body, c := cut(m.Body, n)
		cutAny = cutAny || c
		out[i] = Memory{Key: m.Key, Scope: m.Scope, Body: body, Tags: m.Tags, Issue: m.Issue, Pinned: m.Pinned,
			Relevant: m.Relevant, Rev: m.Rev, By: m.UpdatedBy}
		if !m.UpdatedAt.IsZero() {
			out[i].At = stamp(m.UpdatedAt)
		}
	}
	return out, cutAny
}

func (s *Server) registerMemory() {
	add(s, tool{name: "remember", desc: rememberDesc, ann: write, retry: true, enums: enums{"scope": scopes}},
		func(ctx context.Context, c Conn, in RememberIn) (proto.WriteResult, error) {
			var out proto.WriteResult
			a := proto.RememberArgs{Scope: in.Scope, Key: in.Key, Body: in.Body, Issue: in.Issue, Rev: in.Rev, Idem: in.idem}
			// JSON decodes an absent tags as nil and [] as empty, so a
			// replace that omits tags keeps them and [] clears them.
			if in.Tags != nil {
				a.Tags = &in.Tags
			}
			return out, c.Call(ctx, proto.OpRemember, a, &out)
		})
	add(s, tool{name: "recall", desc: "Search memories.", ann: readOnly, retry: true,
		enums: enums{"scope": scopes}},
		func(ctx context.Context, c Conn, in RecallIn) (Memories, error) { return recall(ctx, c, in) })
	add(s, tool{name: "forget", desc: "Delete a memory.", ann: write, retry: true, enums: enums{"scope": scopes}},
		func(ctx context.Context, c Conn, in ForgetIn) (proto.WriteResult, error) {
			var out proto.WriteResult
			return out, c.Call(ctx, proto.OpForget, proto.ForgetArgs{Scope: in.Scope, Key: in.Key, Rev: in.Rev, Idem: in.idem}, &out)
		})
}

// recall is the recall tool: bodies are cut to maxMemoryText, then the
// last memories dropped to fit MaxResultTokens.
func recall(ctx context.Context, c Conn, in RecallIn) (Memories, error) {
	var r proto.RecallResult
	if err := c.Call(ctx, proto.OpRecall, proto.RecallArgs{Text: in.Text, Tag: in.Tag, Key: in.Key, Scope: in.Scope}, &r); err != nil {
		return Memories{}, err
	}
	ms, cutAny := compactMemories(r.Memories, maxMemoryText)
	out := Memories{Memories: ms, More: r.More, Truncated: cutAny}
	for size(out) > MaxResultTokens && len(out.Memories) > 1 {
		out.Memories, out.More, out.Truncated = out.Memories[:len(out.Memories)-1], out.More+1, true
	}
	return out, nil
}
