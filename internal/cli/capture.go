package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ariesworx/starfix/internal/agentsetup"
	"github.com/ariesworx/starfix/internal/capture"
	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// usageTimeout bounds `usage --hook`, a little inside the timeout setup
// gives the hook, so a slow server costs a note, not a killed process.
// Claude Code waits for the SessionEnd hook as it exits, so that run has
// less.
func usageTimeout(event string) time.Duration {
	if event == "SessionEnd" {
		return agentsetup.SessionEndTimeout - time.Second
	}
	return agentsetup.UsageTimeout - 5*time.Second
}

func cmdUsage(ctx context.Context, r *runner, args []string) error {
	const usage = "usage --hook[=AGENT]"
	fs := r.newFlags("usage")
	hook := hookFlag{agents: agentsetup.UsageHooks()}
	fs.Var(&hook, "hook", "run as AGENT's hook (bare: claude-code): send the session's new token counts, and never fail")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "usage takes no arguments")
	}
	if hook.agent == "" {
		return usagef(usage, "usage runs from an agent's hooks: give --hook; `sfx setup AGENT --write` installs them")
	}
	r.usageHook(ctx)
	return nil
}

// usageHook sends the token counts in the session's Claude Code
// transcripts that earlier runs have not sent (package capture). It never
// fails, so the agent's turn does not depend on starfix, and it prints
// nothing on stdout. Outside a starfix repository, or with nothing new,
// it prints nothing at all; otherwise a failure, or a transcript in a
// format sfx does not know, is one note on stderr. No transcript text is
// ever printed.
//
// The session id follows prime --hook: STARFIX_SESSION if set, else the
// hook input's session_id, which is the CLAUDE_CODE_SESSION_ID `sfx mcp`
// claims under, so the records go to the session that held the issues.
func (r *runner) usageHook(ctx context.Context) {
	in := readHookInput(r.env.Stdin)
	if in.SessionID != "" && r.env.Getenv("STARFIX_SESSION") == "" {
		r.session = in.SessionID
	}
	if in.Cwd != "" && r.dir == "." {
		r.dir = in.Cwd
	}
	r.quiet = true
	ctx, cancel := context.WithTimeout(ctx, usageTimeout(in.HookEventName))
	defer cancel()
	res, err := r.captureClaude(ctx, in)
	if errors.Is(err, client.ErrNoConfig) {
		return // not a starfix repository: nothing to say
	}
	var note string
	switch {
	case err != nil:
		note = usageFailure(err)
	case res.Refused > 0:
		msg := res.Refusal.Error()
		if pe, ok := errors.AsType[*proto.Error](res.Refusal); ok {
			msg = pe.Message
		}
		note = "starfix: " + strconv.Itoa(res.Refused) + " token usage record" + plural(res.Refused) + " refused and dropped: " +
			strconv.Quote(msg) + "; fix: \"check this machine's clock against the server's; dropped records are not sent again\""
	case res.Unrecognized:
		v := "an unknown version"
		if res.Version != "" {
			v = res.Version
		}
		note = "starfix: token usage not sent: transcript format not recognized (Claude Code " + v + "); " +
			"fix: run `sfx upgrade`; if that is the latest, report the Claude Code version"
	default:
		return
	}
	_, _ = r.env.Stderr.Write([]byte(note + "\n"))
}

// captureClaude reads the session's transcripts and sends what is new.
func (r *runner) captureClaude(ctx context.Context, in hookInput) (capture.Result, error) {
	if _, err := client.LoadConfig(r.dir); err != nil {
		if _, ok := errors.AsType[*proto.Error](err); ok {
			return capture.Result{}, err
		}
		return capture.Result{}, proto.Errf(proto.CodeInvalid, "correct "+client.ConfigFile+"; see docs/cli.md, Connect a repository", err.Error())
	}
	if in.SessionID == "" || in.TranscriptPath == "" {
		return capture.Result{}, proto.Errf(proto.CodeInvalid, "run it from the agent's hooks: `sfx setup claude-code --write` installs them",
			"no session_id and transcript_path in the hook input")
	}
	cache, err := r.env.UserCacheDir()
	if err != nil {
		return capture.Result{}, proto.Errf(proto.CodeInvalid, "set HOME (XDG_CACHE_HOME on Linux, LocalAppData on Windows) so sfx can keep its offsets",
			"cannot find the cache directory: "+err.Error())
	}
	files, err := capture.ClaudeFiles(in.TranscriptPath, in.SessionID, in.AgentTranscriptPath)
	// At SessionEnd the session has stopped writing, so its last
	// responses are finished; at SubagentStop, the subagent's are.
	var done []string
	if in.HookEventName == "SubagentStop" && filepath.IsAbs(in.AgentTranscriptPath) {
		done = []string{filepath.Clean(in.AgentTranscriptPath)}
	}
	res, cerr := capture.Claude(ctx, capture.Input{Session: in.SessionID, Files: files, StateDir: filepath.Join(cache, "starfix"),
		Send: r.sendUsage, Final: in.HookEventName == "SessionEnd", Done: done})
	return res, errors.Join(err, cerr)
}

// plural is "s" unless n is 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// sendUsage sends one batch with the usage op.
func (r *runner) sendUsage(ctx context.Context, recs []proto.UsageRecord) error {
	var out proto.UsageResult
	return r.call(ctx, proto.OpUsage, proto.UsageArgs{Records: recs}, &out)
}

// usageFailure is the hook's one-line note on a failed capture: the
// first refusal's message and fix, quoted, since they may be the
// server's, else the error and a fix to check the files involved.
func usageFailure(err error) string {
	msg, fix := err.Error(), "check that the transcript is readable and sfx's cache directory writable, then let the next hook retry"
	if pe, ok := errors.AsType[*proto.Error](err); ok {
		msg, fix = pe.Message, pe.Fix
	}
	text := "starfix: token usage not sent: " + strconv.Quote(msg)
	if fix != "" {
		text += "; fix: " + strconv.Quote(fix)
	}
	return text
}
