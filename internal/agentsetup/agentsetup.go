// Package agentsetup registers `sfx mcp` with an agent harness by
// editing that harness's MCP configuration (design §5, `sfx setup`).
//
// Edits are idempotent and minimal: an existing file keeps its other
// servers, its other keys and their order, and an existing starfix entry
// keeps keys starfix does not own (an env block, a timeout). Applying a
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

// Agent is one supported harness.
type Agent struct {
	// Name is what `sfx setup` takes.
	Name string
	// Title is the harness's own name.
	Title string
	// Project is the project-scoped config file, relative to the
	// repository root; it is meant to be committed.
	Project string
	// Global is the user's config file, relative to the home directory.
	Global string
	// SessionEnv is the variable the harness sets to its session id for
	// the processes it starts, or "" when it sets none.
	SessionEnv string
	// Note is said after a project snippet: anything the person must do that a
	// file edit cannot.
	Note string

	format format
}

type format int

const (
	jsonClaude format = iota // {"mcpServers": {name: {"type":"stdio", …}}}
	jsonGemini               // {"mcpServers": {name: {…}}}
	tomlCodex                // [mcp_servers.name]
)

// Agents are the supported harnesses, by name.
var Agents = map[string]Agent{
	"claude-code": {Name: "claude-code", Title: "Claude Code", Project: ".mcp.json", Global: ".claude.json",
		SessionEnv: "CLAUDE_CODE_SESSION_ID", format: jsonClaude,
		Note: "Claude Code asks each person to approve a project's .mcp.json servers the first time it opens the project."},
	"codex": {Name: "codex", Title: "Codex", Project: ".codex/config.toml", Global: ".codex/config.toml",
		format: tomlCodex,
		Note:   "Codex reads a project's .codex/config.toml only once the project is trusted."},
	"gemini": {Name: "gemini", Title: "Gemini CLI", Project: ".gemini/settings.json", Global: ".gemini/settings.json",
		format: jsonGemini},
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
		return strings.Join(tomlTable(e), "\n") + "\n"
	}
	b, err := applyJSON(nil, a.format, e)
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
	if a.format == tomlCodex {
		out = applyTOML(content, e)
	} else {
		var err error
		if out, err = applyJSON(content, a.format, e); err != nil {
			return nil, Unchanged, err
		}
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
	out, err := removeJSON(content)
	if err != nil {
		return nil, Unchanged, err
	}
	return out, Removed, nil
}

// Registered reports whether content already starts the server as e does.
func (a Agent) Registered(content []byte, e Entry) bool {
	if a.format == tomlCodex {
		return tomlRegistered(content, e)
	}
	return jsonRegistered(content, a.format, e)
}

// has reports whether content has any starfix entry.
func (a Agent) has(content []byte) bool {
	if a.format == tomlCodex {
		_, _, ok := tomlSection(strings.Split(string(content), "\n"))
		return ok
	}
	_, ok := jsonEntry(content)
	return ok
}
