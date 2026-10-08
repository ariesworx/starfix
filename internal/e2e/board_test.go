package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/board"
	"github.com/ariesworx/starfix/internal/client"
	"github.com/ariesworx/starfix/internal/mcpserver"
	"github.com/ariesworx/starfix/internal/proto"
)

// boardView is a board model fed by Live over a real connection, drawn
// without a terminal.
type boardView struct {
	t       *testing.T
	m       *board.Model
	updates chan board.Update
}

// board starts Live for this user, as `sfx tui` does, until the test
// ends.
func (u *user) board() *boardView {
	t := u.w.t
	t.Helper()
	live := board.NewLive(func(ctx context.Context, onPush func(proto.Push)) (board.Conn, error) {
		c, err := mcpserver.DialRepo(ctx, u.repo, client.Options{Version: "v0.2.0", Session: client.CLISession,
			Machine: "laptop-test", Getenv: func(k string) string { return u.env[k] }, OnPush: onPush})
		if err != nil {
			return nil, err
		}
		return c, nil
	})
	conn, err := live.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b := &boardView{t: t, m: board.New("e2e"), updates: make(chan board.Update)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		live.Run(t.Context(), conn, nil, b.updates)
	}()
	// t.Context ends before cleanups run, which ends Run.
	t.Cleanup(func() { <-done })
	return b
}

func (b *boardView) frame() string {
	return strings.Join(b.m.Frame(120, 30, time.Now(), false), "\n")
}

// until applies updates until the frame holds every one of want.
func (b *boardView) until(what string, want ...string) {
	b.t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		f := b.frame()
		missing := false
		for _, w := range want {
			if !strings.Contains(f, w) {
				missing = true
			}
		}
		if !missing {
			return
		}
		select {
		case u := <-b.updates:
			b.m.Apply(u)
		case <-deadline:
			b.t.Fatalf("timed out waiting for %s; the board shows:\n%s", what, f)
		}
	}
}

// Bob's board follows alice's work from the server's pushed events alone:
// her claim appears under held, with her session, and her close takes it
// off; after the connection drops, the board redials and catches up.
func TestBoardFollowsTheEventStream(t *testing.T) {
	w := newWorld(t, daemonOpts{})
	alice, bob := w.newUser("alice", ""), w.newUser("bob", "")
	id := decode[proto.WriteResult](t, alice.ok("create", "Fix", "the", "parser", "-p", "1", "--json")).ID

	b := bob.board()
	b.until("the first read", "live", "Ready 1", "> "+id+" P1 Fix the parser", "Held 0")

	alice.ok("start", id)
	b.until("alice's claim", "Ready 0", "Held 1", id+" P1 alice/cli 0m Fix the parser", "alice/cli claim.take "+id)

	alice.ok("close", id, "--reason", "fixed")
	b.until("alice's close", "Held 0", "alice/cli issue.close "+id)
	if f := b.frame(); strings.Contains(f, "> "+id) {
		t.Errorf("the closed issue is still listed:\n%s", f)
	}

	w.dropAll()
	b.until("the lost connection", "reconnecting")
	next := decode[proto.WriteResult](t, alice.ok("create", "Write", "the", "docs", "--json")).ID
	b.until("the reconnect and a fresh read", "live", "Ready 1", next+" P2 Write the docs")
}
