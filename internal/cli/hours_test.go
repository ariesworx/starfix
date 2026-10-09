package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

func TestPrintHours(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  proto.HoursResult
		want string
	}{
		{"none", proto.HoursResult{}, "no hours logged\n"},
		{"some", proto.HoursResult{Entries: []proto.HoursEntry{
			{ID: "aaaaaaaaaaaaaaaa", Issue: "sf-a1", Principal: "alice", On: "2026-10-09", Seconds: 5400, Note: "pairing\x1b[2J", At: at},
			{ID: "bbbbbbbbbbbbbbbb", Issue: "sf-b2", Principal: "bob", On: "2026-10-08", Seconds: 28800, At: at},
		}, More: 3},
			"ON          WHO    ISSUE  HOURS  ENTRY             NOTE\n" +
				"2026-10-09  alice  sf-a1  1.5h   aaaaaaaaaaaaaaaa  pairing\\x1b[2J\n" +
				"2026-10-08  bob    sf-b2  8h     bbbbbbbbbbbbbbbb\n" +
				"and 3 more; raise -n, or narrow with --issue or --by\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printHours(&b, tc.res)
			if b.String() != tc.want {
				t.Errorf("printHours =\n%s\nwant\n%s", b.String(), tc.want)
			}
		})
	}
}

func TestPrintPlans(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		res  proto.PlansResult
		want string
	}{
		{"none", proto.PlansResult{}, "no plans; an admin sets one with `sfx admin plans set`\n"},
		{"some", proto.PlansResult{Plans: []proto.Plan{
			{Name: "team", From: "2026-09", Fee: 25_000_000, Seats: 3, Principals: []string{"alice", "bob"}, SetBy: "dana", SetAt: at},
			{Name: "team", From: "2027-01", Principals: []string{}, SetBy: "dana", SetAt: at},
		}},
			"USD a seat a month\n" +
				"NAME  FROM     FEE  SEATS  MONTHLY  PRINCIPALS  SET BY\n" +
				"team  2026-09  25   3      75       alice, bob  dana\n" +
				"team  2027-01  0    0      0        -           dana\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			printPlans(&b, tc.res)
			if b.String() != tc.want {
				t.Errorf("printPlans =\n%s\nwant\n%s", b.String(), tc.want)
			}
		})
	}
}

// A report with plans shows each group's amortized cost beside its list
// price, and one with hours each group's logged hours.
func TestPrintCostAmortizedAndLogged(t *testing.T) {
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	res := proto.CostResult{By: "issue", Since: since, Until: since.Add(24 * time.Hour), Groups: []proto.CostGroup{
		{Key: "sf-a1", Title: "one", Tokens: proto.Tokens{Input: n(1_200_000)}, CostUSD: "12.345", AmortizedUSD: "20", LoggedSeconds: 5400},
		{Key: proto.CostUnattributed, Tokens: proto.Tokens{Input: n(500)}, CostUSD: "0.5", AmortizedUSD: "10"},
		{Key: "sf-b2", Title: "two", CostUSD: "0", AmortizedUSD: "0", LoggedSeconds: 900},
	}, Total: proto.CostGroup{Tokens: proto.Tokens{Input: n(1_200_500)}, CostUSD: "12.845", AmortizedUSD: "30", LoggedSeconds: 6300}}
	want := "cost by issue, 2026-10-01 00:00 UTC to 2026-10-02 00:00 UTC (1d), list price and amortized\n" +
		"  sf-a1           one  $12.35  $20.00 amortized  1.2M tokens  1.5h logged\n" +
		"  (unattributed)       $0.50   $10.00 amortized  500 tokens\n" +
		"  sf-b2           two  $0.00   $0.00 amortized   0 tokens     0.25h logged\n" +
		"total $12.85, $30.00 amortized, 1.2M tokens, 1.75h logged\n"
	var b bytes.Buffer
	printCost(&b, res)
	if b.String() != want {
		t.Errorf("printCost =\n%s\nwant\n%s", b.String(), want)
	}
}

