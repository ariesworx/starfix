package version

import (
	"os"
	"regexp"
	"testing"
)

// setting returns the first submatch of pattern in the file at path.
func setting(t *testing.T, path, pattern string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a file of this repository, named by the test
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(pattern).FindSubmatch(b)
	if m == nil {
		t.Fatalf("%s matches no %s", path, pattern)
	}
	return string(m[1])
}

const (
	ciFile     = "../../.github/workflows/ci.yml"
	dockerFile = "../../deploy/docker/Dockerfile"
)

// TestDoltMatchesCI keeps the pin and the version CI tests against equal.
func TestDoltMatchesCI(t *testing.T) {
	if got := setting(t, ciFile, `(?m)^\s*DOLT_VERSION:\s*(\S+)\s*$`); got != Dolt {
		t.Errorf("ci.yml DOLT_VERSION is %s, version.Dolt is %s; change both together", got, Dolt)
	}
}

// TestDoltMatchesDocker keeps the Docker test server's Dolt at the pin,
// and its amd64 checksum equal to the one CI verifies.
func TestDoltMatchesDocker(t *testing.T) {
	if got := setting(t, dockerFile, `(?m)^ARG DOLT_VERSION=(\S+)$`); got != Dolt {
		t.Errorf("Dockerfile DOLT_VERSION is %s, version.Dolt is %s; change both together", got, Dolt)
	}
	want := setting(t, ciFile, `(?m)^\s*DOLT_SHA256:\s*(\S+)\s*$`)
	if got := setting(t, dockerFile, `(?m)^ARG DOLT_SHA256_AMD64=(\S+)$`); got != want {
		t.Errorf("Dockerfile DOLT_SHA256_AMD64 is %s, ci.yml DOLT_SHA256 is %s; change both together", got, want)
	}
}
