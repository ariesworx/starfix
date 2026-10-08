// Package releasetest serves fake GitHub releases for tests of
// internal/release and the upgrade commands built on it.
//
// A [Server] is an httptest server that answers the GitHub API's
// release requests for one release and serves its assets: archives added
// with [Server.AddBinary], plus a checksums.txt and checksums.txt.sig it
// generates and signs with a per-Server key, unless a test overrides
// them. Its handler reads the Server's fields on another goroutine, so a
// test sets them before the code under test makes requests. [Verifier]
// is a [release.Verifier] double that records its last call, and is safe
// for concurrent use.
package releasetest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ariesworx/starfix/internal/release"
)

// Repo is the repository the fake serves.
const Repo = "example/starfix"

// Server is a fake GitHub API serving one release. Change its fields
// before the code under test makes requests.
type Server struct {
	// URL is the fake API's root, for [release.Client.BaseURL].
	URL string
	// Tag is the release served as latest and by tag.
	Tag string
	// Assets maps archive names to their contents.
	Assets map[string][]byte
	// Sums overrides checksums.txt, which is otherwise generated from
	// Assets.
	Sums *string
	// Sig overrides checksums.txt.sig, which is otherwise a signature
	// over checksums.txt by Key.
	Sig *string
	// Omit names assets left out of the release.
	Omit map[string]bool
	// Rewrite edits the release metadata before it is served.
	Rewrite func(*release.Release)

	// Key is the test release key, generated per Server; Public is its
	// public half.
	Key    ed25519.PrivateKey
	Public ed25519.PublicKey
}

// New starts a fake serving release tag, signed with a fresh test key,
// with no archives.
func New(t testing.TB, tag string) *Server {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Tag: tag, Assets: map[string][]byte{}, Omit: map[string]bool{}, Key: priv, Public: pub}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Client returns a release client for the fake, verifying with v.
func (s *Server) Client(v release.Verifier) *release.Client {
	return &release.Client{BaseURL: s.URL, Repo: Repo, Verifier: v}
}

// Verifier is the real Ed25519 verifier, trusting only this fake's key.
func (s *Server) Verifier() release.Ed25519 {
	return release.Ed25519{Keys: []ed25519.PublicKey{s.Public}}
}

// Signature is the checksums.txt.sig served.
func (s *Server) Signature() string {
	if s.Sig != nil {
		return *s.Sig
	}
	return string(release.EncodeSignature(ed25519.Sign(s.Key, []byte(s.Checksums()))))
}

// AddBinary adds the archive of binary bin, holding body, for goos/goarch
// and returns the archive's name. A Windows archive is a zip holding
// bin.exe.
func (s *Server) AddBinary(t testing.TB, bin, goos, goarch string, body []byte) string {
	t.Helper()
	name := release.ArchiveName(bin, s.Tag, goos, goarch)
	if goos == "windows" {
		s.Assets[name] = Zip(t, bin+".exe", body)
	} else {
		s.Assets[name] = TarGz(t, bin, body)
	}
	return name
}

// Checksums is the checksums.txt served.
func (s *Server) Checksums() string {
	if s.Sums != nil {
		return *s.Sums
	}
	var b strings.Builder
	for _, n := range slices.Sorted(maps.Keys(s.Assets)) {
		fmt.Fprintf(&b, "%x  %s\n", sha256.Sum256(s.Assets[n]), n)
	}
	return b.String()
}

// serve answers requests for the latest release and for the release by
// its tag, listing checksums.txt, its signature and every asset, each
// under /dl/ unless omitted, and serves those files.
func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	base := "/repos/" + Repo + "/releases/"
	switch {
	case r.URL.Path == base+"latest", r.URL.Path == base+"tags/"+s.Tag:
		rel := release.Release{Tag: s.Tag}
		names := []string{release.ChecksumsName, release.SignatureName}
		for n := range s.Assets {
			names = append(names, n)
		}
		for _, n := range names {
			if !s.Omit[n] {
				rel.Assets = append(rel.Assets, release.Asset{Name: n, URL: s.URL + "/dl/" + n})
			}
		}
		if s.Rewrite != nil {
			s.Rewrite(&rel)
		}
		_ = json.NewEncoder(w).Encode(rel)
	case r.URL.Path == "/dl/"+release.ChecksumsName:
		_, _ = w.Write([]byte(s.Checksums()))
	case r.URL.Path == "/dl/"+release.SignatureName:
		_, _ = w.Write([]byte(s.Signature()))
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		b, ok := s.Assets[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	default:
		http.NotFound(w, r)
	}
}

// Verifier is a release.Verifier test double: it records its last call
// and returns Err.
type Verifier struct {
	Err error

	mu        sync.Mutex // guards the last call's arguments below
	checksums []byte
	sig       []byte
	tag       string
}

// Verify implements release.Verifier.
func (v *Verifier) Verify(_ context.Context, checksums, sig []byte, tag string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.checksums, v.sig, v.tag = checksums, sig, tag
	return v.Err
}

// Last returns the arguments of the last Verify call.
func (v *Verifier) Last() (checksums, sig []byte, tag string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.checksums, v.sig, v.tag
}

// TarGz returns a gzipped tar holding a LICENSE and name with body.
func TarGz(t testing.TB, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, f := range []struct {
		name string
		body []byte
	}{{"LICENSE", []byte("license")}, {name, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Zip returns a zip holding name with body.
func Zip(t testing.TB, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
