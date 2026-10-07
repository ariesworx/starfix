package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	req, err := Request(7, OpShow, ShowArgs{ID: "sf-abcd1234", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	ok, err := Response(7, WriteResult{ID: "sf-abcd1234", Rev: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refused, err := Response(8, nil, Errf(CodeConflict, "re-read", "changed"))
	if err != nil {
		t.Fatal(err)
	}
	frames := []*Frame{
		{T: FrameBridge, Principal: "alice"},
		{T: FrameHello, Version: "v0.1.0", Proto: 1, Project: "p", Session: "s", Machine: "m"},
		{T: FrameWelcome, Version: "v0.1.0", Min: 1, Max: 2, Session: "s", Latest: "v0.2.0"},
		{T: FrameWelcome, Err: Errf(CodeVersion, "upgrade", "too old")},
		req, ok, refused,
		{T: FrameEvent},
	}
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for _, f := range frames {
		if err := enc.Encode(f); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "\n"); n != len(frames) {
		t.Fatalf("got %d lines, want %d", n, len(frames))
	}
	dec := NewDecoder(&buf)
	for i, want := range frames {
		got, err := dec.Decode()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("frame %d: got %+v, want %+v", i, got, want)
		}
	}
	if _, err := dec.Decode(); !errors.Is(err, io.EOF) {
		t.Fatalf("after last frame: %v, want EOF", err)
	}
}

func TestFrameTerseOnTheWire(t *testing.T) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Encode(&Frame{T: FrameReq, ID: 1, Op: OpReady, Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if got, want := buf.String(), `{"t":"req","id":1,"op":"ready","a":{}}`+"\n"; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestDecodeRejects(t *testing.T) {
	tests := map[string]struct {
		in   string
		want error
	}{
		"not json":   {in: "nope\n"},
		"no type":    {in: `{"id":1}` + "\n"},
		"blank line": {in: "\n"},
		"too large":  {in: `{"t":"req","a":"` + strings.Repeat("x", MaxFrame) + `"}` + "\n", want: ErrFrameTooLarge},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := NewDecoder(strings.NewReader(tc.in)).Decode()
			if err == nil {
				t.Fatal("decoded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestEncodeTooLarge(t *testing.T) {
	big, err := json.Marshal(strings.Repeat("x", MaxFrame))
	if err != nil {
		t.Fatal(err)
	}
	err = NewEncoder(io.Discard).Encode(&Frame{T: FrameReq, Args: big})
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("got %v", err)
	}
}

func TestCheckProto(t *testing.T) {
	tests := []struct {
		name      string
		p, lo, hi int
		ok        bool
		fix       string
	}{
		{"in range", 1, 1, 1, true, ""},
		{"one back", 1, 1, 2, true, ""},
		{"one forward", 3, 2, 3, true, ""},
		{"client too old", 1, 2, 3, false, "sfx upgrade"},
		{"client too new", 4, 2, 3, false, "starfixd upgrade"},
		{"empty range", 1, 2, 1, false, "protocol range"},
		{"zero range", 1, 0, 0, false, "protocol range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := CheckProto(tc.p, tc.lo, tc.hi, "v1.0.0")
			if tc.ok {
				if e != nil {
					t.Fatalf("refused: %v", e)
				}
				return
			}
			if e == nil {
				t.Fatal("accepted")
			}
			if e.Code != CodeVersion || !strings.Contains(e.Fix, tc.fix) {
				t.Fatalf("got %+v, want code version and fix containing %q", e, tc.fix)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"v1.2.3", "v1.2.3", 0, true},
		{"v1.2.3", "v1.2.4", -1, true},
		{"v1.10.0", "v1.9.9", 1, true},
		{"v2.0.0", "v10.0.0", -1, true},
		{"v1.0.0-rc.1", "v1.0.0", -1, true},
		{"v1.0.0", "v1.0.0-rc.1", 1, true},
		{"v1.0.0-rc.1", "v1.0.0-rc.2", -1, true},
		{"v1.0.0+build", "v1.0.0", 0, true},
		{"dev", "v1.0.0", 0, false},
		{"v1.0", "v1.0.0", 0, false},
		{"1.0.0", "v1.0.0", 0, false},
		{"v01.0.0", "v1.0.0", 0, false},
		{"v1.0.0-", "v1.0.0", 0, false},
		{"", "", 0, false},
	}
	for _, tc := range tests {
		cmp, ok := CompareVersions(tc.a, tc.b)
		if cmp != tc.cmp || ok != tc.ok {
			t.Errorf("CompareVersions(%q, %q) = %d, %v; want %d, %v", tc.a, tc.b, cmp, ok, tc.cmp, tc.ok)
		}
	}
}

func TestOlderClientWarning(t *testing.T) {
	tests := []struct{ client, server, want string }{
		{"v0.1.0", "v0.2.0", "starfix v0.1.0 is older than the server (v0.2.0); run `sfx upgrade`"},
		{"v0.2.0", "v0.2.0", ""},
		{"v0.3.0", "v0.2.0", ""},
		{"dev", "v0.2.0", ""},
		{"v0.1.0", "dev", ""},
	}
	for _, tc := range tests {
		if got := OlderClientWarning(tc.client, tc.server); got != tc.want {
			t.Errorf("(%s, %s) = %q, want %q", tc.client, tc.server, got, tc.want)
		}
	}
}

func TestErrorString(t *testing.T) {
	if got := Errf(CodeConflict, "re-read it", "it changed").Error(); got != "it changed; re-read it" {
		t.Fatal(got)
	}
	if got := (&Error{Code: CodeInvalid, Message: "bad"}).Error(); got != "bad" {
		t.Fatal(got)
	}
}

func TestOlderServerWarning(t *testing.T) {
	tests := []struct{ server, latest, want string }{
		{"v0.1.0", "v0.2.0", "the server runs starfixd v0.1.0; v0.2.0 is out: ask the admin to run `starfixd upgrade`"},
		{"v0.2.0", "v0.2.0", ""},
		{"v0.2.0", "", ""},
		{"dev", "v0.2.0", ""},
	}
	for _, tc := range tests {
		if got := OlderServerWarning(tc.server, tc.latest); got != tc.want {
			t.Errorf("(%s, %s) = %q, want %q", tc.server, tc.latest, got, tc.want)
		}
	}
}

func TestEventChanged(t *testing.T) {
	tests := []struct {
		e    Event
		want string
	}{
		{Event{Op: "issue.update", Before: json.RawMessage(`{"title":"a"}`), After: json.RawMessage(`{"title":"b","assignee":"x"}`)}, "assignee, title"},
		{Event{Op: "label.add", After: json.RawMessage(`{"label":"docs"}`)}, "docs"},
		{Event{Op: "dep.remove", Before: json.RawMessage(`{"type":"blocks","to":"sf-b"}`)}, "blocks sf-b"},
		{Event{Op: "issue.create", After: json.RawMessage(`{"title":"Ship"}`)}, `"Ship"`},
		{Event{Op: "comment.add"}, ""},
	}
	for _, tc := range tests {
		if got := tc.e.Changed(); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.e.Op, got, tc.want)
		}
	}
}
