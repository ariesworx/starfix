package version

import (
	"os"
	"regexp"
	"testing"
)

// TestDoltMatchesCI keeps the pin and the version CI tests against equal.
func TestDoltMatchesCI(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*DOLT_VERSION:\s*(\S+)\s*$`).FindSubmatch(b)
	if m == nil {
		t.Fatal("ci.yml sets no DOLT_VERSION")
	}
	if got := string(m[1]); got != Dolt {
		t.Errorf("ci.yml DOLT_VERSION is %s, version.Dolt is %s; change both together", got, Dolt)
	}
}
