package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrNoPrevious means there is no <exe>.prev to roll back to.
var ErrNoPrevious = errors.New("no previous binary")

// Executable returns the running program's path with symlinks resolved,
// so a swap replaces the file itself, not a link to it.
func Executable() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate this executable: %w", err)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("locate this executable: %w", err)
	}
	return r, nil
}

// Swap replaces an executable in place and keeps the one it replaced as
// <exe>.prev for Rollback.
type Swap struct {
	// Exe is the executable's path, symlinks resolved.
	Exe string
	// GOOS selects Windows behavior. Default runtime.GOOS.
	GOOS string
	// Check, when set, runs against the staged new binary before it is
	// installed; an error stops the swap.
	Check func(ctx context.Context, path string) error
}

// Prev is where the replaced binary is kept.
func (s Swap) Prev() string { return s.Exe + ".prev" }

func (s Swap) windows() bool {
	if s.GOOS == "" {
		return runtime.GOOS == "windows"
	}
	return s.GOOS == "windows"
}

// Install writes bin beside Exe, checks it, copies Exe to Prev and renames
// the new file over Exe. A failure before the final rename leaves Exe
// untouched.
func (s Swap) Install(ctx context.Context, bin []byte) (err error) {
	fi, err := os.Stat(s.Exe)
	if err != nil {
		return fmt.Errorf("current binary: %w", err)
	}
	mode := fi.Mode().Perm() | 0o100
	tmp, err := s.stage(bin, mode)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if s.Check != nil {
		if err := s.Check(ctx, tmp); err != nil {
			return fmt.Errorf("new binary: %w", err)
		}
	}
	if err := s.keepPrevious(mode); err != nil {
		return err
	}
	return s.replace(tmp)
}

// Rollback moves Prev back over Exe. Prev is consumed: a second rollback
// finds nothing to restore and returns ErrNoPrevious.
func (s Swap) Rollback() error {
	if _, err := os.Stat(s.Prev()); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w at %s", ErrNoPrevious, s.Prev())
		}
		return fmt.Errorf("previous binary: %w", err)
	}
	return s.replace(s.Prev())
}

// stage writes bin to a new file in Exe's directory, so the final rename
// stays on one filesystem.
func (s Swap) stage(bin []byte, mode fs.FileMode) (string, error) {
	ext := ""
	if s.windows() {
		ext = ".exe"
	}
	f, err := os.CreateTemp(filepath.Dir(s.Exe), "."+filepath.Base(s.Exe)+".new-*"+ext)
	if err != nil {
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	_, werr := f.Write(bin)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(werr, serr, cerr, os.Chmod(f.Name(), mode)); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	return f.Name(), nil
}

// keepPrevious copies Exe to Prev through a temporary file, so Prev is
// always a whole binary.
func (s Swap) keepPrevious(mode fs.FileMode) error {
	src, err := os.Open(s.Exe)
	if err != nil {
		return fmt.Errorf("keep previous binary: %w", err)
	}
	defer func() { _ = src.Close() }()
	f, err := os.CreateTemp(filepath.Dir(s.Exe), "."+filepath.Base(s.Exe)+".prev-*")
	if err != nil {
		return fmt.Errorf("keep previous binary: %w", err)
	}
	_, cperr := io.Copy(f, src)
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(cperr, serr, cerr, os.Chmod(f.Name(), mode), os.Rename(f.Name(), s.Prev())); err != nil {
		_ = os.Remove(f.Name())
		return fmt.Errorf("keep previous binary: %w", err)
	}
	return nil
}

// replace renames src over Exe. Windows cannot overwrite a running
// executable but can rename it, so there Exe moves to <exe>.old first and
// moves back if the second rename fails.
func (s Swap) replace(src string) error {
	if !s.windows() {
		if err := os.Rename(src, s.Exe); err != nil {
			return fmt.Errorf("install new binary: %w", err)
		}
		return nil
	}
	old := s.Exe + ".old"
	if err := os.Remove(old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s left by the last upgrade (is an old process still running?): %w", old, err)
	}
	if err := os.Rename(s.Exe, old); err != nil {
		return fmt.Errorf("move the running binary aside: %w", err)
	}
	if err := os.Rename(src, s.Exe); err != nil {
		return errors.Join(fmt.Errorf("install new binary: %w", err), os.Rename(old, s.Exe))
	}
	return nil
}

// VersionCheck returns a Swap.Check that runs `<path> version` and wants
// the second word of its output (starfix's and starfixd's version lines
// both read "<name> <version> (protocol …)") to be want.
func VersionCheck(want string) func(context.Context, string) error {
	return func(ctx context.Context, path string) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var out bytes.Buffer
		cmd := exec.CommandContext(ctx, path, "version") //nolint:gosec // path is the verified binary just staged
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s version: %w", filepath.Base(path), err)
		}
		if f := strings.Fields(out.String()); len(f) < 2 || f[1] != want {
			return fmt.Errorf("reports %q, want version %s", strings.TrimSpace(out.String()), want)
		}
		return nil
	}
}
