package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ariesworx/starfix/internal/bdimport"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/store"
)

// exitError ends the program with a code after the command has already
// printed what went wrong.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit %d", int(e)) }

// adminEnv is what the admin commands read and write, so tests can run
// them in process.
type adminEnv struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	getenv         func(string) string
}

// osEnv is the adminEnv of this process.
func osEnv() adminEnv {
	return adminEnv{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv}
}

// openAdminStore resolves the daemon's settings and opens its store
// directly, as the admin commands run on the server beside the daemon.
func openAdminStore(ctx context.Context, env adminEnv, fl server.Settings, cfgPath string) (*store.Store, error) {
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if err != nil {
		return nil, err
	}
	if s.DSN == "" {
		return nil, errors.New("no database DSN; fix: set $STARFIXD_DSN or dsn: in the config file")
	}
	// No background committer: Close makes the one Dolt commit.
	st, err := store.Open(ctx, s.DSN, store.Options{Prefix: s.Prefix, CommitInterval: -1, AllowUnsafeAccount: s.AllowUnsafeDolt,
		Limits: s.Limits.Limits})
	if _, ok := errors.AsType[*store.UnsafeAccountError](err); ok {
		return nil, fmt.Errorf("open store: %w", err) // it names its own fix
	}
	if err != nil {
		return nil, fmt.Errorf("open store: %w; fix: check the DSN and that dolt sql-server is running", err)
	}
	return st, nil
}

// adminFlags parses an admin command's flags: --config, --dsn, --dev,
// --allow-unsafe-dolt, and those extra registers. It returns the FlagSet,
// for the positional arguments, and refuses --allow-unsafe-dolt without
// --dev.
func adminFlags(name string, args []string, fl *server.Settings, cfgPath *string, extra func(*flag.FlagSet)) (*flag.FlagSet, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(cfgPath, "config", "", "config file")
	fs.StringVar(&fl.DSN, "dsn", "", "Dolt DSN (no password)")
	var dev bool
	devFlags(fs, &dev, &fl.AllowUnsafeDolt)
	extra(fs)
	if err := fs.Parse(args); err != nil {
		return nil, usageError(err.Error())
	}
	return fs, checkDev(dev, fl.AllowUnsafeDolt)
}

// devFlags registers --dev and --allow-unsafe-dolt.
func devFlags(fs *flag.FlagSet, dev, unsafe *bool) {
	fs.BoolVar(dev, "dev", false, "development mode")
	fs.BoolVar(unsafe, "allow-unsafe-dolt", false, "open the store even as root or with secure_file_priv empty (needs --dev)")
}

// checkDev refuses --allow-unsafe-dolt without --dev (decision D3): an
// unsafe Dolt account is for a developer's own machine only.
func checkDev(dev, unsafe bool) error {
	if unsafe && !dev {
		return usageError("--allow-unsafe-dolt is for development only and needs --dev; on a server, give starfixd a least-privileged Dolt account (README, quick start)")
	}
	return nil
}

// closeStore closes st, keeping err as it is when Close succeeds so that a
// bare exitError stays bare.
func closeStore(err error, st *store.Store) error {
	if cerr := st.Close(); cerr != nil {
		return errors.Join(err, fmt.Errorf("close store: %w", cerr))
	}
	return err
}

// importBD runs `starfixd import-bd [flags] FILE|-`.
func importBD(ctx context.Context, env adminEnv, args []string) (err error) {
	var fl server.Settings
	var cfgPath, principal string
	var dryRun, asJSON bool
	fs, err := adminFlags("import-bd", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&principal, "principal", "import", "principal recorded on the import's events")
		fs.BoolVar(&dryRun, "dry-run", false, "report what would change; write nothing")
		fs.BoolVar(&asJSON, "json", false, "print one JSON document")
	})
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("import-bd takes one file (or - for stdin)")
	}
	name := fs.Arg(0)
	in := env.stdin
	if name != "-" {
		f, err := os.Open(name) //nolint:gosec // the admin names the file
		if err != nil {
			return fmt.Errorf("%w; fix: give the path of bd's export (bd export -o FILE)", err)
		}
		defer func() { _ = f.Close() }()
		in = f
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "server"
	}
	actor := store.Actor{Principal: principal, Session: "import-bd-" + time.Now().UTC().Format("20060102T150405Z"), Machine: host}

	st, err := openAdminStore(ctx, env, fl, cfgPath)
	if err != nil {
		return err
	}
	defer func() { err = closeStore(err, st) }()

	rep, ierr := bdimport.Import(ctx, st, in, bdimport.Options{Actor: actor, DryRun: dryRun})
	if ierr != nil {
		return fmt.Errorf("import %s stopped: %w; fix: correct the cause and run it again (it is safe to rerun)", filepath.Base(name), ierr)
	}
	if asJSON {
		doc := struct {
			Summary string `json:"summary"`
			*bdimport.Report
		}{rep.Summary(), rep}
		enc := json.NewEncoder(env.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
	} else {
		for _, p := range rep.Problems {
			_, _ = fmt.Fprintln(env.stderr, p.Text())
		}
		_, _ = fmt.Fprintln(env.stdout, rep.Summary())
	}
	if rep.Errors() > 0 {
		return exitError(1)
	}
	return nil
}

// exportBD runs `starfixd export-bd [flags] [-o FILE]`.
func exportBD(ctx context.Context, env adminEnv, args []string) (err error) {
	var fl server.Settings
	var cfgPath, out string
	fs, err := adminFlags("export-bd", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&out, "o", "", "write to FILE instead of stdout")
	})
	if err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError("export-bd takes no arguments; use -o FILE")
	}
	st, err := openAdminStore(ctx, env, fl, cfgPath)
	if err != nil {
		return err
	}
	defer func() { err = closeStore(err, st) }()

	w := env.stdout
	var f *os.File
	if out != "" {
		// Write beside the target and rename, so a failed export never
		// leaves a truncated file behind.
		f, err = os.CreateTemp(filepath.Dir(out), ".export-bd-*")
		if err != nil {
			return fmt.Errorf("%w; fix: choose a writable directory for -o", err)
		}
		defer func() {
			if err != nil {
				_ = f.Close()
				_ = os.Remove(f.Name())
			}
		}()
		w = f
	}
	n, err := bdimport.Export(ctx, st, w)
	if err != nil {
		return fmt.Errorf("export: %w; fix: check the store is reachable and run it again", err)
	}
	if f != nil {
		if err = f.Close(); err != nil {
			return fmt.Errorf("export: %w", err)
		}
		if err = os.Rename(f.Name(), out); err != nil {
			return fmt.Errorf("export: %w", err)
		}
		_, _ = fmt.Fprintf(env.stderr, "exported %d issues to %s\n", n, out)
	}
	return nil
}
