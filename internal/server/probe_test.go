package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbe(t *testing.T) {
	s := newServer(t)
	dir, err := os.MkdirTemp("", "sfp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "run", "d.sock")
	l, err := Listen(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, l) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	if v, err := Probe(t.Context(), sock, project, "v0.2.0"); err != nil || v != "v0.2.0" {
		t.Fatalf("Probe = %q, %v", v, err)
	}
	if _, err := Probe(t.Context(), sock, "00000000-0000-4000-8000-000000000999", "v0.2.0"); err == nil ||
		!strings.Contains(err.Error(), "not served by this server") {
		t.Fatalf("Probe(other project) = %v", err)
	}
	if _, err := Probe(t.Context(), filepath.Join(dir, "none.sock"), project, "v0.2.0"); err == nil {
		t.Fatal("Probe of no daemon succeeded")
	}
}
