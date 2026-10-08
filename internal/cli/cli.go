package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/safetext"
)

// Exit codes, as [Run] returns them.
const (
	// ExitOK: the command succeeded.
	ExitOK = 0
	// ExitFailure: the command failed, or the server refused it.
	ExitFailure = 1
	// ExitUsage: a mistake on the command line. Bare sfx prints help and
	// exits with it too.
	ExitUsage = 2
	// ExitVersion: this client's protocol version is outside the server's
	// range.
	ExitVersion = 3
)

// Env is the process environment Run works in: everything the package
// takes from the process. A field left nil or empty takes the default its
// comment names; Stdout and Stderr must be set.
type Env struct {
	// Stdin is read for a text given as "-", by prime --hook, usage
	// --hook and sfx mcp. Default empty.
	Stdin io.Reader
	// Stdout and Stderr receive all output.
	Stdout, Stderr io.Writer
	// Getenv reads an environment variable. Default os.Getenv.
	Getenv func(string) string
	// Hostname names this machine to the server. Default os.Hostname.
	Hostname func() (string, error)
	// UserHomeDir finds the home directory, for `setup --global` and a
	// desktop app's config. Default os.UserHomeDir.
	UserHomeDir func() (string, error)
	// UserCacheDir finds the cache directory, where `usage --hook` keeps
	// its transcript offsets. Default os.UserCacheDir.
	UserCacheDir func() (string, error)
	// GOOS and Executable are used only by `setup claude-desktop`: the
	// platform, and the absolute path of this sfx. Defaults runtime.GOOS,
	// and sfx's entry on PATH when that is this program, else
	// os.Executable.
	GOOS       string
	Executable func() (string, error)
	// Version is this binary's build version.
	Version string
	// Upgrader overrides what `upgrade` uses. Nil uses GitHub releases,
	// the built-in release keys and this executable.
	Upgrader *Upgrader
}

// usageError is a mistake on the command line. usage is the usage line
// its fix shows; Run fills in the command's own when it is empty.
type usageError struct{ msg, usage string }

func (e *usageError) Error() string { return e.msg }

// usagef returns a usageError with a formatted message.
func usagef(usage, format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...), usage: usage}
}

// command is one sfx command: its name, its usage line without "sfx ",
// the summary help prints, and the function that runs it.
type command struct {
	name    string
	usage   string
	summary string
	run     func(ctx context.Context, r *runner, args []string) error
}

// commands lists the commands in the order help prints them.
var commands []command

