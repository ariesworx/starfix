// Package client connects to starfixd over SSH and speaks the protocol.
//
// The SSH client runs in-process (golang.org/x/crypto/ssh); no system ssh
// is used. The server's ED25519 host key is pinned in .starfix.yaml and
// checked on every connection: a mismatch is refused, and an unpinned host
// is never trusted on first use. The developer authenticates with a key
// from ssh-agent (SSH_AUTH_SOCK, or the OpenSSH named pipe on Windows) or the key file named in the config.
package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ariesworx/starfix/internal/proto"
)

// ConfigFile is the committed, per-repository configuration. It holds no
// secrets.
const ConfigFile = ".starfix.yaml"

// Config is .starfix.yaml.
type Config struct {
	// Project is the project's UUID.
	Project string `yaml:"project"`
	Server  struct {
		Host string `yaml:"host"`
		// Port defaults to 22.
		Port int `yaml:"port"`
		// User is the SSH account starfixd runs as. Default "starfix".
		User string `yaml:"user"`
		// HostKey is the server's pinned ED25519 fingerprint, SHA256:….
		HostKey string `yaml:"host_key"`
	} `yaml:"server"`
	// Key is an optional private key file, absolute, ~/-relative, or
	// relative to the repository root. Without it, ssh-agent is used.
	Key string `yaml:"key"`

	// Root is the directory holding the config file (not read from YAML).
	Root string `yaml:"-"`
}

var (
	uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:-]{0,252}$`)
	userRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	fprRE  = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

// LoadConfig finds .starfix.yaml in dir or the nearest parent that has one.
func LoadConfig(dir string) (*Config, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for {
		path := filepath.Join(dir, ConfigFile)
		b, err := os.ReadFile(path) //nolint:gosec // the repository's own config
		if err == nil {
			c, err := ParseConfig(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			c.Root = dir
			return c, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("config: %w", err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, proto.Errf(proto.CodeInvalid,
				"run starfix inside a repository that has one, or pass -C DIR",
				ConfigFile+" not found here or in any parent directory")
		}
		dir = parent
	}
}

// ParseConfig reads and validates a config, filling defaults. Unknown keys
// are refused.
func ParseConfig(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if c.Server.Port == 0 {
		c.Server.Port = 22
	}
	if c.Server.User == "" {
		c.Server.User = "starfix"
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	switch {
	case !uuidRE.MatchString(c.Project):
		return errors.New("project must be the project's lowercase UUID")
	case !hostRE.MatchString(c.Server.Host):
		return fmt.Errorf("server.host %q is not a host name", c.Server.Host)
	case c.Server.Port < 1 || c.Server.Port > 65535:
		return fmt.Errorf("server.port %d is out of range", c.Server.Port)
	case !userRE.MatchString(c.Server.User):
		return fmt.Errorf("server.user %q is not a Unix account name", c.Server.User)
	case !fprRE.MatchString(c.Server.HostKey):
		return errors.New("server.host_key must be the server's ED25519 fingerprint (SHA256:…, from `ssh-keyscan -t ed25519 HOST | ssh-keygen -lf -`)")
	case strings.ContainsAny(c.Key, "\x00\n"):
		return errors.New("key is not a file path")
	}
	return nil
}

// KeyPath resolves Key: "~/" is the home directory, and a relative path is
// relative to the repository root. It returns "" when no key is set.
func (c *Config) KeyPath() (string, error) {
	k := c.Key
	switch {
	case k == "":
		return "", nil
	case k == "~" || strings.HasPrefix(k, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("key: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(k, "~")), nil
	case filepath.IsAbs(k):
		return k, nil
	}
	return filepath.Join(c.Root, k), nil
}
