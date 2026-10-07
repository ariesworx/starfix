// Package cli is the starfix command line: issue CRUD for people, over the
// client package. It writes only to the writers it is given.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/proto"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
	// ExitVersion: the server refused this client's protocol version.
	ExitVersion = 3
)

// Env is the process environment Run works in.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Getenv         func(string) string
	Hostname       func() (string, error)
	// Version is this binary's build version.
	Version string
}

// usageError is a mistake on the command line.
type usageError struct{ msg, usage string }

func (e *usageError) Error() string { return e.msg }

func usagef(usage, format string, a ...any) error {
	return &usageError{msg: fmt.Sprintf(format, a...), usage: usage}
}

type command struct {
	name    string
	usage   string
	summary string
	run     func(ctx context.Context, r *runner, args []string) error
}

var commands []command

func init() {
	commands = []command{
		{"create", "create TITLE... [-p N] [-t TYPE] [--body TEXT|-] [--parent ID] [--label L]...", "create an issue and print its id", cmdCreate},
		{"show", "show ID [--compact]", "show an issue and its dependencies", cmdShow},
		{"list", "list [--status S,S] [--all] [-t TYPE] [-p N,N] [--assignee A] [--parent ID] [--label L]... [-n N] [--cursor C]", "list issues (open ones unless --status or --all)", cmdList},
		{"ready", "ready [-n N]", "list issues nothing holds back", cmdReady},
		{"blocked", "blocked [-n N]", "list issues held back by open blockers", cmdBlocked},
		{"update", "update ID [--rev N] [--title T] [--body TEXT|-] [--status S] [-p N] [-t TYPE] [--assignee A] [--parent ID] ...", "change an issue's fields", cmdUpdate},
		{"close", "close ID [--reason TEXT] [--rev N]", "close an issue", cmdClose},
		{"reopen", "reopen ID [--rev N]", "reopen a closed issue", cmdReopen},
		{"dep", "dep add|rm FROM TO [--type T]", "add or remove a dependency: FROM depends on TO", cmdDep},
		{"label", "label add|rm ID LABEL...", "add or remove labels", cmdLabel},
		{"comment", "comment ID TEXT...|-", "add a comment", cmdComment},
		{"comments", "comments ID", "list an issue's comments", cmdComments},
		{"history", "history ID", "list an issue's changes", cmdHistory},
		{"version", "version", "print the starfix version", cmdVersion},
	}
}

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
	env  Env
	json bool
	dir  string
	conn *client.Conn
}

// Run executes one starfix command line (without the program name) and
// returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Hostname == nil {
		env.Hostname = os.Hostname
	}
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	r := &runner{env: env, dir: "."}
	args, err := r.globals(args)
	if err != nil {
		return r.fail(err)
	}
	if len(args) == 0 || args[0] == "help" {
		if len(args) > 1 {
			if c := lookup(args[1]); c != nil {
				_, _ = fmt.Fprintf(env.Stdout, "usage: starfix %s\n\n%s\n", c.usage, c.summary)
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
				return nil, usagef("starfix [-C DIR] [--json] COMMAND", "-C needs a directory")
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
			return nil, usagef("starfix [-C DIR] [--json] COMMAND", "unknown flag %s", a)
		default:
			return args, nil
		}
	}
	return args, nil
}

func (r *runner) help(w io.Writer) {
	_, _ = fmt.Fprintln(w, "usage: starfix [-C DIR] [--json] COMMAND [ARGS]")
	_, _ = fmt.Fprintln(w)
	for _, c := range commands {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintln(w, "`starfix help COMMAND` shows a command's usage. --json prints one JSON document.")
}

// fail reports err and returns its exit code. Every failure ends with a
// fix: line.
func (r *runner) fail(err error) int {
	code, exit := proto.CodeUnavailable, ExitFailure
	msg, fix := err.Error(), "retry; if it persists, run with -C pointing at the repository and check "+client.ConfigFile
	var ue *usageError
	var pe *proto.Error
	switch {
	case errors.As(err, &ue):
		code, exit, msg = proto.CodeInvalid, ExitUsage, ue.msg
		fix = "run `starfix help` for the commands"
		if ue.usage != "" {
			fix = "usage: starfix " + ue.usage
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
		_, _ = fmt.Fprintf(r.env.Stderr, "starfix: %s\n", msg)
		if fix != "" {
			_, _ = fmt.Fprintf(r.env.Stderr, "fix: %s\n", fix)
		}
	}
	return exit
}

// emit writes v as one JSON document.
func (r *runner) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		_, _ = fmt.Fprintf(r.env.Stderr, "starfix: encode output: %v\n", err)
		return
	}
	_, _ = fmt.Fprintf(r.env.Stdout, "%s\n", b)
}

// connect dials the server on first use.
func (r *runner) connect(ctx context.Context) (*client.Conn, error) {
	if r.conn != nil {
		return r.conn, nil
	}
	cfg, err := client.LoadConfig(r.dir)
	if err != nil {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return nil, err
		}
		return nil, proto.Errf(proto.CodeInvalid, "correct "+client.ConfigFile+"; see the README's quick start", err.Error())
	}
	host, err := r.env.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	c, err := client.Dial(ctx, cfg, client.Options{Version: r.env.Version, Session: client.SessionFromEnv(r.env.Getenv),
		Machine: host, Getenv: r.env.Getenv})
	if err != nil {
		return nil, err
	}
	if w := c.Warning(r.env.Version); w != "" {
		_, _ = fmt.Fprintf(r.env.Stderr, "starfix: %s\n", w)
	}
	r.conn = c
	return c, nil
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
