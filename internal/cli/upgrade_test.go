package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ariesworx/starfix/internal/release"
	"github.com/ariesworx/starfix/internal/release/releasetest"
)

type upgradeFixture struct {
	gh      *releasetest.Server
	exe     string
	checked []string
	env     Env
}

// newUpgrade serves release latest, signed, with an sfx for linux/amd64 whose
// contents are "sfx <latest>", and installs "sfx <current>" as the exe.
func newUpgrade(t *testing.T, current, latest string, verr error) *upgradeFixture {
	t.Helper()
	f := &upgradeFixture{gh: releasetest.New(t, latest), exe: filepath.Join(t.TempDir(), "sfx")}
	f.gh.AddBinary(t, "sfx", "linux", "amd64", []byte("sfx "+latest))
	if err := os.WriteFile(f.exe, []byte("sfx "+current), 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
	// Without an injected error, the real Ed25519 verifier checks the
	// fake's signature.
	var v release.Verifier = f.gh.Verifier()
	if verr != nil {
		v = &releasetest.Verifier{Err: verr}
	}
	f.env = Env{Getenv: func(string) string { return "" }, Version: current, Upgrader: &Upgrader{
		Releases: f.gh.Client(v),
		Exe:      f.exe,
		GOOS:     "linux", GOARCH: "amd64",
		Check: func(_ context.Context, p, tag string) error {
			f.checked = append(f.checked, tag)
			if b, err := os.ReadFile(p); err != nil || string(b) != "sfx "+tag { //nolint:gosec // test temp dir
				return errors.New("staged binary is not the release's")
			}
			return nil
		},
	}}
	return f
}

func (f *upgradeFixture) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	env := f.env
	env.Stdout, env.Stderr = &out, &errb
	code := Run(context.Background(), args, env)
	return code, out.String(), errb.String()
}

func (f *upgradeFixture) exeHolds(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.exe)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeCheck(t *testing.T) {
	tests := []struct {
		name, current, latest, want string
	}{
		{"behind", "v0.2.0", "v0.3.0", "starfix v0.2.0, latest v0.3.0: run sfx upgrade\n"},
		{"current", "v0.3.0", "v0.3.0", "starfix v0.3.0, latest v0.3.0: up to date\n"},
		{"ahead", "v0.4.0-rc.1", "v0.3.0", "starfix v0.4.0-rc.1, latest v0.3.0: up to date\n"},
		{"dev", "dev", "v0.3.0", "starfix dev, latest v0.3.0: dev build; install a release to upgrade\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUpgrade(t, tt.current, tt.latest, nil)
			code, out, errb := f.run("upgrade", "--check")
			if code != ExitOK || out != tt.want || errb != "" {
				t.Fatalf("exit %d, stdout %q (want %q), stderr %q", code, out, tt.want, errb)
			}
			if got := f.exeHolds(t); got != "sfx "+tt.current {
				t.Errorf("--check changed the binary to %q", got)
			}
		})
	}
}

func TestUpgradeCheckJSON(t *testing.T) {
	f := newUpgrade(t, "v0.2.0", "v0.3.0", nil)
	code, out, _ := f.run("upgrade", "--check", "--json")
	var doc map[string]any
	if code != ExitOK || json.Unmarshal([]byte(out), &doc) != nil {
		t.Fatalf("exit %d, %q", code, out)
	}
	if doc["latest"] != "v0.3.0" || doc["upgrade"] != true {
		t.Errorf("doc = %v", doc)
	}
}

