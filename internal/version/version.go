// Package version holds the build version of both binaries.
package version

// Version is set at build time:
//
//	go build -ldflags "-X github.com/ariesworx/starfix/internal/version.Version=v0.1.0" ./cmd/...
//
// A build without it reports "dev", which never triggers version warnings.
var Version = "dev"
