//go:build !unix

package board

import "os"

// notify returns nil channels: no signal tells of a resize or a continue
// here, so Run's redraw each second reads the size instead.
func notify() (resized, continued <-chan os.Signal, stop func()) { return nil, nil, func() {} }
