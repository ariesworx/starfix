package client

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

const (
	testProject = "6f1c2d3e-4a5b-4c6d-8e7f-0a1b2c3d4e5f"
	testFpr     = "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU"
)

func TestParseConfig(t *testing.T) {
	good := "project: " + testProject + "\nserver:\n  host: starfix.example.com\n  host_key: " + testFpr + "\n"
	tests := []struct {
		name string
		in   string
		err  string
	}{
		{name: "minimal", in: good},
		{name: "full", in: good + "  port: 2222\n  user: tracker\nkey: ~/.ssh/id_ed25519\n"},
		{name: "unknown key", in: good + "colour: blue\n", err: "colour"},
		{name: "no project", in: strings.Replace(good, "project: "+testProject+"\n", "", 1), err: "project"},
		{name: "uppercase uuid", in: strings.Replace(good, testProject, strings.ToUpper(testProject), 1), err: "project"},
		{name: "short fingerprint", in: strings.Replace(good, testFpr, "SHA256:abc", 1), err: "host_key"},
		{name: "md5 fingerprint", in: strings.Replace(good, testFpr, "MD5:aa:bb", 1), err: "host_key"},
		{name: "option as host", in: strings.Replace(good, "starfix.example.com", "-oProxyCommand=x", 1), err: "server.host"},
		{name: "bad port", in: good + "  port: 70000\n", err: "server.port"},
		{name: "bad user", in: good + "  user: Root!\n", err: "server.user"},
		{name: "empty", in: "", err: "project"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(tc.in))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("got %v, want error containing %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Server.Port == 0 || c.Server.User == "" {
				t.Fatalf("defaults not filled: %+v", c)
			}
		})
	}
}

func TestLoadConfigWalksUp(t *testing.T) {
	root := t.TempDir()
	body := "project: " + testProject + "\nserver:\n  host: starfix.example.com\n  host_key: " + testFpr + "\nkey: keys/me\n"
	if err := os.WriteFile(filepath.Join(root, ConfigFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(deep)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != root || c.Server.Port != 22 || c.Server.User != "starfix" {
		t.Fatalf("config: %+v", c)
	}
	if p, err := c.KeyPath(); err != nil || p != filepath.Join(root, "keys", "me") {
		t.Fatalf("key path %q %v", p, err)
	}
	_, err = LoadConfig(t.TempDir())
	var pe *proto.Error
	if !errors.Is(err, ErrNoConfig) || !errors.As(err, &pe) || pe.Fix == "" || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("no config: %v", err)
	}
	if _, err := LoadConfig(filepath.Join(root, ConfigFile, "x")); errors.Is(err, ErrNoConfig) {
		t.Fatalf("a config that cannot be read is not a missing one: %v", err)
	}
}

func TestKeyPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	tests := []struct{ key, want string }{
		{"", ""},
		{"/abs/key", "/abs/key"},
		{"~/.ssh/k", filepath.Join(home, ".ssh/k")},
		{"rel/k", "/repo/rel/k"},
	}
	for _, tc := range tests {
		c := &Config{Key: tc.key, Root: "/repo"}
		if got, err := c.KeyPath(); err != nil || got != tc.want {
			t.Errorf("KeyPath(%q) = %q, %v; want %q", tc.key, got, err, tc.want)
		}
	}
}

// Discovery stops at the repository's top level and at the home
// directory, so a config planted in a shared parent (/tmp, a drive root)
// cannot capture a checkout without one (C-6).
func TestLoadConfigBoundary(t *testing.T) {
	body := "project: " + testProject + "\nserver:\n  host: starfix.example.com\n  host_key: " + testFpr + "\n"
	tests := []struct {
		name string
		// dirs are made, files written with body (or as named, mode
		// 0600), relative to a temp top; run is where LoadConfig starts.
		dirs, configs []string
		gitFile       string // a .git file, as a worktree or submodule has
		run, root     string // root "" means not found
	}{
		{name: "in the repository", dirs: []string{"repo/.git", "repo/a"}, configs: []string{"repo"}, run: "repo/a", root: "repo"},
		{name: "not above the repository", dirs: []string{"repo/.git", "repo/a"}, configs: []string{"."}, run: "repo/a"},
		{name: "not above a worktree's .git file", dirs: []string{"wt/a"}, gitFile: "wt", configs: []string{"."}, run: "wt/a"},
		{name: "a nested repository is its own", dirs: []string{"outer/.git", "outer/inner/.git"}, configs: []string{"outer"}, run: "outer/inner"},
		{name: "up to home", dirs: []string{"home/proj/a"}, configs: []string{"home"}, run: "home/proj/a", root: "home"},
		{name: "not above home", dirs: []string{"home/proj"}, configs: []string{"."}, run: "home/proj"},
		{name: "outside home, up to the root", dirs: []string{"srv/proj/a"}, configs: []string{"srv"}, run: "srv/proj/a", root: "srv"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			top := t.TempDir()
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(top, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.gitFile != "" {
				if err := os.WriteFile(filepath.Join(top, tc.gitFile, ".git"), []byte("gitdir: /elsewhere\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for _, c := range tc.configs {
				if err := os.WriteFile(filepath.Join(top, c, ConfigFile), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, err := loadConfig(filepath.Join(top, tc.run), filepath.Join(top, "home"))
			if tc.root == "" {
				if !errors.Is(err, ErrNoConfig) {
					t.Fatalf("loadConfig(%s) = %+v, %v; want not found", tc.run, c, err)
				}
				return
			}
			if err != nil || c.Root != filepath.Join(top, tc.root) {
				t.Fatalf("loadConfig(%s) = %+v, %v; want root %s", tc.run, c, err, tc.root)
			}
		})
	}
}

// A config another user could have written is refused, with a fix.
func TestLoadConfigRefusesOthersFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix owner or mode bits; see LoadConfig")
	}
	body := "project: " + testProject + "\nserver:\n  host: starfix.example.com\n  host_key: " + testFpr + "\n"
	tests := []struct {
		name  string
		mode  fs.FileMode
		owner int // -1: ours
		want  string
		dir   bool
	}{
		{name: "group writable", mode: 0o664, owner: -1, want: "writable by other users"},
		{name: "world writable", mode: 0o646, owner: -1, want: "writable by other users"},
		{name: "another user's", mode: 0o644, owner: 65534, want: "owned by another user"},
		{name: "not a file", owner: -1, dir: true, want: "not a regular file"},
		{name: "ours, read-only to others", mode: 0o644, owner: -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, ConfigFile)
			if tc.dir {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.owner >= 0 {
				if err := os.Chown(path, tc.owner, tc.owner); err != nil {
					t.Skipf("cannot give the file to another user here: %v", err)
				}
			}
			_, err := loadConfig(root, "")
			if tc.want == "" {
				if err != nil {
					t.Fatalf("loadConfig = %v", err)
				}
				return
			}
			var pe *proto.Error
			if !errors.As(err, &pe) || !strings.Contains(err.Error(), tc.want) || pe.Fix == "" || errors.Is(err, ErrNoConfig) {
				t.Fatalf("loadConfig = %v; want a refusal saying %q, with a fix", err, tc.want)
			}
		})
	}
}
