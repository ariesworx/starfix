package release

// releaseKeys are the release signing public keys: the standard base64 of
// the raw 32 bytes, as `go run ./internal/tools/releasekey gen` prints
// them. A signature by any one is accepted, so a rotation ships the new
// key beside the old one for at least one release before the old one is
// removed (RELEASING.md).
//
// Empty until the maintainer generates the first key: until then every
// verification fails closed with ErrNoKey.
var releaseKeys = []string{}
