package main

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/release"
)

type result struct {
	code           int
	stdout, stderr string
}

func runWith(t *testing.T, secret string, trusted []ed25519.PublicKey, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, env{
		getenv: func(k string) string {
			if k == EnvKey {
				return secret
			}
			return ""
		},
		stdout: &out, stderr: &errb,
		keys: func() ([]ed25519.PublicKey, error) { return trusted, nil },
	})
	return result{code, out.String(), errb.String()}
}

// newKey runs gen and returns the key file's contents and the public key.
func newKey(t *testing.T, dir string) (secret string, pub ed25519.PublicKey) {
	t.Helper()
	keyfile := filepath.Join(dir, "release.key")
	r := runWith(t, "", nil, "gen", keyfile)
	if r.code != 0 {
		t.Fatalf("gen: %+v", r)
	}
	b, err := os.ReadFile(keyfile) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	secret = strings.TrimSpace(string(b))
	if strings.Contains(r.stdout, secret) || strings.Contains(r.stderr, secret) {
		t.Fatal("gen printed the private key")
	}
	fi, err := os.Stat(keyfile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v, want 0600", fi.Mode().Perm())
	}
	keys, err := release.ParseKeys([]string{strings.Trim(strings.TrimSpace(r.stdout), `",`)})
	if err != nil {
		t.Fatalf("gen printed %q: %v", r.stdout, err)
	}
	return secret, keys[0]
}

func TestGenSignVerify(t *testing.T) {
	dir := t.TempDir()
	secret, pub := newKey(t, dir)
	sums := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(sums, []byte("abc  sfx_1.0.0_linux_amd64.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted := []ed25519.PublicKey{pub}

	r := runWith(t, secret, trusted, "sign", sums)
	if r.code != 0 || strings.Contains(r.stdout+r.stderr, secret) {
		t.Fatalf("sign: %+v", r)
	}
	if r := runWith(t, "", trusted, "verify", sums); r.code != 0 || r.stdout != sums+": good signature\n" {
		t.Fatalf("verify: %+v", r)
	}
	if r := runWith(t, "", nil, "verify", "--key", release.EncodeKey(pub), sums, sums+".sig"); r.code != 0 {
		t.Fatalf("verify --key: %+v", r)
	}
	// A second sign rewrites the same signature (Ed25519 is deterministic).
	first, _ := os.ReadFile(sums + ".sig") //nolint:gosec // test temp dir
	if r := runWith(t, secret, trusted, "sign", sums); r.code != 0 {
		t.Fatalf("second sign: %+v", r)
	}
	if again, _ := os.ReadFile(sums + ".sig"); !bytes.Equal(first, again) { //nolint:gosec // test temp dir
		t.Error("second sign changed the signature")
	}

	if err := os.WriteFile(sums, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runWith(t, "", trusted, "verify", sums); r.code != 1 || !strings.Contains(r.stderr, "fix: do not use") {
		t.Fatalf("verify tampered: %+v", r)
	}
}

func TestRefusals(t *testing.T) {
	dir := t.TempDir()
	secret, pub := newKey(t, dir)
	other, _ := newKey(t, t.TempDir())
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted := []ed25519.PublicKey{pub}
	tests := []struct {
		name   string
		secret string
		keys   []ed25519.PublicKey
		args   []string
		code   int
		stderr string
	}{
		{"no command", "", nil, nil, 2, "no command"},
		{"gen over an existing file", "", nil, []string{"gen", filepath.Join(dir, "release.key")}, 1, "does not exist yet"},
		{"gen flag-like path", "", nil, []string{"gen", "-x"}, 2, "gen takes one KEYFILE"},
		{"sign without secret", "", trusted, []string{"sign", file}, 1, "STARFIX_RELEASE_KEY is empty"},
		{"sign with a garbage secret", "not-base64!", trusted, []string{"sign", file}, 1, "is not base64"},
		{"sign with a short secret", "AAAA", trusted, []string{"sign", file}, 1, "not a 32-byte"},
		{"sign with an untrusted key", other, trusted, []string{"sign", file}, 1, "is not in internal/release/keys.go"},
		{"verify with no keys", "", []ed25519.PublicKey{}, []string{"verify", file}, 1, "fix: pass --key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := runWith(t, tt.secret, tt.keys, tt.args...)
			if r.code != tt.code || !strings.Contains(r.stderr, tt.stderr) {
				t.Fatalf("got %+v, want exit %d and %q", r, tt.code, tt.stderr)
			}
			if strings.Contains(r.stdout+r.stderr, secret) || strings.Contains(r.stdout+r.stderr, other) {
				t.Fatal("output holds a private key")
			}
			if _, err := os.Stat(file + ".sig"); err == nil {
				t.Fatal("a refused command wrote a signature")
			}
		})
	}
}
