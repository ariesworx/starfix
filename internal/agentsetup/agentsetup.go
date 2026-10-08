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
	// gets the SessionStart hook, and Claude Code's usage hooks;
	// GlobalHook is the home one. "" when the harness has no hook setup
	// writes.
	Hook, GlobalHook string
	// SessionEnv is the variable the harness sets to its session id for
	// the processes it starts, or "" when it sets none.
	SessionEnv string
	// Note is the step the person must still take that a file edit
	// cannot, such as approving the server. Setup prints it after a
	// project snippet, and after it adds a part to a project file.
	Note string
	// Desktop marks a desktop app: one user-global MCP config at a
	// per-platform path (ConfigPath), no pointer or hook, and no working
	// directory, so each project gets its own entry (DesktopEntry). Its
	// Targets are empty; DesktopTarget is its only one.
	Desktop bool
	// AppConfig is a desktop app's config file, slash-separated,
	// relative to the platform's application data directory.
	AppConfig string

	format  format       // the MCP config's shape
	pointer pointerStyle // where the pointer goes
	hooks   []hookStyle  // how each hook is written; SessionStart's first
}

// ManualMCP reports whether the person registers the MCP server by hand,
// in the harness's settings: it has no project file setup can edit.
func (a Agent) ManualMCP() bool { return a.Project == "" && !a.Desktop }

// Steps is what the person must do that a file edit cannot: the Note
// and, where the MCP server is added by hand, the JSON to paste.
func (a Agent) Steps(e Entry) string {
	if a.ManualMCP() {
		return a.Note + "\n" + a.Snippet(e)
	}
	return a.Note
}

// format is the shape of an agent's MCP config file.
type format int

const (
	jsonClaude format = iota // {"mcpServers": {name: {"type":"stdio", …}}}
	jsonGemini               // {"mcpServers": {name: {…}}}; Cursor's too
	jsonVSCode               // {"servers": {name: {"type":"stdio", …}}}
	tomlCodex                // [mcp_servers.name]
)

// Agents are the supported harnesses, by name: every path setup writes
// is in this table, and each hook's format is in hook.go. The hook
// formats follow each harness's documentation as of 7 Oct 2026 (design
// §5, As built). Callers must not modify it.
var Agents = map[string]Agent{
	"claude-code": {Name: "claude-code", Title: "Claude Code", Project: ".mcp.json", Global: ".claude.json",
		Pointer: "CLAUDE.md", GlobalPointer: ".claude/CLAUDE.md",
		Hook: ".claude/settings.json", GlobalHook: ".claude/settings.json",
		SessionEnv: "CLAUDE_CODE_SESSION_ID", format: jsonClaude, hooks: claudeHooks,
		Note: "Claude Code asks each person to approve a project's .mcp.json servers the first time it opens the project."},
	"codex": {Name: "codex", Title: "Codex", Project: ".codex/config.toml", Global: ".codex/config.toml",
		Pointer: "AGENTS.md", GlobalPointer: ".codex/AGENTS.md",
		Hook: ".codex/hooks.json", GlobalHook: ".codex/hooks.json",
		format: tomlCodex, hooks: []hookStyle{codexHook},
		Note: "Codex reads a project's .codex/config.toml only once the project is trusted, and runs its .codex/hooks.json only once you trust the hooks with /hooks."},
	"gemini": {Name: "gemini", Title: "Gemini CLI", Project: ".gemini/settings.json", Global: ".gemini/settings.json",
		Pointer: "GEMINI.md", GlobalPointer: ".gemini/GEMINI.md",
		Hook: ".gemini/settings.json", GlobalHook: ".gemini/settings.json",
		format: jsonGemini, hooks: []hookStyle{geminiHook},
		Note: "Gemini CLI loads a project's .gemini/settings.json, its MCP server and hooks included, only in a trusted folder: trust the folder when Gemini CLI asks."},
	"cursor": {Name: "cursor", Title: "Cursor", Project: ".cursor/mcp.json", Global: ".cursor/mcp.json",
		Pointer: ".cursor/rules/starfix.mdc", // user rules live in Cursor's settings, not a file
		Hook:    ".cursor/hooks.json", GlobalHook: ".cursor/hooks.json",
		format: jsonGemini, pointer: cursorRule, hooks: []hookStyle{cursorHook},
		Note: "Cursor starts a project's .cursor/mcp.json servers only once enabled: turn starfix on in Cursor Settings › MCP."},
	"vscode": {Name: "vscode", Title: "VS Code", Project: ".vscode/mcp.json",
		NoGlobal: "drop --global and commit .vscode/mcp.json, or run `MCP: Add Server` in VS Code and choose Global",
		Pointer:  ".github/copilot-instructions.md",
		Hook:     ".github/hooks/starfix.json",
		format:   jsonVSCode, hooks: []hookStyle{vscodeHook},
		Note: "VS Code asks you to trust the starfix server in .vscode/mcp.json before it starts it. Agent hooks are a Preview feature: .github/hooks/starfix.json runs only where VS Code has them enabled."},
	"junie": {Name: "junie", Title: "Junie", Project: ".junie/mcp/mcp.json", Global: ".junie/mcp/mcp.json",
		Pointer: "AGENTS.md", GlobalPointer: ".junie/AGENTS.md",
		GlobalHook: ".junie/config.json", // Junie ignores hooks in a project's config
		format:     jsonGemini, hooks: []hookStyle{claudeHook},
		Note: "Junie runs hooks only from ~/.junie/config.json, never a project's: run `sfx setup junie --global --write` for the SessionStart hook."},
	"jetbrains": {Name: "jetbrains", Title: "JetBrains AI Assistant",
		NoGlobal: "drop --global: AI Assistant keeps its MCP servers in the IDE's settings, so add starfix there with scope Global",
		Pointer:  ".aiassistant/rules/starfix.md",
		format:   jsonGemini, pointer: jetbrainsRule,
		Note: "AI Assistant has no MCP file setup can edit. In the IDE open Settings › Tools › AI Assistant › Model Context Protocol (MCP), click Add, paste this JSON, and set the scope to Project:"},
	// Claude Desktop reads no project files and starts its servers with
	// no working directory and without the shell's PATH: see desktop.go.
	"claude-desktop": {Name: "claude-desktop", Title: "Claude Desktop", Desktop: true,
		AppConfig: "Claude/claude_desktop_config.json", format: jsonGemini,
		Note: "Quit Claude Desktop completely and reopen it to start the server. Each launch is a new starfix session."},
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

// Entry is how the harness starts the server: it runs Command with Args.
type Entry struct {
	// Command is a program name the harness looks up on PATH, or an
	// absolute path.
	Command string
	Args    []string
	// Server is the name the entry is registered under; "" means
	// ServerName. Only a desktop app's entry has its own (DesktopEntry),
	// and only the JSON formats honor it.
	Server string
}

// server is the name e is registered under.
func (e Entry) server() string {
	if e.Server == "" {
		return ServerName
	}
	return e.Server
}

// DefaultEntry runs `sfx mcp` from PATH. The harness starts it in the
// project directory, where it finds .starfix.yaml.
var DefaultEntry = Entry{Command: "sfx", Args: []string{"mcp"}}

// Result says what Apply or Remove did.
type Result int

// Results of Apply and Remove.
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
// file) and returns the new content and what changed: Unchanged, with
// content itself, when e is registered already; Updated when it replaced
// an entry of the same name; Added otherwise. It fails on content it
// cannot parse or edit.
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
	if a.has(content, e) {
		return out, Updated, nil
	}
	return out, Added, nil
}

