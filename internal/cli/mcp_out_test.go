package cli

import (
	"bytes"
	"io"
	"testing"
)

// sfx mcp speaks JSON-RPC on standard output, which the text escaping must
// not touch: an escape like \x7f inside a JSON string is invalid JSON.
func TestMCPWritesRawStdout(t *testing.T) {
	var out bytes.Buffer
	r := &runner{env: Env{Stdout: &out, Stderr: io.Discard}}
	r.wrapOutput()
	if r.env.Stdout == io.Writer(&out) {
		t.Fatal("wrapOutput left text output unescaped")
	}
	if r.mcpOut() != io.Writer(&out) {
		t.Errorf("mcpOut() = %T, want the raw standard output", r.mcpOut())
	}
}
