package release_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/release"
	"github.com/ariesworx/starfix/internal/release/releasetest"
)

func TestFetch(t *testing.T) {
	bin := []byte("\x7fELF new sfx")
	tests := []struct {
		name     string
		goos     string
		goarch   string
		edit     func(t *testing.T, g *releasetest.Server, archive string)
		verifier error
		wantErr  error
		wantText string
	}{
		{name: "tar.gz", goos: "linux", goarch: "amd64"},
		{name: "zip", goos: "windows", goarch: "arm64"},
		{name: "checksum mismatch", goos: "linux", goarch: "amd64",
			edit: func(_ *testing.T, g *releasetest.Server, archive string) {
				s := fmt.Sprintf("%x  %s\n", sha256.Sum256([]byte("something else")), archive)
				g.Sums = &s
			}, wantErr: release.ErrMismatch},
		{name: "archive swapped after signing", goos: "linux", goarch: "amd64",
			edit: func(t *testing.T, g *releasetest.Server, archive string) {
				s := g.Checksums()
				g.Sums = &s
				g.Assets[archive] = releasetest.TarGz(t, "sfx", []byte("evil"))
			}, wantErr: release.ErrMismatch},
		{name: "not in checksums", goos: "linux", goarch: "amd64",
			edit: func(_ *testing.T, g *releasetest.Server, _ string) {
				s := strings.Repeat("0", 64) + "  other.tar.gz\n"
				g.Sums = &s
			}, wantErr: release.ErrNotFound},
		{name: "no asset for platform", goos: "darwin", goarch: "arm64", wantErr: release.ErrNotFound},
		{name: "unsigned release", goos: "linux", goarch: "amd64",
			edit:    func(_ *testing.T, g *releasetest.Server, _ string) { g.Omit[release.SignatureName] = true },
			wantErr: release.ErrSignature},
		{name: "signature refused", goos: "linux", goarch: "amd64", verifier: release.ErrSignature, wantErr: release.ErrSignature},
		{name: "asset served from another scheme", goos: "linux", goarch: "amd64",
			edit: func(_ *testing.T, g *releasetest.Server, archive string) {
				g.Rewrite = func(r *release.Release) {
					for i := range r.Assets {
						if r.Assets[i].Name == archive {
							r.Assets[i].URL = "ftp://downloads.example.com/" + archive
						}
					}
				}
			}, wantText: "is not http"},
		{name: "archive lacks the binary", goos: "linux", goarch: "amd64",
			edit: func(t *testing.T, g *releasetest.Server, archive string) {
				g.Assets[archive] = releasetest.TarGz(t, "other", bin)
			}, wantErr: release.ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := releasetest.New(t, "v1.2.0")
			linux := g.AddBinary(t, "sfx", "linux", "amd64", bin)
			g.AddBinary(t, "sfx", "windows", "arm64", bin)
			if tt.edit != nil {
				tt.edit(t, g, linux)
			}
			v := &releasetest.Verifier{Err: tt.verifier}
			c := g.Client(v)
			rel, err := c.Latest(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.Fetch(t.Context(), rel, "sfx", tt.goos, tt.goarch)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			case tt.wantText != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantText) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantText)
				}
				return
			case err != nil:
				t.Fatal(err)
			}
			if string(got) != string(bin) {
				t.Errorf("binary = %q, want %q", got, bin)
			}
			sums, sig, tag := v.Last()
			if tag != "v1.2.0" || string(sums) != g.Checksums() || string(sig) != g.Signature() {
				t.Errorf("verifier saw tag %q, checksums %q, sig %q", tag, sums, sig)
			}
		})
	}
}

