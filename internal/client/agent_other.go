//go:build !windows

package client

import (
	"errors"
	"io"
	"net"
)

// dialAgent connects to ssh-agent's unix socket named by SSH_AUTH_SOCK.
func dialAgent(getenv func(string) string) (io.ReadWriteCloser, error) {
	sock := getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, errors.New("SSH_AUTH_SOCK is not set")
	}
	return net.Dial("unix", sock)
}
