package release

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // an executable fixture
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// entries lists dir, so tests can see no staging file was left behind.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range des {
		names = append(names, d.Name())
	}
	return names
}

func TestSwapInstallRollback(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "sfx")
			writeExe(t, exe, "old")
			s := Swap{Exe: exe, GOOS: goos}

			if err := s.Install(t.Context(), []byte("new")); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, exe); got != "new" {
				t.Errorf("exe = %q after install, want new", got)
			}
			if got := readFile(t, s.Prev()); got != "old" {
				t.Errorf("prev = %q, want old", got)
			}
			fi, err := os.Stat(exe)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
				t.Errorf("exe mode = %v, want 0755", fi.Mode().Perm())
			}
			want := []string{"sfx", "sfx.prev"}
			if goos == "windows" {
				want = []string{"sfx", "sfx.old", "sfx.prev"}
			}
			if got := entries(t, dir); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Errorf("dir holds %v, want %v", got, want)
			}

			// A second install over a leftover .old still works.
			if err := s.Install(t.Context(), []byte("newer")); err != nil {
				t.Fatal(err)
			}
			if readFile(t, exe) != "newer" || readFile(t, s.Prev()) != "new" {
				t.Errorf("second install: exe %q, prev %q", readFile(t, exe), readFile(t, s.Prev()))
			}

			if err := s.Rollback(); err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, exe); got != "new" {
				t.Errorf("exe = %q after rollback, want new", got)
			}
			if err := s.Rollback(); !errors.Is(err, ErrNoPrevious) {
				t.Errorf("second rollback = %v, want ErrNoPrevious", err)
			}
			if got := readFile(t, exe); got != "new" {
				t.Errorf("exe = %q after a refused rollback, want new", got)
			}
		})
	}
}

func TestSwapCheckFailureLeavesExe(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "starfixd")
	writeExe(t, exe, "old")
	var staged string
	s := Swap{Exe: exe, Check: func(_ context.Context, p string) error {
		staged = p
		if got := readFile(t, p); got != "broken" {
			t.Errorf("check saw %q", got)
		}
		return errors.New("exec format error")
	}}
	err := s.Install(t.Context(), []byte("broken"))
	if err == nil || !strings.Contains(err.Error(), "exec format error") {
		t.Fatalf("Install = %v, want the check's error", err)
	}
	if filepath.Dir(staged) != dir {
		t.Errorf("staged in %s, want beside the exe in %s", filepath.Dir(staged), dir)
	}
	if got := readFile(t, exe); got != "old" {
		t.Errorf("exe = %q, want it untouched", got)
	}
	if got := entries(t, dir); len(got) != 1 {
		t.Errorf("dir holds %v, want only the exe", got)
	}
}

func TestSwapMissingExe(t *testing.T) {
	s := Swap{Exe: filepath.Join(t.TempDir(), "gone")}
	if err := s.Install(t.Context(), []byte("x")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Install = %v, want not-exist", err)
	}
}

func TestVersionCheck(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as the binary")
	}
	dir := t.TempDir()
	tests := []struct {
		name, script, want string
		ok                 bool
	}{
		{"match", "echo 'starfix v1.2.0 (protocol 1)'", "v1.2.0", true},
		{"server match", "echo 'starfixd v1.2.0 (protocol 1-1)'", "v1.2.0", true},
		{"other version", "echo 'starfix v1.1.0 (protocol 1)'", "v1.2.0", false},
		{"fails", "exit 3", "v1.2.0", false},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(dir, "bin"+strconv.Itoa(i))
			writeExe(t, p, "#!/bin/sh\n"+tt.script+"\n")
			err := VersionCheck(tt.want)(t.Context(), p)
			if (err == nil) != tt.ok {
				t.Errorf("VersionCheck = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}
