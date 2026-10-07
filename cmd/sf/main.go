// Command sf is the starfix client: issue tracking from the command
// line, over SSH to starfixd.
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
	stop()
	os.Exit(code)
}