func TestUpgradeAndRollback(t *testing.T) {
	f := newUpgrade(t, "v0.2.0", "v0.3.0", nil)
	code, out, errb := f.run("upgrade")
	if code != ExitOK || !strings.Contains(out, "upgraded v0.2.0 -> v0.3.0") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errb)
	}
	if got := f.exeHolds(t); got != "sfx v0.3.0" {
		t.Fatalf("exe holds %q", got)
	}
	if len(f.checked) != 1 || f.checked[0] != "v0.3.0" {
		t.Errorf("staged binary checked for %v", f.checked)
	}

	// The binary now reports the new version: a second run changes nothing.
	f.env.Version = "v0.3.0"
	code, out, _ = f.run("upgrade")
	if code != ExitOK || out != "starfix v0.3.0 is up to date\n" || f.exeHolds(t) != "sfx v0.3.0" {
		t.Fatalf("second run: exit %d, %q, exe %q", code, out, f.exeHolds(t))
	}

	code, out, errb = f.run("upgrade", "--rollback")
	if code != ExitOK || f.exeHolds(t) != "sfx v0.2.0" {
		t.Fatalf("rollback: exit %d, %q %q, exe %q", code, out, errb, f.exeHolds(t))
	}
	code, _, errb = f.run("upgrade", "--rollback")
	if code != ExitFailure || !strings.Contains(errb, "no previous binary") || !strings.Contains(errb, "fix: nothing to roll back") {
		t.Fatalf("second rollback: exit %d, %q", code, errb)
	}
	if f.exeHolds(t) != "sfx v0.2.0" {
		t.Errorf("refused rollback changed the binary")
	}
}

func TestUpgradeRefusals(t *testing.T) {
	tests := []struct {
		name    string
		current string
		verr    error
		edit    func(t *testing.T, f *upgradeFixture)
		args    []string
		code    int
		stderr  string
	}{
		{name: "dev build", current: "dev", args: []string{"upgrade"}, code: ExitFailure,
			stderr: "development build (dev)"},
		{name: "signature refused", current: "v0.2.0", verr: release.ErrSignature, args: []string{"upgrade"},
			code: ExitFailure, stderr: "failed verification"},
		{name: "no release key in this build", current: "v0.2.0", verr: release.ErrNoKey, args: []string{"upgrade"},
			code: ExitFailure, stderr: "this build cannot verify releases"},
		{name: "unsigned release", current: "v0.2.0", args: []string{"upgrade"}, code: ExitFailure,
			edit: func(_ *testing.T, f *upgradeFixture) { f.gh.Omit[release.SignatureName] = true }, stderr: "is not signed"},
		{name: "checksum mismatch", current: "v0.2.0", args: []string{"upgrade"}, code: ExitFailure,
			edit: func(t *testing.T, f *upgradeFixture) {
				s := f.gh.Checksums()
				f.gh.Sums = &s
				f.gh.AddBinary(t, "sfx", "linux", "amd64", []byte("tampered"))
			}, stderr: "checksum mismatch"},
		{name: "no build for platform", current: "v0.2.0", args: []string{"upgrade"}, code: ExitFailure,
			edit: func(_ *testing.T, f *upgradeFixture) { f.env.Upgrader.GOARCH = "riscv64" }, stderr: "has no sfx_0.3.0_linux_riscv64.tar.gz"},
		{name: "staged binary fails its check", current: "v0.2.0", args: []string{"upgrade"}, code: ExitFailure,
			edit: func(t *testing.T, f *upgradeFixture) {
				f.gh.Tag = "v0.3.0"
				f.gh.AddBinary(t, "sfx", "linux", "amd64", []byte("sfx v9.9.9"))
			}, stderr: "staged binary is not the release's"},
		{name: "check and rollback", current: "v0.2.0", args: []string{"upgrade", "--check", "--rollback"}, code: ExitUsage,
			stderr: "cannot be combined"},
		{name: "arguments", current: "v0.2.0", args: []string{"upgrade", "v0.3.0"}, code: ExitUsage,
			stderr: "takes no arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUpgrade(t, tt.current, "v0.3.0", tt.verr)
			if tt.edit != nil {
				tt.edit(t, f)
			}
			code, out, errb := f.run(tt.args...)
			if code != tt.code || !strings.Contains(errb, tt.stderr) || !strings.Contains(errb, "fix: ") {
				t.Fatalf("exit %d (want %d), stdout %q, stderr %q (want %q and a fix)", code, tt.code, out, errb, tt.stderr)
			}
			if got := f.exeHolds(t); got != "sfx "+tt.current {
				t.Errorf("refused upgrade left %q", got)
			}
			if _, err := os.Stat(f.exe + ".prev"); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("refused upgrade left a .prev: %v", err)
			}
		})
	}
}
