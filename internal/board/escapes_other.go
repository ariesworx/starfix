//go:build !windows

package board

// enableEscapes does nothing: Unix terminals take escape sequences as
// they are.
func enableEscapes(int) (func(), error) { return func() {}, nil }
