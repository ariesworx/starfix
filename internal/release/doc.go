// Package release finds, downloads and verifies starfix releases, and
// swaps a running binary for a new one.
//
// A release on GitHub carries one archive per binary and platform
// ([ArchiveName]), a checksums.txt of their sha256 sums, and
// checksums.txt.sig, a signature over checksums.txt. [Client.Fetch]
// returns an executable only after its [Verifier] accepts that signature
// (in production, [Ed25519] with the keys built into this binary) and the
// archive's sha256 matches the one checksums.txt lists. Every download is
// capped in size, and assets must be served over the API's own scheme.
// A Client's methods do not modify it, so one Client may serve several
// goroutines.
//
// [Swap] installs the new executable over the running one and keeps the
// one it replaced for [Swap.Rollback]. It works on files beside the
// executable and takes no lock, so callers run one swap at a time.
//
// The design is docs/design/starfix.md §11, whose As-built notes cover
// signing; RELEASING.md covers the release workflow and key rotation.
package release
