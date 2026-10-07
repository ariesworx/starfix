// Package agentsetup registers `sfx mcp` with an agent harness (design
// §5, `sfx setup`). It maintains up to three files per harness: the MCP
// configuration, a marker-delimited pointer in the agent's instruction
// file, and, where the harness has one, a SessionStart hook that runs
// `sfx prime --hook`.
//
// Edits are idempotent and minimal: an existing file keeps its other
// servers, its other keys and their order, and an existing starfix entry
// keeps keys starfix does not own (a timeout, other env variables). The
// entry's env sets HarnessEnv to the agent's name, so the agents registry
// knows which harness each session runs under. Applying a
// registration that is already in place changes nothing, byte for byte.
// The functions here work on file contents; reading and writing the files
// is the caller's.
package agentsetup

import (
	"fmt"
	"sort"
	"strings"
)

// ServerName is the name the MCP server is registered under.
const ServerName = "starfix"

// HarnessEnv is the variable setup sets, in the server's env, to the
// agent's Name; `sfx mcp` sends it to the server (client.HarnessFromEnv).
const HarnessEnv = "STARFIX_HARNESS"

// Agent is one supported harness.
type Agent struct {
	// Name is what `sfx setup` takes.
	Name string
	// Title is the harness's own name.
	Title string
	// Project is the project-scoped config file, relative to the
	// repository root; it is meant to be committed.
	Project string
	// Global is the user's config file, relative to the home directory;
	// "" when the harness keeps it nowhere setup can edit portably.
	Global string
	// NoGlobal is the fix given when Global is "".
	NoGlobal string
	// Pointer is the instruction file, relative to the repository root,
	// that gets the starfix block; GlobalPointer is the home one, or "".
	Pointer, GlobalPointer string
	// Hook is the settings file, relative to the repository root, that
	// gets the SessionStart hook; GlobalHook is the home one. "" when
	// the harness has no hook setup writes.
	Hook, GlobalHook string
	// SessionEnv is the variable the harness sets to its session id for
	// the processes it starts, or "" when it sets none.
	SessionEnv string
	// Note is said after a project snippet: anything the person must do that a
	// file edit cannot.
	Note string

	format  format
	pointer pointerStyle
}

type format int

const (
	jsonClaude format = iota // {"mcpServers": {name: {"type":"stdio", …}}}
	jsonGemini               // {"mcpServers": {name: {…}}}; Cursor's too
	jsonVSCode               // {"servers": {name: {"type":"stdio", …}}}
	tomlCodex                // [mcp_servers.name]
)

// Agents are the supported harnesses, by name: every path setup writes
// is in this table. Hooks are written only for Claude Code; the others
// either have none or none whose format is settled enough to write.
var Agents = map[string]Agent{
	"claude-code": {Name: "claude-code", Title: "Claude Code", Project: ".mcp.json", Global: ".claude.json",
		Pointer: "CLAUDE.md", GlobalPointer: ".claude/CLAUDE.md",
		Hook: ".claude/settings.json", GlobalHook: ".claude/settings.json",
		SessionEnv: "CLAUDE_CODE_SESSION_ID", format: jsonClaude,
		Note: "Claude Code asks each person to approve a project's .mcp.json servers the first time it opens the project."},
	"codex": {Name: "codex", Title: "Codex", Project: ".codex/config.toml", Global: ".codex/config.toml",
		Pointer: "AGENTS.md", GlobalPointer: ".codex/AGENTS.md",
		format: tomlCodex,
		Note:   "Codex reads a project's .codex/config.toml only once the project is trusted."},
	"gemini": {Name: "gemini", Title: "Gemini CLI", Project: ".gemini/settings.json", Global: ".gemini/settings.json",
		Pointer: "GEMINI.md", GlobalPointer: ".gemini/GEMINI.md",
		format: jsonGemini},
	"cursor": {Name: "cursor", Title: "Cursor", Project: ".cursor/mcp.json", Global: ".cursor/mcp.json",
		Pointer: ".cursor/rules/starfix.mdc", // user rules live in Cursor's settings, not a file
		format:  jsonGemini, pointer: ruleFile},
	"vscode": {Name: "vscode", Title: "VS Code", Project: ".vscode/mcp.json",
		NoGlobal: "drop --global and commit .vscode/mcp.json, or run `MCP: Add Server` in VS Code and choose Global",
		Pointer:  ".github/copilot-instructions.md",
		format:   jsonVSCode},
}

