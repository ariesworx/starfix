package proto

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// ParseDuration reads a positive duration in Go's syntax (90m, 1h30m) or
// as whole days (7d) or weeks (2w), as leases and digest windows take it.
func ParseDuration(s string) (time.Duration, error) {
	bad := errors.New("not a duration such as 15m, 8h, 2d or 1w")
	if s == "" {
		return 0, bad
	}
	if unit := s[len(s)-1]; unit == 'd' || unit == 'w' {
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || n < 1 || n > 10000 || strings.HasPrefix(s, "+") {
			return 0, bad
		}
		day := 24 * time.Hour
		if unit == 'w' {
			day *= 7
		}
		return time.Duration(n) * day, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, bad
	}
	return d, nil
}
