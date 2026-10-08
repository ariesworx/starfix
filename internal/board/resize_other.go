//go:build !unix

package board

import "os"

// notifyResize returns nil: no signal tells of a resize here, so Run's
// redraw each second reads the size instead.
func notifyResize() (<-chan os.Signal, func()) { return nil, func() {} }