// GitHub redirects an asset download to a signed URL whose query string
// is a credential. When that request fails, the error names it without
// the query string.
func TestFetchRedactsSignedURL(t *testing.T) {
	const secret = "sig=c2lnbmVkLXRva2Vu" //nolint:gosec // an invented stand-in for a signed URL's token
	tests := []struct {
		name   string
		signed http.HandlerFunc // serves the signed URL
	}{
		{name: "connection closed", signed: func(w http.ResponseWriter, _ *http.Request) {
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
		}},
		{name: "redirect loop", signed: func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/signed?"+secret, http.StatusFound)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/signed" {
					http.Redirect(w, r, "/signed?"+secret, http.StatusFound)
					return
				}
				tc.signed(w, r)
			}))
			t.Cleanup(cdn.Close)
			g := releasetest.New(t, "v1.2.0")
			g.AddBinary(t, "sfx", "linux", "amd64", []byte("sfx"))
			g.Rewrite = func(r *release.Release) {
				for i := range r.Assets {
					r.Assets[i].URL = cdn.URL + "/download/" + r.Assets[i].Name
				}
			}
			c := g.Client(&releasetest.Verifier{})
			rel, err := c.Latest(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Fetch(t.Context(), rel, "sfx", "linux", "amd64")
			if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), release.ChecksumsName) {
				t.Errorf("Fetch = %v, want an error naming %s without %q", err, release.ChecksumsName, secret)
			}
		})
	}
}

func TestFetchNeedsVerifier(t *testing.T) {
	g := releasetest.New(t, "v1.2.0")
	g.AddBinary(t, "sfx", "linux", "amd64", []byte("x"))
	c := g.Client(nil)
	rel, err := c.Latest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fetch(t.Context(), rel, "sfx", "linux", "amd64"); err == nil {
		t.Fatal("Fetch without a verifier succeeded")
	}
}

func TestReleaseMetadata(t *testing.T) {
	tests := []struct {
		tag string
		ok  bool
	}{
		{"v0.3.0", true},
		{"v0.3.0-rc.1", true},
		{"v1.0.0/../../x", false},
		{"latest", false},
		{"v01.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.tag, func(t *testing.T) {
			c := releasetest.New(t, tt.tag).Client(nil)
			rel, err := c.Latest(t.Context())
			if !tt.ok {
				if err == nil {
					t.Fatalf("tag %q accepted", tt.tag)
				}
				return
			}
			if err != nil || rel.Tag != tt.tag {
				t.Fatalf("Latest = %+v, %v", rel, err)
			}
		})
	}
	c := releasetest.New(t, "v0.3.0").Client(nil)
	if _, err := c.ByTag(t.Context(), "v9.9.9"); !errors.Is(err, release.ErrNotFound) {
		t.Errorf("ByTag(missing) = %v, want ErrNotFound", err)
	}
	if _, err := c.ByTag(t.Context(), "-v1"); err == nil || !strings.Contains(err.Error(), "not of the form") {
		t.Errorf("ByTag(-v1) = %v, want a refusal before any request", err)
	}
	if rel, err := c.ByTag(t.Context(), "v0.3.0"); err != nil || rel.Tag != "v0.3.0" {
		t.Errorf("ByTag = %+v, %v", rel, err)
	}
}

