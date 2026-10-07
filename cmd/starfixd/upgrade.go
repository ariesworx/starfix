package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
	"github.com/ariesworx/starfix/internal/release"
	"github.com/ariesworx/starfix/internal/server"
	"github.com/ariesworx/starfix/internal/version"
)

const releasesURL = "https://github.com/" + release.Repo + "/releases"

// upgradeDeps is everything `starfixd upgrade` reaches outside the
// process, so tests can replace each piece.
type upgradeDeps struct {
	current      string
	goos, goarch string
	euid         func() int
	releases     *release.Client
	// exe is the binary to replace; "" means this executable.
	exe string
	// check runs the staged binary and wants it to report tag.
	check func(ctx context.Context, path, tag string) error
	// backup tags the store before the swap and returns the tag made.
	backup func(ctx context.Context, env adminEnv, fl server.Settings, cfgPath, tag string) (string, error)
	// restart restarts the systemd unit.
	restart func(ctx context.Context, unit string) error
	// probe handshakes with the daemon and returns its version.
	probe func(ctx context.Context, socket, project string) (string, error)
	// healthWait is how long to keep probing after a restart, and
	// healthEvery the pause between probes.
	healthWait, healthEvery time.Duration
}

func osUpgradeDeps() upgradeDeps {
	return upgradeDeps{
		current: version.Version, goos: runtime.GOOS, goarch: runtime.GOARCH, euid: os.Geteuid,
		releases: &release.Client{Verifier: release.Ed25519{}},
		check: func(ctx context.Context, path, tag string) error {
			return release.VersionCheck(tag)(ctx, path)
		},
		backup:  backupStore,
		restart: systemctlRestart,
		probe: func(ctx context.Context, socket, project string) (string, error) {
			return server.Probe(ctx, socket, project, version.Version)
		},
		healthWait: 30 * time.Second, healthEvery: 500 * time.Millisecond,
	}
}

