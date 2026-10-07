//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Setting up a project whose server is behind IAP warns, with a fix, when
// gcloud is not on PATH, since every connection will need it; it still
// sets the agent up.
func TestSetupWarnsWithoutGcloud(t *testing.T) {
	const iap = "  iap:\n    project: example-project\n    zone: us-central1-a\n    instance: starfix-1\n"
	const warning = "sfx: server.iap in .starfix.yaml needs gcloud, which is not on PATH\nfix: install the Google Cloud CLI"
	withGcloud := t.TempDir()
	if err := os.WriteFile(filepath.Join(withGcloud, "gcloud"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // an executable stand-in
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		config string
		path   string
		warn   bool
	}{
		{name: "iap without gcloud", config: testConfig + iap, path: t.TempDir(), warn: true},
		{name: "iap with gcloud", config: testConfig + iap, path: withGcloud},
		{name: "no iap", config: testConfig, path: t.TempDir()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.path)
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, ".starfix.yaml"), []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			code, _, stderr := runIn(t, t.TempDir(), "-C", root, "setup", "codex")
			if code != 0 || strings.Contains(stderr, warning) != tc.warn {
				t.Errorf("sfx setup codex (PATH %s) = exit %d, stderr %q; want exit 0, warning %v", tc.path, code, stderr, tc.warn)
			}
		})
	}
}