func init() {
	commands = []command{
		{"create", "create TITLE... [-p N] [-t TYPE] [--body TEXT|-] [--parent ID] [--label L]...", "create an issue and print its id", cmdCreate},
		{"show", "show ID [--compact]", "show an issue and its dependencies", cmdShow},
		{"list", "list [--status S,S] [--all] [-t TYPE] [-p N,N] [--assignee A] [--parent ID] [--label L]... [-n N] [--cursor C]", "list issues (open ones unless --status or --all)", cmdList},
		{"ready", "ready [-n N]", "list issues nothing holds back", cmdReady},
		{"blocked", "blocked [-n N]", "list issues held back by open blockers", cmdBlocked},
		{"update", "update ID [--rev N] [--title T] [--body TEXT|-] [--status S] [-p N] [-t TYPE] [--assignee A] [--parent ID] ...", "change an issue's fields", cmdUpdate},
		{"close", "close ID [--reason TEXT] [--rev N] [--force]", "close an issue (--force, admins only: despite open acceptance items)", cmdClose},
		{"reopen", "reopen ID [--rev N]", "reopen a closed issue", cmdReopen},
		{"dep", "dep add|rm FROM TO [--type T]", "add or remove a dependency: FROM depends on TO", cmdDep},
		{"label", "label add|rm ID LABEL...", "add or remove labels", cmdLabel},
		{"comment", "comment ID TEXT...|-", "add a comment", cmdComment},
		{"comments", "comments ID [-n N]", "list an issue's comments", cmdComments},
		{"history", "history ID [-n N]", "list an issue's changes", cmdHistory},
		{"start", "start [ID] [--for DURATION] [--take] [--branch | --worktree DIR]", "claim an issue (the top ready one without ID) and show it", cmdStart},
		{"finish", "finish ID [--reason TEXT] [--handoff TEXT|-] [--state S] [--next TEXT] [--branch B] [--worktree DIR] [--to P] [--discovered TITLE]... [--tick N,N] [--waive N=REASON]... [--epoch N]", "close your issue with a handoff note and discovered work", cmdFinish},
		{"accept", "accept ID N... [--undo | --waive REASON]", "tick acceptance items; --undo unticks, --waive waives them for a reason", cmdAccept},
		{"handoff", "handoff ID NOTE...|- [--state S] [--next TEXT] [--branch B] [--worktree DIR] [--to P] [--release] [--epoch N]", "leave a note for whoever continues; --release lets it go", cmdHandoff},
		{"inbox", "inbox [--all] [-n N] [--ack ID]... [--ack-all]", "list your unread lost claims, handoffs, mentions and assignments; --ack marks read", cmdInbox},
		{"watch", "watch", "print inbox items as they arrive, until interrupted", cmdWatch},
		{"tui", "tui", "show a live board: ready, held and blocked issues and recent events, until q", cmdTUI},
		{"away", "away DURATION", "extend all your claims, e.g. before going offline (1m to 7d)", cmdAway},
		{"digest", "digest [--since 24h|7d|DATE|TIME] [--by PRINCIPAL] [--label L]", "summarize what closed, started, stalled, is blocked and was handed off", cmdDigest},
		{"who", "who [--since DURATION] [-n N]", "list the agents at work and the issues each holds (seen in the last 5m)", cmdWho},
		{"prime", "prime [--hook[=AGENT]]", "orient a session: your in-progress issues, top ready work, notices", cmdPrime},
		{"usage", "usage --hook[=AGENT]", "send the session's token counts from the agent's transcript (run by its hooks)", cmdUsage},
		{"mcp", "mcp", "serve the MCP tools for an agent on stdin and stdout", cmdMCP},
		{"setup", "setup AGENT|--all [--write|--check|--remove] [--global] [--command PATH]", "set agents up: MCP config, instruction pointer, session hook", cmdSetup},
		{"upgrade", "upgrade [--check] [--rollback]", "replace sfx with the latest verified release; --rollback undoes it", cmdUpgrade},
		{"version", "version", "print the starfix version", cmdVersion},
	}
}

// lookup returns the command named name, or nil.
func lookup(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

// runner carries one invocation's settings and its lazily opened
// connection.
type runner struct {
	// env is Run's Env, with defaults filled in and, once wrapOutput has
	// run, Stdout and Stderr escaped.
	env Env
	// json is --json, given before or after the command name.
	json bool
	// dir is the directory commands run in, where the search for
	// .starfix.yaml starts: -C's, else the hook input's cwd for prime
	// --hook, else ".".
	dir string
	// conn is the connection connect opened, if any; Run closes it.
	conn *mcpserver.RepoConn
	// session overrides the environment's session id (prime and usage
	// --hook).
	session string
	// quiet drops the server's version warning: a hook's stderr carries
	// one note at most (usage --hook).
	quiet bool
	// onPush, if set before connecting, takes the events the server
	// pushes (sfx watch).
	onPush func(proto.Push)
	// rawOut is standard output before wrapOutput, for JSON; outs are the
	// escaping writers wrapOutput put in place.
	rawOut io.Writer
	outs   []*safetext.Writer
}

// wrapOutput routes everything printed through safetext writers. Issue
// text, names, server messages and server stderr come from other people
// and from the server; a control or bidi character in them is shown as an
// escape (\x1b, \u202e), so it cannot clear the screen, set the clipboard,
// hide a link or rewrite a line (C-3). Printers also escape single-line
// fields with safetext.Line, so a newline in one cannot start a line of
// its own. JSON goes to the raw output, escaped by emit.
func (r *runner) wrapOutput() {
	r.rawOut = r.env.Stdout
	out, errw := safetext.NewWriter(r.env.Stdout), safetext.NewWriter(r.env.Stderr)
	r.env.Stdout, r.env.Stderr, r.outs = out, errw, []*safetext.Writer{out, errw}
}

// mcpOut is standard output for a protocol, not for text: sfx mcp's
// JSON-RPC, which encoding/json already escapes.
func (r *runner) mcpOut() io.Writer {
	if r.rawOut != nil {
		return r.rawOut
	}
	return r.env.Stdout
}

// esc is safetext.Line, for a single-line field in text output: a title,
// a name, a label, a reason, an id from the server.
func esc(s string) string { return safetext.Line(s) }

// escAll is esc on each of xs.
func escAll(xs []string) []string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = esc(x)
	}
	return out
}

