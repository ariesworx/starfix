//go:build unix

package board

import (
	"os"
	"os/signal"
	"syscall"
)

// notify returns channels that receive when the terminal is resized
// (SIGWINCH) and when the process continues after a stop (SIGCONT), and
// a func that stops both.
func notify() (resized, continued <-chan os.Signal, stop func()) {
	r, c := make(chan os.Signal, 1), make(chan os.Signal, 1)
	signal.Notify(r, syscall.SIGWINCH)
	signal.Notify(c, syscall.SIGCONT)
	return r, c, func() { signal.Stop(r); signal.Stop(c) }
}
