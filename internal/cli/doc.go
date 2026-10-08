// Package cli is sfx, the starfix client: the commands people run in a
// terminal, `sfx mcp` for agents, `sfx prime` and its SessionStart hook,
// `sfx usage --hook`, which sends Claude Code's token counts, `sfx setup`
// and `sfx upgrade`. cmd/sfx only wires the process to
// [Run].
//
// [Run] executes one command line in an [Env], which holds everything the
// package takes from the process: its streams, environment variables,
// host name, home directory, platform and executable. Nothing here touches
// the process's own streams. A command connects to starfixd on first use,
// over SSH with the pinned host key (docs/design/starfix.md §2), and the
// connection is closed when the command returns.
//
// Issue text, names and server messages are written by other people, so
// everything printed is escaped (§7, "As built (untrusted text)"): text
// output passes through a [safetext.Writer], and each single-line field is
// escaped on its own, so it cannot start a line. With --json a command
// prints one JSON document, errors included, written with [safetext.JSON];
// `sfx watch` prints one per line, an object per event. A failure prints
// an `sfx:` line and, when the error has one, a `fix:` line naming the
// next step.
package cli
