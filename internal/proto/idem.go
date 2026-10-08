package proto

import (
	"crypto/rand"
	"encoding/hex"
)

// NewIdem returns a fresh idempotency key: prefix, a dash and 32 random
// hex digits. A client makes one per request and sends it again only when
// it retries that request. prefix is a short constant such as "cli" or
// "mcp", so the server's events say where a request came from.
func NewIdem(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return prefix + "-" + hex.EncodeToString(b[:])
}
