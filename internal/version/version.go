// Package version holds the build version of both binaries, and the Dolt
// release they are tested with.
//
// [Version] is what sfx and starfixd print, send in the handshake and
// compare with releases. [Dolt] is the pin that tests keep equal to CI's
// Dolt and to the dolt on PATH.
package version

// Version is set at build time:
//
//	go build -ldflags "-X github.com/ariesworx/starfix/internal/version.Version=v0.1.0" ./cmd/...
//
// A build without it reports "dev", which never triggers version warnings.
var Version = "dev"

// Dolt is the Dolt release this starfix release is tested with. CI installs
// the same version (DOLT_VERSION in .github/workflows/ci.yml; a test keeps
// the two equal), and the release notes name it.
const Dolt = "2.4.2"