func TestPrintUsageHours(t *testing.T) {
	opus := []proto.ModelTokens{{Model: "opus", Tokens: proto.Tokens{Input: n(950)}}}
	tests := []struct {
		name string
		u    proto.IssueUsage
		want string
	}{
		{"with tokens", proto.IssueUsage{Account: "acme", HeldSeconds: 600, Models: opus, LoggedSeconds: 12600,
			Logged: []proto.PersonHours{{Principal: "bob", Seconds: 7200}, {Principal: "alice", Seconds: 5400}}},
			"account: acme\nheld 10m\nlogged 3.5h: bob 2h, alice 1.5h\ntokens opus: 950 in\n"},
		{"hours alone", proto.IssueUsage{Account: "acme", LoggedSeconds: 3600, Logged: []proto.PersonHours{{Principal: "bob", Seconds: 3600}}},
			"account: acme\nlogged 1h: bob 1h\n"},
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
	printDigestUsage(&b, &proto.DigestUsage{LoggedSeconds: 9000})
	if want := "logged 2.5h\n"; b.String() != want {
		t.Errorf("printDigestUsage(hours alone) =\n%s\nwant\n%s", b.String(), want)
	}
}

// log and admin plans refuse bad usage before dialing.
func TestLogUsage(t *testing.T) {
	empty := t.TempDir()
	plan := []string{"--fee", "25", "--seats", "3", "--from", "2026-09"}
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"one argument", []string{"-C", empty, "log", "1h"}, "log needs a time and an issue id"},
		{"three arguments", []string{"-C", empty, "log", "1h", "sf-a1", "sf-b2"}, "log needs a time and an issue id"},
		{"bad time", []string{"-C", empty, "log", "1", "sf-a1"}, `"1": not a time from 1m to 24h`},
		{"too long", []string{"-C", empty, "log", "25h", "sf-a1"}, `"25h": not a time`},
		{"bad day", []string{"-C", empty, "log", "1h", "sf-a1", "--on", "yesterday"}, `--on "yesterday" must be a date`},
		{"undo with arguments", []string{"-C", empty, "log", "--undo", "aaaa", "1h", "sf-a1"}, "--undo takes no other arguments"},
		{"undo with on", []string{"-C", empty, "log", "--undo", "aaaa", "--on", "2026-10-01"}, "--undo takes no other arguments"},
		{"list with note", []string{"-C", empty, "log", "--note", "x"}, "--on and --note are for logging"},
		{"log with issue", []string{"-C", empty, "log", "1h", "sf-a1", "--issue", "sf-a1"}, "--issue, --by and -n are for listing"},
		{"plans unknown action", []string{"-C", empty, "admin", "plans", "rm", "team"}, `unknown plans action "rm"`},
		{"plans list takes no terms", []string{"-C", empty, "admin", "plans", "--fee", "1"}, "only plans set takes terms"},
		{"set needs a name", append([]string{"-C", empty, "admin", "plans", "set"}, plan...), "plans set needs one name"},
		{"set needs terms", []string{"-C", empty, "admin", "plans", "set", "team", "--fee", "1"}, "plans set needs --from, --seats"},
		{"bad fee", []string{"-C", empty, "admin", "plans", "set", "team", "--fee", "$25", "--seats", "3", "--from", "2026-09"}, "--fee: not an amount"},
		{"bad seats", []string{"-C", empty, "admin", "plans", "set", "team", "--fee", "25", "--seats", "x", "--from", "2026-09"}, "seats"},
		{"bad month", []string{"-C", empty, "admin", "plans", "set", "team", "--fee", "25", "--seats", "3", "--from", "2026-09-01"},
			`--from "2026-09-01" must be a month`},
		{"admin needs a command", []string{"-C", empty, "admin"}, "admin needs prices or plans"},
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
