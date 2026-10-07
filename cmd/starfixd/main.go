// Command starfixd is the starfix server.
//
//	starfixd serve [--dev] [--config FILE] [--socket PATH] [--project UUID] [--prefix P]
//	starfixd stdio --principal NAME [--socket PATH]
//	starfixd import-bd [--dry-run] [--json] FILE|-
//	starfixd export-bd [-o FILE]
//	starfixd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]
//	starfixd version
//
// serve is the long-running daemon: it owns the Dolt store and serves the
// protocol on a unix socket. stdio is the forced command in each developer
// key's authorized_keys line; it bridges the SSH session to that socket.
// import-bd and export-bd are admin commands run on the server: they open
// the store directly with the daemon's DSN settings and move a backlog in
// and out as bd's JSONL. upgrade replaces this binary with a verified
// release, after tagging the database, and restarts and health-checks the
// daemon when a systemd unit is configured or --restart is given.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
	"github.com/ariesworx/starfix/internal/version"
)

const usage = `usage:
  starfixd serve [--dev] [--config FILE] [--dsn DSN] [--socket PATH] [--project UUID] [--prefix P]
  starfixd stdio --principal NAME [--config FILE] [--socket PATH]
  starfixd import-bd [--config FILE] [--dsn DSN] [--principal NAME] [--dry-run] [--json] FILE|-
  starfixd export-bd [--config FILE] [--dsn DSN] [-o FILE]
  starfixd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart] [--config FILE] [--dsn DSN] [--socket PATH]
  starfixd version

The database DSN comes from --dsn (no password allowed there), $STARFIXD_DSN,
or dsn: in the config file (default /etc/starfix/starfixd.yaml, mode 0600).`

type usageError string

func (e usageError) Error() string { return string(e) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	if err == nil {
		os.Exit(0)
	}
	// A bare exitError means the command already said what went wrong.
	if code, ok := err.(exitError); ok { //nolint:errorlint // only the unwrapped value is silent
		os.Exit(int(code))
	}
	fmt.Fprintf(os.Stderr, "starfixd: %v\n", err)
	var ue usageError
	if errors.As(err, &ue) {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	os.Exit(1)
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usageError("no command")
	}
	switch args[0] {
	case "serve":
		return serve(ctx, args[1:])
	case "stdio":
		return stdio(ctx, args[1:])
	case "import-bd":
		return importBD(ctx, osEnv(), args[1:])
	case "export-bd":
		return exportBD(ctx, osEnv(), args[1:])
	case "upgrade":
		return upgradeCmd(ctx, osEnv(), osUpgradeDeps(), args[1:])
	case "version", "--version":
		fmt.Printf("starfixd %s (protocol %d-%d)\n", version.Version, proto.ProtoMin, proto.ProtoMax)
		return nil
	case "help", "-h", "--help":
		fmt.Println(usage)
		return nil
	}
	return usageError(fmt.Sprintf("unknown command %q", args[0]))
}

func flags(name string, args []string, s *server.Settings, cfg *string, extra func(*flag.FlagSet)) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(cfg, "config", "", "config file")
	fs.StringVar(&s.Socket, "socket", "", "unix socket path")
	extra(fs)
	if err := fs.Parse(args); err != nil {
		return usageError(err.Error())
	}
	if fs.NArg() > 0 {
		return usageError(fmt.Sprintf("%s takes no arguments", name))
	}
	return nil
}

func serve(ctx context.Context, args []string) error {
	var fl server.Settings
	var cfgPath string
	var dev bool
	if err := flags("serve", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.BoolVar(&dev, "dev", false, "allow serving off Linux, where socket peers are not checked")
		fs.StringVar(&fl.DSN, "dsn", "", "Dolt DSN (no password)")
		fs.StringVar(&fl.Project, "project", "", "project UUID")
		fs.StringVar(&fl.Prefix, "prefix", "", "issue-ID prefix")
	}); err != nil {
		return err
	}
	if err := server.RequirePeerCheck(dev); err != nil {
		return err
	}
	s, err := server.ResolveSettings(fl, cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	switch {
	case s.DSN == "":
		return errors.New("no database DSN; fix: set $STARFIXD_DSN or dsn: in the config file")
	case s.Project == "":
		return errors.New("no project; fix: set --project, $STARFIXD_PROJECT or project: in the config file")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	st, err := store.Open(ctx, s.DSN, store.Options{Prefix: s.Prefix, Logger: log})
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	srv, err := server.New(server.Config{Store: st, Project: s.Project, Version: version.Version,
		Latest: s.Latest, Logger: log})
	if err != nil {
		return errors.Join(err, st.Close())
	}
	l, err := server.Listen(s.Socket)
	if err != nil {
		return errors.Join(err, st.Close())
	}
	log.Info("starfixd serving", "version", version.Version, "socket", s.Socket, "project", s.Project)
	err = srv.Serve(ctx, l)
	return errors.Join(err, st.Close())
}

func stdio(ctx context.Context, args []string) error {
	var fl server.Settings
	var cfgPath, principal string
	if err := flags("stdio", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&principal, "principal", "", "principal this key authenticates")
	}); err != nil {
		return err
	}
	if principal == "" {
		return usageError("stdio needs --principal")
	}
	s, err := server.ResolveSettings(fl, cfgPath, os.Getenv)
	if err != nil {
		return err
	}
	return server.Bridge(ctx, s.Socket, principal, os.Stdin, os.Stdout)
}
