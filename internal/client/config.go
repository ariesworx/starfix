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

// ErrNoConfig matches, with errors.Is, LoadConfig's error when neither
// the directory nor any parent holds ConfigFile. The error is also a
// *proto.Error for errors.As.
var ErrNoConfig = errors.New(ConfigFile + " not found")

type noConfigError struct{ pe *proto.Error }

func (e noConfigError) Error() string { return e.pe.Error() }

func (e noConfigError) Unwrap() error { return e.pe }

func (noConfigError) Is(target error) bool { return target == ErrNoConfig }

// LoadConfig finds .starfix.yaml in dir or the nearest parent that has
// one. The search stops at the repository's top level (the first
// directory holding .git, a directory or a worktree's file) and at the
// home directory, never above them: a config in a shared parent such as
// /tmp, or a drive root on Windows, must not capture a checkout that has
// none of its own. The file found must be a regular file; on Unix it
// must also be the user's own and writable by no one else, since it
// names the server every command and hook connects to. Windows has no
// owner or mode check here (its ACLs are not Unix bits), so there the
// boundary is the only guard.
func LoadConfig(dir string) (*Config, error) {
	home, _ := os.UserHomeDir() // no home: the search stops at .git or the root
	return loadConfig(dir, home)
}

func loadConfig(dir, home string) (*Config, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var homeInfo fs.FileInfo
	if home != "" {
		homeInfo, _ = os.Stat(home) // a missing home bounds nothing
	}
	for {
		path := filepath.Join(dir, ConfigFile)
		fi, err := os.Stat(path)
		if err == nil {
			if err := checkConfigFile(path, fi); err != nil {
				return nil, err
			}
			b, err := os.ReadFile(path) //nolint:gosec // the repository's own config, checked above
			if err != nil {
				return nil, fmt.Errorf("config: %w", err)
			}
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
		if parent == dir || isRepoTop(dir) || sameDir(dir, homeInfo) {
			return nil, noConfigError{proto.Errf(proto.CodeInvalid,
				"run starfix inside a repository that has one, or pass -C DIR; the search stops at the repository's top level and at your home directory",
				ConfigFile+" not found here or in any parent directory")}
		}
		dir = parent
	}
}

// isRepoTop reports whether dir holds .git: a directory, or the file a
// worktree or submodule has.
func isRepoTop(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// sameDir reports whether dir is the directory home describes.
func sameDir(dir string, home fs.FileInfo) bool {
	if home == nil {
		return false
	}
	fi, err := os.Stat(dir)
	return err == nil && os.SameFile(fi, home)
}

// checkConfigFile refuses a config that is not a regular file or that
// someone else could have written.
func checkConfigFile(path string, fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return proto.Errf(proto.CodeInvalid, "replace it with the project's "+ConfigFile+" file",
			path+" is not a regular file")
	}
	if !OwnedByUser(fi) {
		return proto.Errf(proto.CodeInvalid,
			"check where it came from; if it is your project's, copy it into your checkout as your own file, else delete it",
			path+" is owned by another user; starfix reads only your own "+ConfigFile)
	}
	if groupOrWorldWritable(fi) {
		return proto.Errf(proto.CodeInvalid, "run `chmod go-w "+path+"`",
			fmt.Sprintf("%s is writable by other users (mode %04o)", path, fi.Mode().Perm()))
	}
	return nil
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