// flush writes out anything the escaping writers hold.
func (r *runner) flush() {
	for _, w := range r.outs {
		_ = w.Flush()
	}
}

// Run executes one sfx command line, without the program name, and
// returns its exit code: [ExitUsage] for a mistake on the command line or
// a bare sfx, which prints help; [ExitVersion] when this client's protocol
// version is outside the server's range; [ExitFailure] for any other
// failure. It prints a failure on Stderr as an `sfx:` line and, when the
// error has one, a `fix:` line, or with --json as one JSON document on
// Stdout. Run keeps no state between calls.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Hostname == nil {
		env.Hostname = os.Hostname
	}
	if env.UserHomeDir == nil {
		env.UserHomeDir = os.UserHomeDir
	}
	if env.UserCacheDir == nil {
		env.UserCacheDir = os.UserCacheDir
	}
	if env.GOOS == "" {
		env.GOOS = runtime.GOOS
	}
	if env.Executable == nil {
		env.Executable = sfxPath
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	r := &runner{env: env, dir: "."}
	r.wrapOutput()
	defer r.flush()
	args, err := r.globals(args)
	if err != nil {
		return r.fail(err)
	}
	if len(args) == 0 || args[0] == "help" {
		if len(args) > 1 {
			if c := lookup(args[1]); c != nil {
				_, _ = fmt.Fprintf(env.Stdout, "usage: sfx %s\n\n%s\n", c.usage, c.summary)
				return ExitOK
			}
		}
		r.help(env.Stdout)
		if len(args) == 0 {
			return ExitUsage
		}
		return ExitOK
	}
	c := lookup(args[0])
	if c == nil {
		return r.fail(usagef("", "unknown command %q", args[0]))
	}
	err = c.run(ctx, r, args[1:])
	if r.conn != nil {
		_ = r.conn.Close()
	}
	if err != nil {
		var ue *usageError
		if errors.As(err, &ue) && ue.usage == "" {
			ue.usage = c.usage
		}
		return r.fail(err)
	}
	return ExitOK
}

// globals consumes -C DIR, --json and -h before the command name.
func (r *runner) globals(args []string) ([]string, error) {
	for len(args) > 0 {
		a := args[0]
		switch {
		case a == "-C" || a == "--C":
			if len(args) < 2 {
				return nil, usagef("sfx [-C DIR] [--json] COMMAND", "-C needs a directory")
			}
			r.dir, args = args[1], args[2:]
		case strings.HasPrefix(a, "-C="):
			r.dir, args = strings.TrimPrefix(a, "-C="), args[1:]
		case a == "--json" || a == "-json":
			r.json, args = true, args[1:]
		case a == "-h" || a == "--help" || a == "-help":
			return []string{"help"}, nil
		case a == "--version" || a == "-version":
			return []string{"version"}, nil
		case strings.HasPrefix(a, "-"):
			return nil, usagef("sfx [-C DIR] [--json] COMMAND", "unknown flag %s", a)
		default:
			return args, nil
		}
	}
	return args, nil
}

// help prints the list of commands to w.
func (r *runner) help(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: sfx [-C DIR] [--json] COMMAND [ARGS]")
	_, _ = fmt.Fprintln(w)
	for _, c := range commands {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "`sfx help COMMAND` shows a command's usage. --json prints one JSON document.")
}

