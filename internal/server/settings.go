package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/store"
	"github.com/go-sql-driver/mysql"
	"go.yaml.in/yaml/v3"
)

// DefaultConfigFile is read when it exists and no --config is given.
const DefaultConfigFile = "/etc/starfix/starfixd.yaml"

// Settings configure the starfixd commands: serve, stdio, upgrade, and the
// admin commands import-bd and export-bd.
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
	// LogLevel is the least severe level logged: debug, info, warn or
	// error. Default info; debug adds a line per request.
	LogLevel string `yaml:"log_level"`
	// LogFormat is text (slog's key=value) or json. Default text.
	LogFormat string `yaml:"log_format"`
	// Admins are the principals who may change issues others hold and
	// force a close (decision D2). They are set here, or in
	// STARFIXD_ADMINS as a comma-separated list, and nowhere else:
	// nothing over the protocol reads or changes them. None may be
	// reserved (store.ReservedPrincipals).
	Admins []string `yaml:"admins"`
	// AllowUnsafeDolt opens the store even when its Dolt account is
	// unsafe (store.Options.AllowUnsafeAccount, S-2). Only a command-line
	// flag sets it, and only with --dev (decision D3); no config file or
	// environment variable can.
	AllowUnsafeDolt bool `yaml:"-"`
	// Limits bound what one principal can make the server do; see
	// [Limits]. Zero fields take the defaults.
	Limits Limits `yaml:"limits"`
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
	// EnvLogLevel and EnvLogFormat set LogLevel and LogFormat.
	EnvLogLevel  = "STARFIXD_LOG_LEVEL"
	EnvLogFormat = "STARFIXD_LOG_FORMAT"
	// EnvAdmins sets Admins, comma-separated.
	EnvAdmins = "STARFIXD_ADMINS"
)

// ResolveSettings merges, highest precedence first: flags, the environment,
// the config file, and defaults. configPath "" means STARFIXD_CONFIG, else
// DefaultConfigFile if it exists; a named file must exist. Limits come from
// the file only, Admins from the file unless STARFIXD_ADMINS replaces
// them, and AllowUnsafeDolt from flags only. It refuses a password in
// flags.DSN, which the process list would show; a config file with an
// unknown key, or with a password and any access for group or others; an
// admin name that is invalid or reserved; an invalid limit; a systemd
// unit that is not a unit name; and an unknown log level or format.
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
	env := Settings{DSN: getenv(EnvDSN), Socket: getenv(EnvSocket), Project: getenv(EnvProject),
		LogLevel: getenv(EnvLogLevel), LogFormat: getenv(EnvLogFormat)}
	out := Settings{Socket: DefaultSocket, Prefix: "sf", LogLevel: "info", LogFormat: "text"}
	for _, s := range []Settings{file, env, flags} {
		for _, f := range []struct {
			dst *string
			v   string
		}{
			{&out.DSN, s.DSN}, {&out.Socket, s.Socket}, {&out.Project, s.Project},
			{&out.Prefix, s.Prefix}, {&out.Latest, s.Latest}, {&out.SystemdUnit, s.SystemdUnit},
			{&out.LogLevel, s.LogLevel}, {&out.LogFormat, s.LogFormat},
		} {
			if f.v != "" {
				*f.dst = f.v
			}
		}
	}
	out.AllowUnsafeDolt = flags.AllowUnsafeDolt
	out.Limits = file.Limits
	out.Admins = file.Admins
	if v := getenv(EnvAdmins); v != "" {
		out.Admins = nil
		for a := range strings.SplitSeq(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				out.Admins = append(out.Admins, a)
			}
		}
	}
	for _, a := range out.Admins {
		if !PrincipalPattern.MatchString(a) || store.Reserved(a) {
			return Settings{}, fmt.Errorf("admin %q is not a principal name, or is reserved for the server; fix: list the admins' principal names under admins: or in %s", a, EnvAdmins)
		}
	}
	if err := out.Limits.Validate(); err != nil {
		return Settings{}, err
	}
	out.Limits = out.Limits.WithDefaults()
	if out.SystemdUnit != "" && !UnitPattern.MatchString(out.SystemdUnit) {
		return Settings{}, fmt.Errorf("systemd_unit %q is not a unit name; fix: set it to the service's name, for example starfixd.service", out.SystemdUnit)
	}
	if _, err := NewLogger(io.Discard, out.LogLevel, out.LogFormat); err != nil {
		return Settings{}, err
	}
	return out, nil
}

// NewLogger returns a logger writing to w at level (debug, info, warn or
// error) in format (text or json).
func NewLogger(w io.Writer, level, format string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil || !slices.Contains([]string{"debug", "info", "warn", "error"}, level) {
		return nil, fmt.Errorf("log level %q is not debug, info, warn or error; fix: set log_level: or %s to one of them", level, EnvLogLevel)
	}
	opts := &slog.HandlerOptions{Level: l}
	switch format {
	case "text":
		return slog.New(slog.NewTextHandler(w, opts)), nil
	case "json":
		return slog.New(slog.NewJSONHandler(w, opts)), nil
	}
	return nil, fmt.Errorf("log format %q is not text or json; fix: set log_format: or %s to text or json", format, EnvLogFormat)
}

// loadSettings reads the config file at path, refusing unknown keys. A
// missing file is not an error unless required. A file whose DSN holds a
// password is refused if its group or others have any access to it.
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
