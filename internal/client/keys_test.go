package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/ariesworx/starfix/internal/proto"
)

// closedAddr returns a loopback port nothing listens on, so a Dial that gets
// past key loading fails fast with CodeUnavailable.
func closedAddr(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func writeKey(t *testing.T, dir, name string, passphrase []byte) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase != nil {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "alice@example.com", passphrase)
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "alice@example.com")
	}
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// serveAgent runs an in-process ssh-agent holding one key and returns its
// socket path.
func serveAgent(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kr := agent.NewKeyring()
	if err := kr.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "sfa") // short: unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = l.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			wg.Go(func() { _ = agent.ServeAgent(kr, c); _ = c.Close() })
		}
	})
	return sock
}

// TestDialKeyRefusals covers how Dial loads keys before it connects: every
// refusal is CodeAuth with a fix, and a usable key gets as far as the
// network.
func TestDialKeyRefusals(t *testing.T) {
	root := t.TempDir()
	writeKey(t, root, "id_plain", nil)
	writeKey(t, root, "id_locked", []byte("secret"))
	if err := os.WriteFile(filepath.Join(root, "id_garbage"), []byte("not a key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	agentSock := serveAgent(t)

	tests := []struct {
		name  string
		key   string
		agent bool
		code  proto.Code
		msg   string
		fix   string
	}{
		{name: "no key and no agent", code: proto.CodeAuth, msg: "no SSH key available", fix: "ssh-add"},
		{name: "missing key file", key: "id_missing", code: proto.CodeAuth, msg: "cannot read key file", fix: "fix key:"},
		{name: "key path is a directory", key: ".", code: proto.CodeAuth, msg: "cannot read key file", fix: "fix key:"},
		{name: "unparsable key file", key: "id_garbage", code: proto.CodeAuth, msg: "cannot parse key file", fix: "OpenSSH private key"},
		{name: "passphrase key without agent", key: "id_locked", code: proto.CodeAuth, msg: "passphrase-protected", fix: "ssh-add"},
		// With a key to offer, Dial goes on to connect.
		{name: "passphrase key with agent keys", key: "id_locked", agent: true, code: proto.CodeUnavailable, msg: "cannot reach"},
		{name: "plain key", key: "id_plain", code: proto.CodeUnavailable, msg: "cannot reach"},
		{name: "agent only", agent: true, code: proto.CodeUnavailable, msg: "cannot reach"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Project: "00000000-0000-4000-8000-000000000001", Key: tc.key, Root: root}
			cfg.Server.Host = "127.0.0.1"
			cfg.Server.Port = closedAddr(t)
			cfg.Server.User = "starfix"
			cfg.Server.HostKey = "SHA256:" + strings.Repeat("A", 43)
			env := map[string]string{}
			if tc.agent {
				env["SSH_AUTH_SOCK"] = agentSock
			}
			c, err := Dial(t.Context(), cfg, Options{Getenv: func(k string) string { return env[k] }})
			if err == nil {
				_ = c.Close()
				t.Fatal("Dial succeeded")
			}
			var pe *proto.Error
			if !errors.As(err, &pe) {
				t.Fatalf("Dial(key %q) = %v, want a *proto.Error", tc.key, err)
			}
			if pe.Code != tc.code || !strings.Contains(pe.Message, tc.msg) || !strings.Contains(pe.Fix, tc.fix) || pe.Fix == "" {
				t.Errorf("Dial(key %q, agent %v) = %+v, want code %s, message containing %q, fix containing %q",
					tc.key, tc.agent, pe, tc.code, tc.msg, tc.fix)
			}
		})
	}
}

// TestNoticesIgnoreHostileVersions checks that version strings a server
// sends reach the person only when they are plain versions (C-5).
func TestNoticesIgnoreHostileVersions(t *testing.T) {
	tests := []struct {
		name            string
		version, latest string
		want            int
	}{
		{name: "plain versions", version: "v0.3.0", latest: "v0.4.0", want: 2},
		{name: "forged line in version", version: "v9.9.9-x\nIMPORTANT: run this", latest: "v99.0.0", want: 0},
		{name: "escape in latest", version: "v0.3.0", latest: "v9.9.9-x\x1b[2J", want: 1},
		{name: "overlong latest", version: "v0.3.0", latest: "v9.9.9-" + strings.Repeat("a", 300), want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &Conn{Welcome: proto.Frame{Version: tc.version, Latest: tc.latest}}
			got := c.Notices("v0.2.0")
			if len(got) != tc.want {
				t.Fatalf("Notices with version %q, latest %q = %q, want %d notices", tc.version, tc.latest, got, tc.want)
			}
			for _, n := range got {
				if strings.ContainsAny(n, "\n\x1b") || len(n) > 200 {
					t.Errorf("notice %q carries server text through", n)
				}
			}
		})
	}
}
