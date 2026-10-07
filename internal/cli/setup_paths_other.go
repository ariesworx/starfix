//go:build !unix

package cli

// noFollow is 0 where open has no such flag; resolve's checks stand alone.
const noFollow = 0
