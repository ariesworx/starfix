package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrintPrices(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  proto.PricesResult
		want string
	}{
		{"none", proto.PricesResult{}, "no prices; an admin sets one with `sfx admin prices set`\n"},
		{"some", proto.PricesResult{Prices: []proto.Price{
			{Model: "example-large", From: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
				Rates: proto.Rates{Input: 4_000_000, Output: 20_000_000, CacheWrite: 5_000_000, CacheWrite1h: 8_000_000, CacheRead: 400_000},
				SetBy: "dana", SetAt: at},
			{Model: "example-small\x1b[2J", From: time.Date(2026, 6, 1, 9, 30, 0, 0, time.UTC),
				Rates: proto.Rates{Input: 250_000, Output: 1_250_000}, SetBy: "erin", SetAt: at},
		}},
			"USD per million tokens\n" +
				"MODEL                 FROM              INPUT  OUTPUT  CACHE WRITE  CACHE WRITE 1H  CACHE READ  SET BY\n" +
				"example-large         2026-01-01        4      20      5            8               0.4         dana\n" +
				"example-small\\x1b[2J  2026-06-01 09:30  0.25   1.25    0            0               0           erin\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printPrices(&b, tc.res)
			if b.String() != tc.want {
				t.Errorf("printPrices =\n%s\nwant\n%s", b.String(), tc.want)
			}
		})
	}
}

func TestPrintCost(t *testing.T) {
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  proto.CostResult
		want string
	}{
		{"no titles", proto.CostResult{By: "person", Since: since, Until: since.Add(time.Hour), Groups: []proto.CostGroup{
			{Key: "alice", Tokens: proto.Tokens{Input: n(2_000)}, CostUSD: "1"}, {Key: "bob", Tokens: proto.Tokens{Input: n(5)}, CostUSD: "0.01"},
		}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(2_005)}, CostUSD: "1.01"}},
			"cost by person, 2026-10-01 00:00 UTC to 2026-10-01 01:00 UTC (1h), list price\n" +
				"  alice  $1.00  2k tokens\n" +
				"  bob    $0.01  5 tokens\n" +
				"total $1.01, 2k tokens\n"},
		{"nothing priced", proto.CostResult{By: "model", Since: since, Until: since.Add(time.Hour), Groups: []proto.CostGroup{
			{Key: "mystery", Tokens: proto.Tokens{Input: n(7)}, CostUSD: "0", Unpriced: true},
		}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(7)}, CostUSD: "0", Unpriced: true}, Unpriced: []string{"mystery"}},
			"cost by model, 2026-10-01 00:00 UTC to 2026-10-01 01:00 UTC (1h), list price\n" +
				"  mystery  unpriced  7 tokens\n" +
				"total unpriced, 7 tokens\n" +
				"unpriced: no price for mystery; an admin sets one with `sfx admin prices set`\n"},
		{"empty", proto.CostResult{By: "account", Since: since, Until: since.Add(7 * 24 * time.Hour), Total: proto.CostGroup{CostUSD: "0"}},
			"cost by account, 2026-10-01 00:00 UTC to 2026-10-08 00:00 UTC (7d): no tokens\n"},
		{"groups", proto.CostResult{By: "issue", Since: since, Until: since.Add(24 * time.Hour), Groups: []proto.CostGroup{
			{Key: "sf-a1", Title: "a long title " + strings.Repeat("x", 60), Tokens: proto.Tokens{Input: n(1_200_000), Output: n(34_000)},
				CostUSD: "12.345", Split: true},
			{Key: proto.CostUnattributed, Tokens: proto.Tokens{Input: n(500)}, CostUSD: "0.0001"},
			{Key: "sf-b2", Title: "odd", Tokens: proto.Tokens{Input: n(50)}, CostUSD: "0", Unpriced: true},
			{Key: "sf-c3", Tokens: proto.Tokens{Input: n(9)}, CostUSD: "1", Unpriced: true}, // partly priced
		}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(1_200_559), Output: n(34_000)}, CostUSD: "13.3451", Unpriced: true, Split: true},
			Unpriced: []string{"mystery"}, Truncated: true},
			"cost by issue, 2026-10-01 00:00 UTC to 2026-10-02 00:00 UTC (1d), list price\n" +
				"  sf-a1           a long title xxxxxxxxxxxxxxxxxxxxxxxxxxx…  $12.35    1.2M tokens  split\n" +
				"  (unattributed)                                             <$0.01    500 tokens\n" +
				"  sf-b2           odd                                        unpriced  50 tokens\n" +
				"  sf-c3                                                      $1.00     9 tokens     unpriced\n" +
				"total $13.35, 1.2M tokens\n" +
				"split: shared by time with other work, so an estimate\n" +
				"unpriced: no price for mystery; an admin sets one with `sfx admin prices set`\n" +
				"partial: more records than the server reads; narrow the window\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printCost(&b, tc.res)
			if b.String() != tc.want {
				t.Errorf("printCost =\n%s\nwant\n%s", b.String(), tc.want)
			}
		})
	}
}

