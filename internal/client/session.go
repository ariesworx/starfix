package client

// SessionEnv lists the environment variables that name the harness session,
// most specific first. STARFIX_SESSION is set by hand or by a harness's
// MCP config; CLAUDE_CODE_SESSION_ID is set by Claude Code for the
// processes it starts; CLAUDE_SESSION_ID is the older spelling. Codex and
// Gemini CLI set no session variable that starfix knows of, so their
// sessions get a server-assigned id unless STARFIX_SESSION is set.
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
