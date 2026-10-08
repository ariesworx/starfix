package proto

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A pushed inbox item or event is an evt frame with no id, the payload
// under "e"; a resync is the op alone. Decoding gives back the push.
func TestPushFrames(t *testing.T) {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	item := InboxItem{ID: 7, Kind: "mention", Issue: "sf-abcd1234", Body: "@bob look", From: "alice", At: at}
	tests := []struct {
		name string
		push Push
		wire string
	}{
		{"item", Push{Op: EvInbox, Item: &item},
			`{"t":"evt","op":"inbox","e":{"id":7,"kind":"mention","issue":"sf-abcd1234","body":"@bob look","from":"alice","at":"2026-10-07T12:00:00Z"}}`},
		{"resync", Push{Op: EvResync}, `{"t":"evt","op":"resync"}`},
		{"event", Push{Op: EvEvent, Event: &Event{Seq: 12, At: at, Principal: "alice", Session: "s1", Machine: "laptop",
			Op: "claim.take", Issue: "sf-abcd1234"}},
			`{"t":"evt","op":"event","e":{"seq":12,"at":"2026-10-07T12:00:00Z","principal":"alice","session":"s1","machine":"laptop","op":"claim.take","issue":"sf-abcd1234"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := tc.push.Frame()
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if err := NewEncoder(&buf).Encode(f); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(buf.String()); got != tc.wire {
				t.Errorf("push %s on the wire = %s, want %s", tc.name, got, tc.wire)
			}
			back, err := NewDecoder(&buf).Decode()
			if err != nil {
				t.Fatal(err)
			}
			got, err := DecodePush(back)
			if err != nil || !reflect.DeepEqual(got, tc.push) {
				t.Errorf("DecodePush = %+v, %v; want %+v", got, err, tc.push)
			}
		})
	}
	for _, f := range []*Frame{{T: FrameRes}, {T: FrameEvent, Op: EvInbox}, {T: FrameEvent, Op: EvInbox, E: json.RawMessage(`[`)},
		{T: FrameEvent, Op: EvEvent}, {T: FrameEvent, Op: EvEvent, E: json.RawMessage(`"x"`)}} {
		if _, err := DecodePush(f); err == nil {
			t.Errorf("DecodePush(%+v) succeeded", f)
		}
	}
}