func TestPrintUsageCost(t *testing.T) {
	opus := []proto.ModelTokens{{Model: "opus", Tokens: proto.Tokens{Input: n(950)}}}
	tests := []struct {
		name string
		u    proto.IssueUsage
		want string
	}{
		{"priced", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus, CostUSD: "1.005"},
			"account: acme\nheld 10m\ncost $1.01 list price\ntokens opus: 950 in\n"},
		{"partly unpriced", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus, CostUSD: "0.5", Unpriced: true},
			"account: acme\nheld 10m\ncost $0.50 list price; some tokens unpriced\ntokens opus: 950 in\n"},
		{"all unpriced", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus, CostUSD: "0", Unpriced: true},
			"account: acme\nheld 10m\ncost unpriced\ntokens opus: 950 in\n"},
		{"older server", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus},
			"account: acme\nheld 10m\ntokens opus: 950 in\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printIssueUsage(&b, "acme", &tc.u)
			if b.String() != tc.want {
				t.Errorf("printIssueUsage(%+v) =\n%s\nwant\n%s", tc.u, b.String(), tc.want)
			}
		})
	}
	var b bytes.Buffer
	printDigestUsage(&b, &proto.DigestUsage{HeldSeconds: 600, Models: opus, CostUSD: "250.5"})
	if want := "held 10m\ncost $250.50 list price\ntokens opus: 950 in\n"; b.String() != want {
		t.Errorf("printDigestUsage =\n%s\nwant\n%s", b.String(), want)
	}
}

// Cost and admin commands refuse bad usage before dialing.
func TestCostUsage(t *testing.T) {
	empty := t.TempDir()
	rates := []string{"--input", "4", "--output", "20", "--cache-write", "5", "--cache-write-1h", "8", "--cache-read", "0.4"}
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"cost needs since", []string{"-C", empty, "cost", "--by", "issue"}, "cost needs --since"},
		{"cost takes no arguments", []string{"-C", empty, "cost", "--since", "7d", "x"}, "cost takes no arguments"},
		{"cost bad by", []string{"-C", empty, "cost", "--since", "7d", "--by", "team"}, "--by must be account, issue, epic, person or model"},
		{"admin needs prices", []string{"-C", empty, "admin"}, "admin needs prices"},
		{"admin unknown", []string{"-C", empty, "admin", "keys"}, `unknown admin command "keys"`},
		{"prices unknown action", []string{"-C", empty, "admin", "prices", "rm", "m"}, `unknown prices action "rm"`},
		{"prices list takes no rates", []string{"-C", empty, "admin", "prices", "--input", "1"}, "only prices set takes rates"},
		{"set needs a model", append([]string{"-C", empty, "admin", "prices", "set", "--from", "2026-01-01"}, rates...), "prices set needs one model"},
		{"set needs from", append([]string{"-C", empty, "admin", "prices", "set", "m"}, rates...), "--from"},
		{"set needs every rate", []string{"-C", empty, "admin", "prices", "set", "m", "--from", "2026-01-01", "--input", "1"},
			"--output, --cache-write, --cache-write-1h, --cache-read"},
		{"bad rate", append([]string{"-C", empty, "admin", "prices", "set", "m", "--from", "2026-01-01", "--input", "$4"}, rates[2:]...),
			"--input: not a rate"},
		// With two bad rates, the first in the usage's order is named.
		{"two bad rates", []string{"-C", empty, "admin", "prices", "set", "m", "--from", "2026-01-01", "--input", "1", "--output", "x",
			"--cache-write", "1", "--cache-write-1h", "1", "--cache-read", "-1"}, "--output: not a rate"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errb := runCLI(t, tc.args...)
			if code != ExitUsage || !strings.Contains(errb, tc.stderr) || !strings.Contains(errb, "fix: usage: sfx ") {
				t.Fatalf("exit %d (want %d), stdout %q, stderr %q; want it to name %q and the usage", code, ExitUsage, out, errb, tc.stderr)
			}
		})
	}
}
