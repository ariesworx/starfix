// Package release finds, downloads and verifies starfix releases, and
// swaps a running binary for a new one.
//
// A release on GitHub carries one archive per binary and platform, a
// checksums.txt (sha256) and checksums.txt.sig, an Ed25519 signature over
// checksums.txt (see Ed25519). Fetch trusts an archive only when that
// signature verifies against a key built into this binary and the
// archive's sha256 is listed in checksums.txt.
package release

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

// Repo is the GitHub repository releases come from.
const Repo = "ariesworx/starfix"

// Asset names shared by every release.
const (
	ChecksumsName = "checksums.txt"
	SignatureName = "checksums.txt.sig"
)

// Size limits for what a release may serve.
const (
	maxMeta      = 1 << 20
	maxChecksums = 64 << 10
	maxSig       = 1 << 10
	maxArchive   = 256 << 20
	maxBinary    = 256 << 20
)

// TagPattern is what a release tag may look like.
var TagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]{1,32})?$`)

// ErrNotFound means the release or one of its assets does not exist.
var ErrNotFound = errors.New("not found")

// ErrMismatch means a download's sha256 differs from checksums.txt.
var ErrMismatch = errors.New("checksum mismatch")

// Client reads releases from the GitHub REST API.
type Client struct {
	// BaseURL is the API root. Default https://api.github.com.
	BaseURL string
	// Repo is owner/name. Default Repo.
	Repo string
	// HTTP makes the requests. Default: the default transport, which
	// honors HTTPS_PROXY, with a 5-minute overall timeout.
	HTTP *http.Client
	// Verifier checks the signature on checksums.txt. Required by Fetch.
	Verifier Verifier
}

// Release is one published release.
type Release struct {
	Tag    string  `json:"tag_name"`
	Assets []Asset `json:"assets"`
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

func (c *Client) base() string {
	if c.BaseURL == "" {
		return "https://api.github.com"
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) repo() string {
	if c.Repo == "" {
		return Repo
	}
	return c.Repo
}

func (c *Client) http() *http.Client {
	if c.HTTP == nil {
		return &http.Client{Timeout: 5 * time.Minute}
	}
	return c.HTTP
}

// Latest returns the newest published release (GitHub excludes drafts and
// prereleases).
func (c *Client) Latest(ctx context.Context) (*Release, error) {
	return c.release(ctx, "/repos/"+c.repo()+"/releases/latest")
}

// ByTag returns the release tagged tag.
func (c *Client) ByTag(ctx context.Context, tag string) (*Release, error) {
	if !TagPattern.MatchString(tag) {
		return nil, fmt.Errorf("release tag %q is not of the form vMAJOR.MINOR.PATCH", tag)
	}
	return c.release(ctx, "/repos/"+c.repo()+"/releases/tags/"+tag)
}

func (c *Client) release(ctx context.Context, p string) (*Release, error) {
	b, err := c.get(ctx, c.base()+p, maxMeta, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("release metadata: %w", err)
	}
	var r Release
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("release metadata: %w", err)
	}
	if !TagPattern.MatchString(r.Tag) {
		return nil, fmt.Errorf("release metadata: tag %q is not of the form vMAJOR.MINOR.PATCH", r.Tag)
	}
	return &r, nil
}

func (c *Client) get(ctx context.Context, u string, limit int64, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "starfix-upgrade")
	if accept != "" {
		req.Header.Set("Accept", accept)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	res, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	switch {
	case res.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", redact(u), ErrNotFound)
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d", redact(u), res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", redact(u), err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s: larger than %d bytes", redact(u), limit)
	}
	return b, nil
}

// redact drops any query string, which might carry a signed token.
func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "download"
	}
	p.RawQuery, p.Fragment, p.User = "", "", nil
	return p.String()
}

// ArchiveName is the archive holding bin for goos/goarch in release tag:
// <bin>_<version>_<os>_<arch>.tar.gz (.zip on Windows), version without
// the leading v.
func ArchiveName(bin, tag, goos, goarch string) string {
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s_%s_%s_%s%s", bin, strings.TrimPrefix(tag, "v"), goos, goarch, ext)
}

func (r *Release) asset(name string) (Asset, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, nil
		}
	}
	return Asset{}, fmt.Errorf("release %s has no %s: %w", r.Tag, name, ErrNotFound)
}

// Fetch downloads bin for goos/goarch from rel and returns the executable's
// bytes, after verifying the signature on checksums.txt and the archive's
// checksum against it.
func (c *Client) Fetch(ctx context.Context, rel *Release, bin, goos, goarch string) ([]byte, error) {
	if c.Verifier == nil {
		return nil, errors.New("release: no signature verifier")
	}
	name := ArchiveName(bin, rel.Tag, goos, goarch)
	arch, err := rel.asset(name)
	if err != nil {
		return nil, err
	}
	sums, err := c.download(ctx, rel, ChecksumsName, maxChecksums)
	if err != nil {
		return nil, err
	}
	sig, err := c.download(ctx, rel, SignatureName, maxSig)
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("%w: release %s is not signed (no %s)", ErrSignature, rel.Tag, SignatureName)
	}
	if err != nil {
		return nil, err
	}
	if err := c.Verifier.Verify(ctx, sums, sig, rel.Tag); err != nil {
		return nil, fmt.Errorf("%s of %s: %w", ChecksumsName, rel.Tag, err)
	}
	want, err := lookupSum(sums, name)
	if err != nil {
		return nil, err
	}
	data, err := c.downloadAsset(ctx, arch, maxArchive)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(data)
	if !bytes.Equal(got[:], want) {
		return nil, fmt.Errorf("%s: %w: got %x, %s lists %x", name, ErrMismatch, got, ChecksumsName, want)
	}
	exe := bin
	if goos == "windows" {
		exe += ".exe"
	}
	if strings.HasSuffix(name, ".zip") {
		return fromZip(data, exe)
	}
	return fromTarGz(data, exe)
}

func (c *Client) download(ctx context.Context, rel *Release, name string, limit int64) ([]byte, error) {
	a, err := rel.asset(name)
	if err != nil {
		return nil, err
	}
	return c.downloadAsset(ctx, a, limit)
}

func (c *Client) downloadAsset(ctx context.Context, a Asset, limit int64) ([]byte, error) {
	u, err := url.Parse(a.URL)
	if err != nil {
		return nil, fmt.Errorf("asset %s: %w", a.Name, err)
	}
	b, err := url.Parse(c.base())
	if err != nil {
		return nil, fmt.Errorf("base URL: %w", err)
	}
	// Downloads use the API's scheme: https in production; a test server
	// may be plain http.
	if u.Scheme != b.Scheme || u.Host == "" {
		return nil, fmt.Errorf("asset %s: URL %s is not %s", a.Name, redact(a.URL), b.Scheme)
	}
	data, err := c.get(ctx, a.URL, limit, "")
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", a.Name, err)
	}
	return data, nil
}

// lookupSum finds name in a sha256sum-format file.
func lookupSum(sums []byte, name string) ([]byte, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 || strings.TrimPrefix(f[1], "*") != name {
			continue
		}
		h, err := hex.DecodeString(f[0])
		if err != nil || len(h) != sha256.Size {
			return nil, fmt.Errorf("%s: bad sha256 for %s", ChecksumsName, name)
		}
		return h, nil
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", ChecksumsName, err)
	}
	return nil, fmt.Errorf("%s does not list %s: %w", ChecksumsName, name, ErrNotFound)
}

func fromTarGz(data []byte, exe string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("archive holds no %s: %w", exe, ErrNotFound)
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == exe {
			return readLimited(tr, exe)
		}
	}
}

func fromZip(data []byte, exe string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("archive: %w", err)
	}
	for _, f := range zr.File {
		if !f.Mode().IsRegular() || path.Base(f.Name) != exe {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		b, err := readLimited(rc, exe)
		return b, errors.Join(err, rc.Close())
	}
	return nil, fmt.Errorf("archive holds no %s: %w", exe, ErrNotFound)
}

func readLimited(r io.Reader, name string) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxBinary+1))
	if err != nil {
		return nil, fmt.Errorf("archive %s: %w", name, err)
	}
	if len(b) > maxBinary {
		return nil, fmt.Errorf("archive %s: larger than %d bytes", name, maxBinary)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("archive %s is empty", name)
	}
	return b, nil
}
