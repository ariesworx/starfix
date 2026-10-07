//go:build unix

package cli

import "syscall"

// noFollow makes open fail on a link instead of following it.
const noFollow = syscall.O_NOFOLLOW
