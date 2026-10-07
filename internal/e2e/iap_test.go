//go:build unix

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/iaptest"
)

// viaIAP points this user's .starfix.yaml at the world's server through
// Google Cloud IAP: the host becomes an instance name nothing resolves,
// so only the tunnel can reach it.
func (u *user) viaIAP() {
	t := u.w.t
	t.Helper()
	path := filepath.Join(u.repo, ".starfix.yaml")
	b, err := os.ReadFile(path) //nolint:gosec // the test's own temp repository
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "  host: 127.0.0.1\n",
		"  host: starfix-1\n  iap:\n    project: example-project\n    zone: us-central1-a\n", 1)
	if err := os.WriteFile(path, []byte(s), 0o600); err != nil { //nolint:gosec // the test's own temp repository
		t.Fatal(err)
	}
}

// sfx reaches a real daemon through the IAP tunnel, and the pinned host
// key is still checked at the far end of it.
func TestIAP(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	gcloud := iaptest.Install(t, iaptest.Bridge)
	alice := w.newUser("alice", "")
	alice.viaIAP()
	id := strings.TrimSpace(alice.ok("create", "through the tunnel"))
	if out := alice.ok("show", id); !strings.Contains(out, "through the tunnel") {
		t.Errorf("sfx show %s through IAP:\n%s", id, out)
	}
	if args := gcloud.Last(t).Args; len(args) < 3 || args[2] != "starfix-1" {
		t.Errorf("gcloud args = %q, want the tunnel to instance starfix-1", args)
	}

	mallory := w.newUser("mallory", "SHA256:et6CqkKsyU2BxzA7Ws+V18rXrtM9Gj6dk1/m0C5TqvU")
	mallory.viaIAP()
	r := mallory.run("v0.2.0", "list")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "host key mismatch for starfix-1") ||
		!strings.Contains(r.stderr, "fix: if the server was rebuilt") {
		t.Fatalf("sfx list through IAP with the wrong pin: exit %d\n%s", r.code, r.stderr)
	}
}