func TestArchiveName(t *testing.T) {
	tests := []struct{ bin, tag, goos, goarch, want string }{
		{"sfx", "v1.2.3", "linux", "amd64", "sfx_1.2.3_linux_amd64.tar.gz"},
		{"sfx", "v1.2.3", "windows", "arm64", "sfx_1.2.3_windows_arm64.zip"},
		{"starfixd", "v0.1.0-rc.1", "linux", "arm64", "starfixd_0.1.0-rc.1_linux_arm64.tar.gz"},
	}
	for _, tt := range tests {
		if got := release.ArchiveName(tt.bin, tt.tag, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("ArchiveName(%s, %s, %s, %s) = %s, want %s", tt.bin, tt.tag, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

// TestFetchEd25519 runs the real verifier end to end against the fake.
func TestFetchEd25519(t *testing.T) {
	tests := []struct {
		name    string
		edit    func(t *testing.T, g *releasetest.Server)
		wantErr error
	}{
		{name: "signed"},
		{name: "signed by another key", wantErr: release.ErrSignature, edit: func(t *testing.T, g *releasetest.Server) {
			_, other, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			sig := string(release.EncodeSignature(ed25519.Sign(other, []byte(g.Checksums()))))
			g.Sig = &sig
		}},
		{name: "checksums edited after signing", wantErr: release.ErrSignature, edit: func(_ *testing.T, g *releasetest.Server) {
			sig := g.Signature()
			g.Sig = &sig
			sums := g.Checksums() + strings.Repeat("0", 64) + "  extra.tar.gz\n"
			g.Sums = &sums
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := releasetest.New(t, "v1.2.0")
			g.AddBinary(t, "sfx", "linux", "amd64", []byte("bin"))
			if tt.edit != nil {
				tt.edit(t, g)
			}
			c := g.Client(g.Verifier())
			rel, err := c.Latest(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Fetch(t.Context(), rel, "sfx", "linux", "amd64")
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Fatalf("Fetch = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestEd25519(t *testing.T) {
	pub1, priv1, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub2, priv2, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("abc  sfx_1.0.0_linux_amd64.tar.gz\n")
	sig := func(k ed25519.PrivateKey) []byte { return release.EncodeSignature(ed25519.Sign(k, msg)) }
	tests := []struct {
		name    string
		keys    []ed25519.PublicKey
		sig     []byte
		msg     []byte
		wantErr error
	}{
		{name: "valid", keys: []ed25519.PublicKey{pub1}, sig: sig(priv1)},
		{name: "rotation: new key beside old", keys: []ed25519.PublicKey{pub1, pub2}, sig: sig(priv2)},
		{name: "no trailing newline", keys: []ed25519.PublicKey{pub1}, sig: []byte(strings.TrimSpace(string(sig(priv1))))},
		{name: "unknown key", keys: []ed25519.PublicKey{pub1}, sig: sig(priv2), wantErr: release.ErrSignature},
		{name: "other message", keys: []ed25519.PublicKey{pub1}, sig: sig(priv1), msg: []byte("x"), wantErr: release.ErrSignature},
		{name: "not base64", keys: []ed25519.PublicKey{pub1}, sig: []byte("%%%"), wantErr: release.ErrSignature},
		{name: "short", keys: []ed25519.PublicKey{pub1}, sig: []byte("AAAA"), wantErr: release.ErrSignature},
		{name: "empty", keys: []ed25519.PublicKey{pub1}, sig: nil, wantErr: release.ErrSignature},
		{name: "no keys fails closed", keys: []ed25519.PublicKey{}, sig: sig(priv1), wantErr: release.ErrNoKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := msg
			if tt.msg != nil {
				m = tt.msg
			}
			err := release.Ed25519{Keys: tt.keys}.Verify(t.Context(), m, tt.sig, "v1.0.0")
			if !errors.Is(err, tt.wantErr) || (tt.wantErr == nil && err != nil) {
				t.Errorf("Verify = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestReleaseKeys checks the built-in list parses, and that an empty one
// fails closed rather than accepting anything.
func TestReleaseKeys(t *testing.T) {
	keys, err := release.Keys()
	if err != nil {
		t.Fatalf("keys.go: %v", err)
	}
	if len(keys) == 0 {
		err := release.Ed25519{}.Verify(t.Context(), []byte("x"), []byte("x"), "v1.0.0")
		if !errors.Is(err, release.ErrNoKey) {
			t.Errorf("Verify with no built-in keys = %v, want ErrNoKey", err)
		}
	}
	for _, bad := range []string{"", "AAAA", "not base64!"} {
		if _, err := release.ParseKeys([]string{bad}); err == nil {
			t.Errorf("ParseKeys(%q) accepted", bad)
		}
	}
}
