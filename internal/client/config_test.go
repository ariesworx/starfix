package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if _, err := LoadConfig(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("no config: %v", err)
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
