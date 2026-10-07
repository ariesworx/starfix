//go:build windows

package client

import (
	"io"
	"os"
	"strings"
)

// defaultAgentPipe is where the OpenSSH for Windows agent listens.
const defaultAgentPipe = `\\.\pipe\openssh-ssh-agent`

// dialAgent opens the Windows OpenSSH agent's named pipe, or the pipe named
// by SSH_AUTH_SOCK when it names one.
func dialAgent(getenv func(string) string) (io.ReadWriteCloser, error) {
	pipe := defaultAgentPipe
	if sock := getenv("SSH_AUTH_SOCK"); strings.HasPrefix(sock, `\\.\pipe\`) {
		pipe = sock
	}
	return os.OpenFile(pipe, os.O_RDWR, 0) //nolint:gosec // the agent's well-known pipe
}
