// Command sfx is the starfix client: a command line for people and, as
// `sfx mcp`, an MCP server for agents, both talking to starfixd over SSH
// with a pinned host key (docs/design/starfix.md §2). `sfx help` lists
// the commands.
//
// It exits 0 on success, 1 on a failure, 2 on a mistake on the command
// line and 3 when its protocol version is outside the server's range.
// The work is all in [cli.Run]; main only hands it the process's streams,
// environment and version, and a context that ends on an interrupt or
// SIGTERM.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ariesworx/starfix/internal/cli"
	"github.com/ariesworx/starfix/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.Run(ctx, os.Args[1:], cli.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Hostname: os.Hostname, Version: version.Version,
	})
	stop() // not deferred: os.Exit skips deferred calls
	os.Exit(code)
}
