package cli

import (
	"strconv"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/ariesworx/starfix/internal/proto"
)

// pushes turns "1 r 2" into inbox item #1, a resync and item #2.
func pushes(t *testing.T, s string) []proto.Push {
	t.Helper()
	var out []proto.Push
	for f := range strings.FieldsSeq(s) {
		if f == "r" {
			out = append(out, proto.Push{Op: proto.EvResync})
			continue
		}
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			t.Fatalf("bad push %q", f)
		}
		out = append(out, proto.Push{Op: proto.EvInbox, Item: &proto.InboxItem{ID: id}})
	}
	return out
}

// A watch queue never blocks the connection's reader. When the printing
// loop falls behind, the loop still takes a resync where the pushes that
// did not fit would have been, so it says items were missed and watches
// again, even when the server's own resync is among them.
func TestWatchQueue(t *testing.T) {
	tests := []struct {
		name string
		put  string // put before the loop takes any: item ids, r for a resync
		take int    // how many the loop then takes
		more string // put after that
		want string // everything the loop takes, in order
	}{
		{name: "what fits passes through", put: "1 2 3", want: "1 2 3"},
		{name: "a server resync passes through", put: "1 r 2", want: "1 r 2"},
		{name: "falling behind queues one resync for what it drops", put: "1 2 3 4 5 6", want: "1 2 3 r"},
		{name: "a server resync past a full queue still arrives", put: "1 2 3 4 r", want: "1 2 3 r"},
		{name: "items flow again once there is room", put: "1 2 3 4 5 6", take: 2, more: "7 8 9", want: "1 2 3 r 7 r"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// In a bubble, a put that blocked would fail the test as a
			// deadlock instead of hanging it.
			synctest.Test(t, func(t *testing.T) {
				q := newWatchQueue(4)
				var got []string
				take := func(n int) {
					for range n {
						p := <-q.c
						if p.Item != nil {
							got = append(got, strconv.FormatInt(p.Item.ID, 10))
						} else {
							got = append(got, "r")
						}
					}
				}
				for _, p := range pushes(t, tc.put) {
					q.put(p)
				}
				take(tc.take)
				for _, p := range pushes(t, tc.more) {
					q.put(p)
				}
				take(len(q.c))
				if g := strings.Join(got, " "); g != tc.want {
					t.Errorf("put %q, took %d, put %q: the loop took %q, want %q", tc.put, tc.take, tc.more, g, tc.want)
				}
			})
		})
	}
}