// fail reports err and returns its exit code. A usage error's fix is the
// command's usage line; a *proto.Error keeps its code, message and fix,
// and prints no fix: line when the fix is empty; any other error is
// reported as unavailable, with a fix to retry and check the config.
func (r *runner) fail(err error) int {
	code, exit := proto.CodeUnavailable, ExitFailure
	msg, fix := err.Error(), "retry; if it persists, run with -C pointing at the repository and check "+client.ConfigFile
	var ue *usageError
	var pe *proto.Error
	switch {
	case errors.As(err, &ue):
		code, exit, msg = proto.CodeInvalid, ExitUsage, ue.msg
		fix = "run `sfx help` for the commands"
		if ue.usage != "" {
			fix = "usage: sfx " + ue.usage
		}
	case errors.As(err, &pe):
		code, msg, fix = pe.Code, pe.Message, pe.Fix
		if pe.Code == proto.CodeVersion {
			exit = ExitVersion
		}
	}
	if r.json {
		r.emit(map[string]*proto.Error{"error": {Code: code, Message: msg, Fix: fix}})
	} else {
		// The message and fix may be the server's: one line each.
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\n", safetext.Line(msg))
		if fix != "" {
			_, _ = fmt.Fprintf(r.env.Stderr, "fix: %s\n", safetext.Line(fix))
		}
	}
	return exit
}

// emit writes v as one JSON document. encoding/json escapes the C0
// controls; safetext.JSON escapes DEL, C1 and bidi characters as \uXXXX
// too, so the document is the same value and safe on a terminal.
func (r *runner) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: encode output: %v\n", err)
		return
	}
	out := r.rawOut
	if out == nil {
		out = r.env.Stdout
	}
	_, _ = fmt.Fprintf(out, "%s\n", safetext.JSON(b))
}

// connect dials the server on first use, and prints the server's version
// warning, if it has one, on Stderr.
func (r *runner) connect(ctx context.Context) (*mcpserver.RepoConn, error) {
	if r.conn != nil {
		return r.conn, nil
	}
	c, err := mcpserver.DialRepo(ctx, r.dir, r.clientOptions())
	if err != nil {
		return nil, err
	}
	if w := c.Warning(r.env.Version); w != "" && !r.quiet {
		_, _ = fmt.Fprintf(r.env.Stderr, "sfx: %s\n", esc(w))
	}
	r.conn = c
	return c, nil
}

// clientOptions describe this machine and session to the server. The
// session id is r.session (prime --hook's), else the harness's from the
// environment, else client.CLISession.
func (r *runner) clientOptions() client.Options {
	host, err := r.env.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	session := r.session
	if session == "" {
		session = client.SessionFromEnv(r.env.Getenv)
	}
	if session == "" {
		session = client.CLISession
	}
	return client.Options{Version: r.env.Version, Session: session,
		Machine: host, Harness: client.HarnessFromEnv(r.env.Getenv), Getenv: r.env.Getenv,
		OnPush: r.onPush}
}

// call runs one operation.
func (r *runner) call(ctx context.Context, op string, args, result any) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	return c.Call(ctx, op, args, result)
}

// newFlags returns a flag set that also accepts --json after the command.
func (r *runner) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&r.json, "json", r.json, "print one JSON document")
	return fs
}

// parse parses flags anywhere among the arguments; everything after "--" is
// positional.
func parse(fs *flag.FlagSet, args []string, usage string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, usagef(usage, "help requested")
			}
			return nil, usagef(usage, "%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

// set reports whether the flag was given.
func set(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

// listFlag collects a repeatable or comma-separated flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

// priority parses "1", "p1" or "P1".
func priority(s string) (int, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "P"), "p")
	if len(s) != 1 || s[0] < '0' || s[0] > '4' {
		return 0, errors.New("priority must be 0-4 (or P0-P4)")
	}
	return int(s[0] - '0'), nil
}
