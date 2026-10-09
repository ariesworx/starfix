package proto

import (
	"strings"
	"testing"
	"time"
)

func TestParseHours(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration // 0 for refused
	}{
		{"1.5h", 90 * time.Minute},
		{"90m", 90 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"1m", time.Minute},
		{"24h", 24 * time.Hour},
		{"1.33h", 4788 * time.Second},
		{"45m30s", 45*time.Minute + 30*time.Second},
		{"", 0},
		{"1", 0},        // no unit
		{"59s", 0},      // under a minute
		{"24h1s", 0},    // over a day
		{"-1h", 0},      // negative
		{"+1h", 0},      // signed
		{"1.0001m", 0},  // not whole seconds
		{"2d", 0},       // days are not hours
		{"1h 30m", 0},   // space
		{"9999999h", 0}, // overflow is over a day
		{"0.5h0.5h", 0}, // Go accepts it, and it is an hour; refused as odd
		{"1e3m", 0},     // exponents
		{"1h30m\n", 0},  // trailing newline
		{"１h", 0},       // a full-width digit
		{"999999999999999999999h", 0},
	}
	for _, tc := range tests {
		got, err := ParseHours(tc.in)
		switch {
		case tc.want == 0 && err == nil:
			t.Errorf("ParseHours(%q) = %s, want an error", tc.in, got)
		case tc.want != 0 && (err != nil || got != tc.want):
			t.Errorf("ParseHours(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
}

func TestHours(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{0, "0h"},
		{3600, "1h"},
		{5400, "1.5h"},
		{4788, "1.33h"},
		{60, "0.02h"},
		{18, "0.01h"}, // 0.005h rounds half up
		{17, "<0.01h"},
		{86400 * 400, "9600h"},
	}
	for _, tc := range tests {
		if got := Hours(tc.seconds); got != tc.want {
			t.Errorf("Hours(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}

// Whatever ParseHours accepts is a whole number of seconds from a minute
// to a day, and reads back from Go's own rendering of it.
func FuzzParseHours(f *testing.F) {
	for _, s := range []string{"1.5h", "90m", "1h30m", "24h", "59s", "-1h", "1.0001m", "0.5h0.5h", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseHours(s)
		if err != nil {
			return
		}
		if d < time.Minute || d > 24*time.Hour || d%time.Second != 0 {
			t.Fatalf("ParseHours(%q) = %s, want whole seconds from 1m to 24h", s, d)
		}
		if again, err := ParseHours(d.String()); err != nil || again != d {
			t.Fatalf("ParseHours(%q) = %s, and ParseHours(%q) = %s, %v", s, d, d.String(), again, err)
		}
	})
}

// A fee reads as a rate does, in US dollars to the micro-dollar, and
// says it is an amount rather than a rate when refused.
func TestParseFee(t *testing.T) {
	tests := []struct {
		in   string
		want int64 // -1 for refused
	}{
		{"25", 25_000_000},
		{"19.99", 19_990_000},
		{"0", 0},
		{"0.000001", 1},
		{"1000000", MaxFee},
		{"1000000.000001", -1},
		{"$25", -1},
		{"-1", -1},
		{"", -1},
	}
	for _, tc := range tests {
		got, err := ParseFee(tc.in)
		switch {
		case tc.want < 0 && (err == nil || !strings.Contains(err.Error(), "not an amount of US dollars")):
			t.Errorf("ParseFee(%q) = %d, %v; want an error saying it is not an amount", tc.in, got, err)
		case tc.want >= 0 && (err != nil || got != tc.want):
			t.Errorf("ParseFee(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
		}
	}
}
