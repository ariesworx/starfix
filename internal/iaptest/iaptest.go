// Package iaptest stands in for gcloud in tests of the IAP transport.
//
// Install puts the test binary itself first on PATH under the name gcloud,
// so the client finds and starts it exactly as it would the real one. Main,
// called first in the package's TestMain, makes that binary act as
// `gcloud compute start-iap-tunnel INSTANCE PORT --listen-on-stdin ...`
// when it was started so, and returns at once otherwise.
package iaptest

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Mode is how the fake gcloud behaves.
type Mode string

const (
	// Bridge connects stdin and stdout to 127.0.0.1 at the tunnel's port
	// argument, as IAP connects them to the instance.
	Bridge Mode = "bridge"
	// Deny prints gcloud's refusal, DenyMessage, and exits 1, as gcloud
	// does for a caller without the IAP-secured Tunnel User role.
	Deny Mode = "deny"
	// Hang never answers and ignores stdin closing, so only a kill ends
	// it.
	Hang Mode = "hang"
)

// DenyMessage is what Deny prints on stderr: gcloud's refusal, with a
// terminal escape that a caller must not print raw.
const DenyMessage = "ERROR: (gcloud.compute.start-iap-tunnel) Error while connecting [4033: 'not authorized'].\x1b]0;pwned\x07"

// Install passes these to the fake through its environment.
const (
	modeEnv   = "STARFIX_IAPTEST_MODE"   // the Mode
	recordEnv = "STARFIX_IAPTEST_RECORD" // the file Last reads
)

// Fake is an installed fake gcloud.
type Fake struct {
	record string
}

// Invocation is how the fake was last started.
type Invocation struct {
	PID  int      `json:"pid"`
	Args []string `json:"args"` // after the program name
}

// Install puts a fake gcloud, in mode, first on PATH for the rest of t.
// It uses t.Setenv, so t must not be parallel.
func Install(t testing.TB, mode Mode) *Fake {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("iaptest: %v", err)
	}
	dir := t.TempDir()
	name := "gcloud"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.Symlink(exe, filepath.Join(dir, name)); err != nil {
		t.Fatalf("iaptest: %v", err)
	}
	f := &Fake{record: filepath.Join(dir, "invocation.json")}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(modeEnv, string(mode))
	t.Setenv(recordEnv, f.record)
	return f
}

// Last returns how the fake was last started.
func (f *Fake) Last(t testing.TB) Invocation {
	t.Helper()
	b, err := os.ReadFile(f.record)
	if err != nil {
		t.Fatalf("iaptest: the fake gcloud was not started: %v", err)
	}
	var inv Invocation
	if err := json.Unmarshal(b, &inv); err != nil {
		t.Fatalf("iaptest: %v", err)
	}
	return inv
}

// Main runs the fake gcloud and exits when this process was started as
// one by Install's PATH; otherwise it returns. Call it first in TestMain.
func Main() {
	mode := Mode(os.Getenv(modeEnv))
	if mode == "" {
		return
	}
	os.Exit(fake(mode, os.Args[1:]))
}

// fake acts as gcloud, in mode, with args, and returns its exit code. It
// uses the process's own streams, which internal code otherwise never
// touches, because this process is the gcloud the client started.
func fake(mode Mode, args []string) int {
	if err := record(args); err != nil {
		fmt.Fprintf(os.Stderr, "iaptest: %v\n", err)
		return 2
	}
	switch mode {
	case Deny:
		fmt.Fprintln(os.Stderr, DenyMessage)
		return 1
	case Hang:
		time.Sleep(time.Hour)
		return 1
	case Bridge:
		if len(args) < 4 || args[0] != "compute" || args[1] != "start-iap-tunnel" {
			fmt.Fprintf(os.Stderr, "iaptest: unexpected arguments %q\n", args)
			return 2
		}
		nc, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", args[3])) //nolint:gosec // loopback, the port the test configured
		if err != nil {
			fmt.Fprintf(os.Stderr, "iaptest: %v\n", err)
			return 1
		}
		// Either direction ending ends the tunnel, as with gcloud; the
		// process exiting ends the other copy.
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(nc, os.Stdin); done <- struct{}{} }()
		go func() { _, _ = io.Copy(os.Stdout, nc); done <- struct{}{} }()
		<-done
		return 0
	}
	fmt.Fprintf(os.Stderr, "iaptest: unknown mode %q\n", mode)
	return 2
}

// record writes the invocation for Last, by rename, so a reader never
// sees half of it.
func record(args []string) error {
	path := os.Getenv(recordEnv)
	b, err := json.Marshal(Invocation{PID: os.Getpid(), Args: args})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { //nolint:gosec // the path Install set, in the test's temp directory
		return err
	}
	return os.Rename(tmp, path) //nolint:gosec // as above
}