// Names lists the supported agents, sorted.
func Names() []string {
	out := make([]string, 0, len(Agents))
	for n := range Agents {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Entry is how the harness starts the server.
type Entry struct {
	Command string
	Args    []string
}

// DefaultEntry runs `sfx mcp` from PATH. The harness starts it in the
// project directory, where it finds .starfix.yaml.
var DefaultEntry = Entry{Command: "sfx", Args: []string{"mcp"}}

// Result says what Apply or Remove did.
type Result int

// Results.
const (
	Unchanged Result = iota
	Added
	Updated
	Removed
)

func (r Result) String() string {
	return [...]string{"unchanged", "added", "updated", "removed"}[r]
}

// Snippet is the registration on its own, to paste by hand.
func (a Agent) Snippet(e Entry) string {
	if a.format == tomlCodex {
		return strings.Join(tomlTable(e, a.Name), "\n") + "\n"
	}
	b, err := applyJSON(nil, a.format, e, a.Name)
	if err != nil {
		panic(fmt.Sprintf("agentsetup: snippet: %v", err)) // only on a programming error
	}
	return string(b)
}

// Apply registers e in the config file content (nil or empty for a new
// file) and returns the new content.
func (a Agent) Apply(content []byte, e Entry) ([]byte, Result, error) {
	if a.Registered(content, e) {
		return content, Unchanged, nil
	}
	var out []byte
	var err error
	if a.format == tomlCodex {
		out, err = applyTOML(content, e, a.Name)
	} else {
		out, err = applyJSON(content, a.format, e, a.Name)
	}
	if err != nil {
		return nil, Unchanged, err
	}
	if a.has(content) {
		return out, Updated, nil
	}
	return out, Added, nil
}

// Remove takes the starfix registration out of content.
func (a Agent) Remove(content []byte) ([]byte, Result, error) {
	if !a.has(content) {
		return content, Unchanged, nil
	}
	if a.format == tomlCodex {
		return removeTOML(content), Removed, nil
	}
	out, err := removeJSON(content, a.format)
	if err != nil {
		return nil, Unchanged, err
	}
	return out, Removed, nil
}

// Registered reports whether content already starts the server as e does.
func (a Agent) Registered(content []byte, e Entry) bool {
	if a.format == tomlCodex {
		return tomlRegistered(content, e, a.Name)
	}
	return jsonRegistered(content, a.format, e, a.Name)
}

// has reports whether content has any starfix entry.
func (a Agent) has(content []byte) bool {
	if a.format == tomlCodex {
		_, _, ok := tomlSection(strings.Split(string(content), "\n"))
		return ok
	}
	_, ok := jsonEntry(content, a.format)
	return ok
}

// Kind is what a Target holds.
type Kind int

// Kinds, in the order setup handles them.
const (
	KindMCP     Kind = iota // the MCP server registration
	KindPointer             // the block in the agent's instruction file
	KindHook                // the SessionStart hook
)

func (k Kind) String() string {
	return [...]string{"mcp", "pointer", "hook"}[k]
}

// Target is one file setup maintains for an agent.
type Target struct {
	Kind Kind
	// Path is slash-separated, relative to the repository root or, for
	// a global target, the home directory.
	Path  string
	agent Agent
}

// Targets lists the files setup maintains for the agent, in the project
// or, with global, in the home directory. A global setup has no MCP
// target when Global is "".
func (a Agent) Targets(global bool) []Target {
	mcp, pointer, hook := a.Project, a.Pointer, a.Hook
	if global {
		mcp, pointer, hook = a.Global, a.GlobalPointer, a.GlobalHook
	}
	var out []Target
	for _, t := range []Target{{KindMCP, mcp, a}, {KindPointer, pointer, a}, {KindHook, hook, a}} {
		if t.Path != "" {
			out = append(out, t)
		}
	}
	return out
}

// Apply puts the target's part in content and returns the new content.
func (t Target) Apply(content []byte, e Entry) ([]byte, Result, error) {
	switch t.Kind {
	case KindPointer:
		return applyPointer(content, t.agent.pointer)
	case KindHook:
		return applyHook(content, e)
	}
	return t.agent.Apply(content, e)
}

// Remove takes the target's part out of content. Empty output for a
// pointer means the file held nothing else and may be deleted.
func (t Target) Remove(content []byte, e Entry) ([]byte, Result, error) {
	switch t.Kind {
	case KindPointer:
		return removePointer(content, t.agent.pointer)
	case KindHook:
		return removeHook(content, e)
	}
	return t.agent.Remove(content)
}

// Registered reports whether content already holds the target's part as
// Apply would write it.
func (t Target) Registered(content []byte, e Entry) bool {
	switch t.Kind {
	case KindPointer:
		return pointerRegistered(content, t.agent.pointer)
	case KindHook:
		return hookRegistered(content, e)
	}
	return t.agent.Registered(content, e)
}

// Snippet is the target's part on its own, to paste by hand.
func (t Target) Snippet(e Entry) string {
	switch t.Kind {
	case KindPointer:
		return pointerSnippet(t.agent.pointer)
	case KindHook:
		return hookSnippet(e)
	}
	return t.agent.Snippet(e)
}
