// Package agentsetup registers `sfx mcp` with agent harnesses: the file
// edits behind `sfx setup` (docs/design/starfix.md §5 and its "As built"
// notes). [Agents] holds each supported harness and every path setup
// writes for it.
//
// # Parts
//
// For each harness setup maintains up to three parts, each a [Target]
// from [Agent.Targets]: the MCP server registration, a marker-delimited
// pointer in the agent's instruction file, and, where the harness has
// one, a SessionStart hook that runs `sfx prime --hook` (Claude Code) or
// `sfx prime --hook=AGENT`. A desktop app (Claude Desktop) has only its
// user-global MCP config, with one entry per project ([DesktopEntry],
// [Agent.DesktopTarget]).
//
// The functions work on file contents; reading and writing the files is
// the caller's. Where a harness keeps two parts in one file (Gemini CLI's
// settings.json) or two harnesses share a file (AGENTS.md for Codex and
// Junie), the caller applies each part to the output of the last.
//
// # Edits
//
// Edits are idempotent and keep the rest of a file: an existing file
// keeps its other servers, its other keys and their order, and applying a
// registration that is already in place returns the content unchanged,
// byte for byte. A JSON file that does change comes back indented with
// two spaces. Codex's TOML keeps everything outside the starfix tables as
// written, comments included, apart from blank lines next to the entry
// and at the end of the file.
//
// The starfix entry itself is replaced wholesale: an extra key (cwd,
// another env variable such as PATH or LD_PRELOAD) could make a server
// named starfix run something else, so [Agent.Registered] fails on one
// and [Agent.Apply] drops it. The entry's env sets [HarnessEnv] to the
// agent's name, so the agents registry knows which harness each session
// runs under. A JSON file with a duplicate key, at a level setup reads,
// is refused: harnesses keep the last copy, so editing the first would
// report a registration the harness never runs.
//
// The functions are safe for concurrent use. [Agents] and [DefaultEntry]
// are shared tables: callers copy an entry to change it and never modify
// them.
package agentsetup