// upgradeCmd runs `starfixd upgrade [--check] [--to vX.Y.Z] [--rollback] [--restart]`.
func upgradeCmd(ctx context.Context, env adminEnv, d upgradeDeps, args []string) error {
	var fl server.Settings
	var cfgPath, to string
	var check, rollback, restart bool
	fs, err := adminFlags("upgrade", args, &fl, &cfgPath, func(fs *flag.FlagSet) {
		fs.StringVar(&fl.Socket, "socket", "", "unix socket path")
		fs.BoolVar(&check, "check", false, "print whether an upgrade is available; change nothing")
		fs.StringVar(&to, "to", "", "upgrade to this release instead of the latest")
		fs.BoolVar(&rollback, "rollback", false, "restore the binary the last upgrade replaced")
		fs.BoolVar(&restart, "restart", false, "restart the systemd unit (systemd_unit, default starfixd) and health-check it")
	})
	if err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return usageError("upgrade takes no arguments; use --to vX.Y.Z")
	case rollback && (check || to != ""):
		return usageError("--rollback cannot be combined with --check or --to")
	case to != "" && !release.TagPattern.MatchString(to):
		return usageError(fmt.Sprintf("--to %q is not a release version (vMAJOR.MINOR.PATCH)", to))
	}
	if d.goos != "linux" {
		return errors.New("starfixd upgrade runs on the Linux server only; fix: run it there, as the user starfixd runs as")
	}
	s, err := server.ResolveSettings(fl, cfgPath, env.getenv)
	if err != nil {
		return err
	}
	unit := s.SystemdUnit
	if restart && unit == "" {
		unit = "starfixd"
	}
	if !check && d.euid() == 0 {
		return errors.New("starfixd upgrade refuses to run as root: the new binary would be root's, and the health check " +
			"must connect as the daemon's user; fix: run it as that user (sudo -u USER starfixd upgrade)")
	}
	exe := d.exe
	if exe == "" && !check {
		if exe, err = release.Executable(); err != nil {
			return fmt.Errorf("%w; fix: run the installed starfixd, not a temporary build", err)
		}
	}
	if rollback {
		return upgradeRollback(ctx, env, d, s, unit, exe)
	}

	cur := d.current
	var rel *release.Release
	if to != "" {
		rel, err = d.releases.ByTag(ctx, to)
	} else {
		rel, err = d.releases.Latest(ctx)
	}
	if err != nil {
		if errors.Is(err, release.ErrNotFound) && to != "" {
			return fmt.Errorf("no release %s: %w; fix: pick a version listed at %s", to, err, releasesURL)
		}
		return fmt.Errorf("read the release: %w; fix: check the server can reach api.github.com (HTTPS_PROXY is honored)", err)
	}
	cmp, ok := proto.CompareVersions(cur, rel.Tag)
	ok = ok && release.TagPattern.MatchString(cur)
	if check {
		state := "up to date"
		switch {
		case !ok:
			state = "dev build; install a release to upgrade"
		case cmp < 0:
			state = "run starfixd upgrade"
		}
		_, _ = fmt.Fprintf(env.stdout, "starfixd %s, %s %s: %s\n", cur, targetWord(to), rel.Tag, state)
		return nil
	}
	switch {
	case !ok:
		return fmt.Errorf("this starfixd is a development build (%s), which never upgrades itself; fix: install a release from %s", cur, releasesURL)
	case cmp == 0:
		_, _ = fmt.Fprintf(env.stdout, "starfixd %s is up to date\n", cur)
		return nil
	case cmp > 0 && to == "":
		_, _ = fmt.Fprintf(env.stdout, "starfixd %s is up to date (newer than the latest release, %s)\n", cur, rel.Tag)
		return nil
	case cmp > 0:
		return fmt.Errorf("%s is older than the running %s, and migrations only go forward; fix: use --rollback to return to the binary the last upgrade replaced", to, cur)
	}

	bin, err := d.releases.Fetch(ctx, rel, "starfixd", d.goos, d.goarch)
	if err != nil {
		return fetchFailure(rel.Tag, err)
	}
	tag, err := d.backup(ctx, env, fl, cfgPath, "starfix-"+cur)
	if err != nil {
		return fmt.Errorf("backup before upgrade: %w; fix: nothing was changed; make sure dolt sql-server is running and retry", err)
	}
	swap := release.Swap{Exe: exe, GOOS: d.goos, Check: func(ctx context.Context, p string) error { return d.check(ctx, p, rel.Tag) }}
	if err := swap.Install(ctx, bin); err != nil {
		return swapFailure(exe, err)
	}
	if unit == "" {
		_, _ = fmt.Fprintf(env.stdout, "installed starfixd %s (was %s; backup tag %s); restart it to run the new version: sudo systemctl restart starfixd\n",
			rel.Tag, cur, tag)
		return nil
	}
	herr := restartHealthy(ctx, d, s, unit, rel.Tag)
	if herr == nil {
		_, _ = fmt.Fprintf(env.stdout, "starfixd upgraded %s -> %s (backup tag %s; %s restarted and healthy)\n", cur, rel.Tag, tag, unit)
		return nil
	}
	// Put the old binary back and restart it, then say what happened.
	if err := swap.Rollback(); err != nil {
		return fmt.Errorf("starfixd %s is unhealthy (%w), and restoring %s failed: %w; fix: copy %s over %s by hand, then restart %s",
			rel.Tag, herr, cur, err, swap.Prev(), exe, unit)
	}
	if err := restartHealthy(ctx, d, s, unit, cur); err != nil {
		return fmt.Errorf("starfixd %s is unhealthy (%w); restored %s, which is not healthy either (%w); fix: check `journalctl -u %s`; the data is tagged %s",
			rel.Tag, herr, cur, err, unit, tag)
	}
	return fmt.Errorf("starfixd %s is unhealthy (%w); restored %s, which is running; fix: check `journalctl -u %s` and report it at %s",
		rel.Tag, herr, cur, unit, releasesURL)
}

func targetWord(to string) string {
	if to != "" {
		return "target"
	}
	return "latest"
}