// Remove takes the entry registered under ServerName out of content.
func (a Agent) Remove(content []byte) ([]byte, Result, error) {
	return a.remove(content, DefaultEntry)
}

// remove takes out the entry registered under e's name. Content that
// cannot be parsed is an error, not Unchanged.
func (a Agent) remove(content []byte, e Entry) ([]byte, Result, error) {
	var out []byte
	var found bool
	var err error
	if a.format == tomlCodex {
		out, found, err = removeTOML(content)
	} else {
		out, found, err = removeJSON(content, a.format, e.server())
	}
	if err != nil || !found {
		return content, Unchanged, err
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

// has reports whether content has an entry under e's name, whatever
// it holds.
func (a Agent) has(content []byte, e Entry) bool {
	if a.format == tomlCodex {
		return tomlHas(content)
	}
	_, ok := jsonEntry(content, a.format, e.server())
	return ok
}

// Kind is what a Target holds.
type Kind int

// Kinds, in the order setup handles them.
const (
	KindMCP     Kind = iota // the MCP server registration
	KindPointer             // the block in the agent's instruction file
	KindHook                // the hooks: SessionStart, and Claude Code's usage hooks
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
// or, with global, in the home directory. A part with no file there, such
// as an MCP config the harness keeps in its own settings or a hook it
// lacks, has no target, and a desktop app has none at all (see
// [Agent.DesktopTarget]).
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

// Apply puts the target's part in content and returns the new content
// and what changed, as [Agent.Apply] does.
func (t Target) Apply(content []byte, e Entry) ([]byte, Result, error) {
	switch t.Kind {
	case KindPointer:
		return applyPointer(content, t.agent.pointer)
	case KindHook:
		return applyHooks(t.agent.hooks, content, e, t.agent.Name)
	}
	return t.agent.Apply(content, e)
}

// Remove takes the target's part out of content. For a pointer or a
// hook, empty output means the file held nothing else and may be
// deleted.
func (t Target) Remove(content []byte, e Entry) ([]byte, Result, error) {
	switch t.Kind {
	case KindPointer:
		return removePointer(content, t.agent.pointer)
	case KindHook:
		return removeHooks(t.agent.hooks, content, e, t.agent.Name)
	}
	return t.agent.remove(content, e)
}

// Registered reports whether content already holds the target's part as
// Apply would write it.
func (t Target) Registered(content []byte, e Entry) bool {
	switch t.Kind {
	case KindPointer:
		return pointerRegistered(content, t.agent.pointer)
	case KindHook:
		return len(missingHooks(t.agent.hooks, content, e, t.agent.Name)) == 0
	}
	return t.agent.Registered(content, e)
}

// Snippet is the target's part on its own, to paste by hand.
func (t Target) Snippet(e Entry) string {
	switch t.Kind {
	case KindPointer:
		return pointerSnippet(t.agent.pointer)
	case KindHook:
		return hooksSnippet(t.agent.hooks, e, t.agent.Name)
	}
	return t.agent.Snippet(e)
}

// MissingHooks lists the events whose starfix hook content lacks, as
// Apply would write it, for a hook target; nil for any other kind.
func (t Target) MissingHooks(content []byte, e Entry) []string {
	if t.Kind != KindHook {
		return nil
	}
	return missingHooks(t.agent.hooks, content, e, t.agent.Name)
}
