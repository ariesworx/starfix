package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"

	"github.com/go-sql-driver/mysql"
	"go.yaml.in/yaml/v3"
)

// DefaultConfigFile is read when it exists and no --config is given.
const DefaultConfigFile = "/etc/starfix/starfixd.yaml"

// Settings configure `starfixd serve` and `starfixd stdio`.
type Settings struct {
	// DSN is the Dolt database (go-sql-driver/mysql form). It may hold a
	// password, so it never comes from a command-line flag with one.
	DSN string `yaml:"dsn"`
	// Socket is the daemon's unix socket. Default DefaultSocket.
	Socket string `yaml:"socket"`
	// Project is the UUID of the project served.
	Project string `yaml:"project"`
	// Prefix is the issue-ID prefix for generated IDs. Default "sf".
	Prefix string `yaml:"prefix"`
	// Latest is the latest release, set by hand on air-gapped servers.
	Latest string `yaml:"latest"`
	// SystemdUnit is the unit `starfixd upgrade` restarts. Empty means
	// upgrade installs the binary and leaves the restart to the admin,
	// unless --restart is given.
	SystemdUnit string `yaml:"systemd_unit"`
}

// UnitPattern is what a systemd unit name may look like. It cannot start
// with "-", so it never reaches systemctl as an option.
var UnitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._:-]{0,127}$`)

// Environment variables read by ResolveSettings.
const (
	EnvDSN     = "STARFIXD_DSN"
	EnvSocket  = "STARFIXD_SOCKET"
	EnvProject = "STARFIXD_PROJECT"
	EnvConfig  = "STARFIXD_CONFIG"
)

// ResolveSettings merges, highest precedence first: flags, the environment,
// the config file, and defaults. configPath "" means STARFIXD_CONFIG, else
// DefaultConfigFile if it exists; a named file must exist.
func ResolveSettings(flags Settings, configPath string, getenv func(string) string) (Settings, error) {
	if flags.DSN != "" {
		cfg, err := mysql.ParseDSN(flags.DSN)
		if err != nil {
			return Settings{}, fmt.Errorf("--dsn: %w", err)
		}
		if cfg.Passwd != "" {
			return Settings{}, fmt.Errorf("--dsn holds a password, which would show in the process list; fix: set %s or dsn: in the config file instead", EnvDSN)
		}
	}
	required := true
	if configPath == "" {
		configPath = getenv(EnvConfig)
	}
	if configPath == "" {
		configPath, required = DefaultConfigFile, false
	}
	file, err := loadSettings(configPath, required)
	if err != nil {
		return Settings{}, err
	}
	env := Settings{DSN: getenv(EnvDSN), Socket: getenv(EnvSocket), Project: getenv(EnvProject)}
	out := Settings{Socket: DefaultSocket, Prefix: "sf"}
	for _, s := range []Settings{file, env, flags} {
		for _, f := range []struct {
			dst *string
			v   string
		}{
			{&out.DSN, s.DSN}, {&out.Socket, s.Socket}, {&out.Project, s.Project},
			{&out.Prefix, s.Prefix}, {&out.Latest, s.Latest}, {&out.SystemdUnit, s.SystemdUnit},
		} {
			if f.v != "" {
				*f.dst = f.v
			}
		}
	}
	if out.SystemdUnit != "" && !UnitPattern.MatchString(out.SystemdUnit) {
		return Settings{}, fmt.Errorf("systemd_unit %q is not a unit name; fix: set it to the service's name, for example starfixd.service", out.SystemdUnit)
	}
	return out, nil
}

func loadSettings(path string, required bool) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(path) //nolint:gosec // the admin names the config file
	if errors.Is(err, fs.ErrNotExist) && !required {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil && !errors.Is(err, io.EOF) {
		return s, fmt.Errorf("config %s: %w", path, err)
	}
	if s.DSN != "" {
		if cfg, err := mysql.ParseDSN(s.DSN); err == nil && cfg.Passwd != "" {
			fi, err := os.Stat(path)
			if err != nil {
				return s, fmt.Errorf("config: %w", err)
			}
			if fi.Mode().Perm()&0o077 != 0 {
				return s, fmt.Errorf("config %s holds a database password but is mode %04o; fix: chmod 600 %s", path, fi.Mode().Perm(), path)
			}
		}
	}
	return s, nil
}
