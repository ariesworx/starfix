package client

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/ariesworx/starfix/internal/proto"
)

// SessionEnv lists the environment variables that name the harness session,
// most specific first. STARFIX_SESSION is set by hand or by a harness's
// MCP config; CLAUDE_CODE_SESSION_ID is set by Claude Code for the
// processes it starts; CLAUDE_SESSION_ID is the older spelling. Codex and
// Gemini CLI set no session variable that starfix knows of: the CLI then
// uses CLISession, and `sfx mcp` picks one per process.
var SessionEnv = []string{"STARFIX_SESSION", "CLAUDE_CODE_SESSION_ID", "CLAUDE_SESSION_ID"}

// SessionFromEnv returns the first non-empty variable of SessionEnv, or ""
// to let the server assign a session id.
func SessionFromEnv(getenv func(string) string) string {
	for _, k := range SessionEnv {
		if v := getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// HarnessFromEnv names the agent harness this process runs under, as an
// `sfx setup` agent name, for the agents registry: STARFIX_HARNESS (which
// setup writes into every MCP config) wins; else CLAUDECODE=1 means
// claude-code and GEMINI_CLI=1 means gemini, the markers those harnesses
// set for the commands they run; else "".
func HarnessFromEnv(getenv func(string) string) string {
	switch {
	case getenv("STARFIX_HARNESS") != "":
		return getenv("STARFIX_HARNESS")
	case getenv("CLAUDECODE") == "1":
		return "claude-code"
	case getenv("GEMINI_CLI") == "1":
		return "gemini"
	}
	return ""
}

// CLISession is the session of a person's own `sfx` commands: one per
// principal and machine, so a claim taken in one command is the same
// session's in the next. The server lets only this session renew every
// session's claims (`sfx away`).
const CLISession = proto.CLISession

// NewSessionID returns a random session id, "m-" and 16 hex digits, for a
// process that runs without a harness session id.
func NewSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return "m-" + hex.EncodeToString(b[:])
}