func upgradeRollback(ctx context.Context, env adminEnv, d upgradeDeps, s server.Settings, unit, exe string) error {
	swap := release.Swap{Exe: exe, GOOS: d.goos}
	if err := swap.Rollback(); err != nil {
		if errors.Is(err, release.ErrNoPrevious) {
			return fmt.Errorf("%w; fix: nothing to roll back (already rolled back, or never upgraded); install the release you want with --to", err)
		}
		return swapFailure(exe, err)
	}
	if unit == "" {
		_, _ = fmt.Fprintf(env.stdout, "restored the previous starfixd at %s; restart it to run it: sudo systemctl restart starfixd\n", exe)
		return nil
	}
	if err := restartHealthy(ctx, d, s, unit, ""); err != nil {
		return fmt.Errorf("restored the previous starfixd, but it is not healthy: %w; fix: check `journalctl -u %s`", err, unit)
	}
	_, _ = fmt.Fprintf(env.stdout, "restored the previous starfixd; %s restarted and healthy\n", unit)
	return nil
}

// restartHealthy restarts unit and probes the daemon until it answers as
// want ("" accepts any version) or healthWait passes.
func restartHealthy(ctx context.Context, d upgradeDeps, s server.Settings, unit, want string) error {
	if s.Project == "" {
		return errors.New("no project configured, so the daemon cannot be health-checked; fix: set project: in the config file")
	}
	if err := d.restart(ctx, unit); err != nil {
		return err
	}
	deadline := time.Now().Add(d.healthWait)
	for {
		v, err := d.probe(ctx, s.Socket, s.Project)
		switch {
		case err == nil && (want == "" || v == want):
			return nil
		case err == nil:
			err = fmt.Errorf("the daemon reports %s, want %s", v, want)
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("health check: %w", err)
		}
		t := time.NewTimer(d.healthEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// systemctlRestart restarts unit. The admin runs upgrade as the daemon's
// user, so the restart goes through sudo -n, which fails rather than
// prompting when no sudoers rule allows it.
func systemctlRestart(ctx context.Context, unit string) error {
	if !server.UnitPattern.MatchString(unit) {
		return fmt.Errorf("%q is not a systemd unit name", unit)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "restart", unit).CombinedOutput() //nolint:gosec // unit matches UnitPattern
	if err != nil {
		return fmt.Errorf("sudo -n systemctl restart %s: %w: %s; fix: allow it in sudoers (USER ALL=(root) NOPASSWD: /usr/bin/systemctl restart %s)",
			unit, err, strings.TrimSpace(string(out)), unit)
	}
	return nil
}

// backupStore makes the Dolt tag starfix-<old version> on the daemon's
// database, covering every write so far.
func backupStore(ctx context.Context, env adminEnv, fl server.Settings, cfgPath, tag string) (string, error) {
	st, err := openAdminStore(ctx, env, fl, cfgPath)
	if err != nil {
		return "", err
	}
	name, err := st.BackupTag(ctx, tag)
	return name, closeStore(err, st)
}

func fetchFailure(tag string, err error) error {
	switch {
	case errors.Is(err, release.ErrNoKey):
		return fmt.Errorf("cannot verify starfixd %s: %w; fix: nothing was changed; this build cannot verify releases, so install one by hand (README, Install)", tag, err)
	case errors.Is(err, release.ErrSignature), errors.Is(err, release.ErrMismatch):
		return fmt.Errorf("starfixd %s failed verification: %w; fix: nothing was changed; do not install it by hand, and report it at %s", tag, err, releasesURL)
	case errors.Is(err, release.ErrNotFound):
		return fmt.Errorf("starfixd %s: %w; fix: nothing was changed; pick a release with a linux build, or build from source", tag, err)
	}
	return fmt.Errorf("download starfixd %s: %w; fix: nothing was changed; check the network (HTTPS_PROXY is honored) and retry", tag, err)
}

func swapFailure(exe string, err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("cannot replace %s: %w; fix: run as the user who owns %s (the user starfixd runs as)", exe, err, filepath.Dir(exe))
	}
	return fmt.Errorf("replace %s: %w; fix: retry; if it persists, reinstall from %s", exe, err, releasesURL)
}
