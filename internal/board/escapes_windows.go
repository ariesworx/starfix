package board

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// enableEscapes turns on escape sequences on the console that fd is,
// which Windows consoles leave off unless asked, and returns the func
// that puts the mode back.
func enableEscapes(fd int) (func(), error) {
	h := windows.Handle(fd)
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return nil, fmt.Errorf("read console mode: %w", err)
	}
	if err := windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		return nil, fmt.Errorf("enable escape sequences on this console: %w", err)
	}
	return func() { _ = windows.SetConsoleMode(h, mode) }, nil
}
