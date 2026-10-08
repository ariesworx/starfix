package cli

import (
	"bytes"
	"testing"

	"github.com/ariesworx/starfix/internal/proto"
)

func n(v int64) *int64 { return &v }

func TestPrintUsage(t *testing.T) {
	opus := proto.ModelTokens{Model: "opus", Tokens: proto.Tokens{Input: n(950), Output: n(12_345), CacheWrite: n(1_500_000),
		CacheWrite1h: n(500_000), CacheRead: n(2_000_000_000)}}
	gemini := proto.ModelTokens{Model: "gemini", Tokens: proto.Tokens{Output: n(0)}}
	blind := proto.ModelTokens{Model: "junie"}
	tests := []struct {
		name string
		own  string
		u    proto.IssueUsage
		want string
	}{
		{"own account, never held", "acme", proto.IssueUsage{Account: "acme"}, "account: acme\n"},
		{"inherited", "", proto.IssueUsage{Account: "acme", AccountFrom: "sf-epic"}, "account: acme (from sf-epic)\n"},
		{"server default", "", proto.IssueUsage{Account: "internal"}, "account: internal (default)\n"},
		{"held, no tokens", "", proto.IssueUsage{Account: "internal", HeldSeconds: 4500},
			"account: internal (default)\nheld 1h\n"},
		{"tokens, split", "acme", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Split: true, Models: []proto.ModelTokens{gemini, opus}},
			"account: acme\nheld 10m; some tokens split by time with other work\n" +
				"tokens gemini: 0 out\n" +
				"tokens opus: 950 in, 12.3k out, 1.5M cache write (500k 1h), 2B cache read\n"},
		{"counts unknown, capped", "acme", proto.IssueUsage{Account: "acme", HeldSeconds: 60, Capped: true, Models: []proto.ModelTokens{blind}},
			"account: acme\nheld 1m; tokens partial\ntokens junie: unknown\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printIssueUsage(&b, tc.own, &tc.u)
			if b.String() != tc.want {
				t.Errorf("printIssueUsage(%+v) =\n%s\nwant\n%s", tc.u, b.String(), tc.want)
			}
		})
	}
	var b bytes.Buffer
	printIssueUsage(&b, "", nil) // a server that sends none prints nothing
	if b.Len() != 0 {
		t.Errorf("printIssueUsage(nil) = %q, want nothing", b.String())
	}
}

func TestPrintDigestUsage(t *testing.T) {
	var b bytes.Buffer
	printDigestUsage(&b, &proto.DigestUsage{HeldSeconds: 6000, Models: []proto.ModelTokens{{Model: "opus", Tokens: proto.Tokens{Input: n(167), Output: n(0)}}},
		Unattributed: []proto.ModelTokens{{Model: "opus", Tokens: proto.Tokens{Input: n(7), Output: n(0)}}}})
	want := "held 1h\ntokens opus: 167 in, 0 out\nunattributed opus: 7 in, 0 out\n"
	if b.String() != want {
		t.Errorf("printDigestUsage =\n%s\nwant\n%s", b.String(), want)
	}
	b.Reset()
	printDigestUsage(&b, nil)
	if b.Len() != 0 {
		t.Errorf("printDigestUsage(nil) = %q, want nothing", b.String())
	}
}
