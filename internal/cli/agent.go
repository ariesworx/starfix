package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

func cmdPrime(ctx context.Context, r *runner, args []string) error {
	const usage = "prime [--hook]"
	fs := r.newFlags("prime")
	hook := fs.Bool("hook", false, "run as a SessionStart hook: JSON for the harness, and never fail")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "prime takes no arguments")
	}
	if *hook {
		r.primeHook(ctx)
		return nil
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	p, err := mcpserver.BuildPrime(ctx, c, r.env.Version)
	if err != nil {
		return err
	}
	if r.json {
		r.emit(p)
		return nil
	}
	_, _ = io.WriteString(r.env.Stdout, p.Text())
	return nil
}

// hookTimeout bounds `prime --hook`, well inside the harness's own hook
// timeout, so a slow server costs the session a note, not its start.
const hookTimeout = 10 * time.Second

// hookInput is the part of a SessionStart hook's stdin that prime uses.
type hookInput struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
}

// primeHook prints prime as a Claude Code SessionStart hook's JSON
// output, whose additionalContext the harness adds to the session. It
// never fails: the agent's start must not depend on starfix. Outside a
// starfix repository it prints nothing; on any other error the context
// is a one-line note saying what failed and the fix.
//
// The session id comes from STARFIX_SESSION, else the hook input's
// session_id, else the environment, so the connection prime opens is
// recorded under the harness's session. Stage 3's agents registry will
// make that a registration; until then the server records it per event.
func (r *runner) primeHook(ctx context.Context) {
	in := readHookInput(r.env.Stdin)
	if in.SessionID != "" && r.env.Getenv("STARFIX_SESSION") == "" {
		r.session = in.SessionID
	}
	if in.Cwd != "" && r.dir == "." {
		r.dir = in.Cwd
	}
	ctx, cancel := context.WithTimeout(ctx, hookTimeout)
	defer cancel()
	text, err := r.primeText(ctx)
	if errors.Is(err, client.ErrNoConfig) {
		return // not a starfix repository: nothing to say
	}
	if err != nil {
		msg, fix := err.Error(), ""
		var pe *proto.Error
		if errors.As(err, &pe) {
			msg, fix = pe.Message, pe.Fix
		}
		text = "starfix: prime failed: " + msg
		if fix != "" {
			text += "; fix: " + fix
		}
		text = strings.ReplaceAll(text, "\n", " ") + "\n"
	}
	r.emit(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName": "SessionStart", "additionalContext": text}})
}

func (r *runner) primeText(ctx context.Context) (string, error) {
	c, err := r.connect(ctx)
	if err != nil {
		return "", err
	}
	p, err := mcpserver.BuildPrime(ctx, c, r.env.Version)
	if err != nil {
		return "", err
	}
	return p.Text(), nil
}

// readHookInput reads the hook's JSON from stdin. A terminal is not read,
// so a person trying the hook by hand is not left waiting; input that is
// not hook JSON is ignored.
func readHookInput(stdin io.Reader) hookInput {
	var in hookInput
	if f, ok := stdin.(*os.File); ok {
		if st, err := f.Stat(); err != nil || st.Mode()&os.ModeCharDevice != 0 {
			return in
		}
	}
	b, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if err == nil {
		_ = json.Unmarshal(b, &in) // not hook JSON: no session id or cwd from it
	}
	return in
}

// cmdMCP serves MCP on stdin and stdout until the agent disconnects. It
// connects to starfixd on the first tool call, not at start, so a broken
// config shows up as a tool error the agent can relay rather than as a
// server that failed to start.
func cmdMCP(ctx context.Context, r *runner, args []string) error {
	const usage = "mcp"
	fs := r.newFlags("mcp")
	pos, err := parse(fs, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef(usage, "mcp takes no arguments")
	}
	opts := r.clientOptions()
	if opts.Session == "" {
		// No harness session id: pick one for this process, so a reconnect
		// keeps the session's history under one id.
		opts.Session = client.NewSessionID()
	}
	srv := mcpserver.New(mcpserver.Options{Version: r.env.Version,
		Dial: func(ctx context.Context) (mcpserver.Conn, error) {
			return mcpserver.DialRepo(ctx, r.dir, opts)
		}})
	return srv.Serve(ctx, io.NopCloser(r.env.Stdin), nopWriteCloser{r.env.Stdout})
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
