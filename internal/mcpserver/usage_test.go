package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func n64(v int64) *int64 { return &v }

func TestShowAndDigestCarryUsage(t *testing.T) {
	opus := proto.ModelTokens{Model: "opus", Tokens: proto.Tokens{Input: n64(950), Output: n64(12_345), CacheWrite: n64(1_500_000),
		CacheRead: n64(2_000_000_000)}}
	gemini := proto.ModelTokens{Model: "gemini", Tokens: proto.Tokens{Output: n64(0)}}
	// Many models, as a hostile principal could report: the line keeps
	// the largest few and counts the rest.
	var many []proto.ModelTokens
	for i := range 300 {
		many = append(many, proto.ModelTokens{Model: fmt.Sprintf("m%03d-%s", i, strings.Repeat("x", 100)),
			Tokens: proto.Tokens{Input: n64(int64(i))}})
	}
	until := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name               string
		show               *proto.IssueUsage
		digest             *proto.DigestUsage
		account, showLine  string
		digestLine         string
		showHas, digestHas string
	}{
		{name: "split tokens", show: &proto.IssueUsage{Account: "acme", AccountFrom: "sf-0", HeldSeconds: 4500, Split: true,
			Models: []proto.ModelTokens{gemini, opus}},
			digest: &proto.DigestUsage{HeldSeconds: 6000, Models: []proto.ModelTokens{opus},
				Unattributed: []proto.ModelTokens{{Model: "opus", Tokens: proto.Tokens{Input: n64(7)}}}},
			account:    "acme",
			showLine:   "held 1h; opus 950 in, 12.3k out, 1.5M cache write, 2B cache read; gemini 0 out; split by time",
			digestLine: "held 1h; opus 950 in, 12.3k out, 1.5M cache write, 2B cache read; unattributed: opus 7 in"},
		{name: "never held", show: &proto.IssueUsage{Account: "internal"}, account: "internal"},
		{name: "many models", show: &proto.IssueUsage{Account: "internal", HeldSeconds: 60, Models: many},
			digest:  &proto.DigestUsage{HeldSeconds: 60, Models: many, Unattributed: many},
			account: "internal", showHas: "; 295 more models", digestHas: "; 295 more models"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeConn{reply: func(op string, _ any) (any, error) {
				switch op {
				case proto.OpShow:
					return proto.ShowResult{Issue: proto.Issue{ID: "sf-1", Title: "t"}, Usage: tc.show}, nil
				case proto.OpDigest:
					return proto.DigestResult{Since: until.Add(-time.Hour), Until: until, Usage: tc.digest}, nil
				}
				return nil, errors.New("unexpected " + op)
			}}
			cs, _ := connect(t, f)
			raw := text(t, callTool(t, cs, "show", map[string]any{"id": "sf-1"}))
			var is Issue
			if err := json.Unmarshal([]byte(raw), &is); err != nil {
				t.Fatal(err)
			}
			if is.Account != tc.account || tc.showHas == "" && is.Usage != tc.showLine || !strings.Contains(is.Usage, tc.showHas) {
				t.Errorf("show: account %q usage %q; want %q, %q (containing %q)", is.Account, is.Usage, tc.account, tc.showLine, tc.showHas)
			}
			if Tokens([]byte(raw)) > MaxResultTokens {
				t.Errorf("show: %d tokens, over %d", Tokens([]byte(raw)), MaxResultTokens)
			}
			raw = text(t, callTool(t, cs, "digest", map[string]any{}))
			var d Digest
			if err := json.Unmarshal([]byte(raw), &d); err != nil {
				t.Fatal(err)
			}
			if tc.digestHas == "" && d.Usage != tc.digestLine || !strings.Contains(d.Usage, tc.digestHas) {
				t.Errorf("digest usage %q; want %q (containing %q)", d.Usage, tc.digestLine, tc.digestHas)
			}
			if Tokens([]byte(raw)) > MaxDigestTokens {
				t.Errorf("digest: %d tokens, over %d", Tokens([]byte(raw)), MaxDigestTokens)
			}
		})
	}
}
