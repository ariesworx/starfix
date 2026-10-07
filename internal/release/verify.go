package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// Verifier checks that sig is a valid signature over checksums for the
// release tagged tag, and returns nil only if it is. It is the one place
// the signature scheme lives, so it can change without touching callers.
type Verifier interface {
	Verify(ctx context.Context, checksums, sig []byte, tag string) error
}

// ErrSignature means the signature on checksums.txt is missing or did
// not verify.
var ErrSignature = errors.New("signature does not verify")

// ErrNoKey means this build carries no release public key, so nothing it
// downloads can be verified.
var ErrNoKey = errors.New("no release signing key is configured in this build")

// Ed25519 verifies checksums.txt.sig: one line, the standard base64 of a
// raw 64-byte Ed25519 signature over the exact bytes of checksums.txt,
// made by the release workflow (internal/tools/releasekey sign) with the
// key in the release environment's STARFIX_RELEASE_KEY secret.
//
// The signature does not cover the tag. It does not need to: Fetch looks
// the archive up in the signed checksums.txt by a name that carries the
// version, so another release's signed file cannot stand in for this one.
type Ed25519 struct {
	// Keys are the accepted public keys; a signature by any one verifies.
	// Nil uses the built-in list, Keys().
	Keys []ed25519.PublicKey
}

// Verify implements Verifier. It returns nil when sig verifies under any
// of the keys, [ErrNoKey] when there are none, an error wrapping
// [ErrSignature] when sig is malformed or no key matches, and the error
// from [Keys] when the built-in list does not parse.
func (v Ed25519) Verify(_ context.Context, checksums, sig []byte, _ string) error {
	keys := v.Keys
	if keys == nil {
		var err error
		if keys, err = Keys(); err != nil {
			return err
		}
	}
	if len(keys) == 0 {
		return ErrNoKey
	}
	raw, err := DecodeSignature(sig)
	if err != nil {
		return err
	}
	for _, k := range keys {
		// ed25519.Verify panics on a key of the wrong length.
		if len(k) == ed25519.PublicKeySize && ed25519.Verify(k, checksums, raw) {
			return nil
		}
	}
	return fmt.Errorf("%w: no release key matches", ErrSignature)
}

// DecodeSignature parses the contents of a .sig file and returns the raw
// signature. Its error wraps [ErrSignature].
func DecodeSignature(sig []byte) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil {
		return nil, fmt.Errorf("%w: not base64: %w", ErrSignature, err)
	}
	if len(raw) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrSignature, len(raw), ed25519.SignatureSize)
	}
	return raw, nil
}

// EncodeSignature returns the contents of a .sig file for raw: its
// standard base64 and a newline.
func EncodeSignature(raw []byte) []byte {
	return []byte(base64.StdEncoding.EncodeToString(raw) + "\n")
}

// EncodeKey returns k as keys.go lists it: the standard base64 of its
// 32 bytes.
func EncodeKey(k ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(k)
}

// Keys parses releaseKeys, the built-in release public keys.
func Keys() ([]ed25519.PublicKey, error) {
	return ParseKeys(releaseKeys)
}

// ParseKeys parses Ed25519 public keys written as [EncodeKey] writes
// them. One that does not parse fails the whole list.
func ParseKeys(enc []string) ([]ed25519.PublicKey, error) {
	keys := make([]ed25519.PublicKey, 0, len(enc))
	for _, s := range enc {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("release key %q is not a base64 Ed25519 public key", s)
		}
		keys = append(keys, ed25519.PublicKey(b))
	}
	return keys, nil
}
