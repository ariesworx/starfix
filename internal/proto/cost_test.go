package proto

import (
	"math/big"
	"testing"
)

func TestUSD(t *testing.T) {
	tests := []struct {
		pico string
		want string
	}{
		{"0", "0"},
		{"1", "0.000000000001"},
		{"1000000000000", "1"},
		{"12500000000000", "12.5"},
		{"3000000", "0.000003"},
		{"123456789012345678901234567890", "123456789012345678.90123456789"},
		{"-2500000000000", "-2.5"},
	}
	for _, tc := range tests {
		n, _ := new(big.Int).SetString(tc.pico, 10)
		if got := USD(n); got != tc.want {
			t.Errorf("USD(%s) = %q, want %q", tc.pico, got, tc.want)
		}
	}
	if got := USD(nil); got != "0" {
		t.Errorf("USD(nil) = %q, want %q", got, "0")
	}
}

func TestParseUSD(t *testing.T) {
	tests := []struct {
		usd  string
		pico string // "" for refused
	}{
		{"0", "0"},
		{"0.000000000001", "1"},
		{"12.5", "12500000000000"},
		{"123456789012345678.90123456789", "123456789012345678901234567890"},
		{"0.0000000000001", ""}, // finer than a picodollar
		{"-2.5", ""},            // a cost is never negative
		{"1/3", ""},
		{"1e3", ""},
		{"", ""},
	}
	for _, tc := range tests {
		got, err := ParseUSD(tc.usd)
		switch {
		case tc.pico == "" && err == nil:
			t.Errorf("ParseUSD(%q) = %s, want an error", tc.usd, got)
		case tc.pico != "" && (err != nil || got.String() != tc.pico):
			t.Errorf("ParseUSD(%q) = %v, %v; want %s picodollars", tc.usd, got, err, tc.pico)
		case tc.pico != "" && USD(got) != tc.usd:
			t.Errorf("USD(ParseUSD(%q)) = %q, want it back", tc.usd, USD(got))
		}
	}
}

func TestDollars(t *testing.T) {
	tests := []struct{ usd, want string }{
		{"0", "$0.00"},
		{"12.5", "$12.50"},
		{"12.345", "$12.35"}, // half rounds up
		{"12.344999", "$12.34"},
		{"0.005", "$0.01"},
		{"0.004999", "<$0.01"},
		{"0.000000000001", "<$0.01"},
		{"1234567.891", "$1,234,567.89"},
		{"999.999", "$1,000.00"},
		{"not money", "not money"},
		{"1/3", "1/3"}, // a fraction is not a decimal
		{"1e3", "1e3"},
		// A cost is never negative, so a negative is a fault, shown
		// exactly rather than rounded to look like a small cost.
		{"-0.004", "-0.004"},
		{"-12.5", "-12.5"},
	}
	for _, tc := range tests {
		if got := Dollars(tc.usd); got != tc.want {
			t.Errorf("Dollars(%q) = %q, want %q", tc.usd, got, tc.want)
		}
	}
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"3", 3_000_000, true},
		{"3.75", 3_750_000, true},
		{"0.000001", 1, true},
		{"0", 0, true},
		{"1000000", 1_000_000_000_000, true},
		{"1000000.000001", 0, false}, // past MaxRate
		{"0.0000001", 0, false},      // finer than a micro-dollar
		{"-1", 0, false},
		{"1e3", 0, false},
		{"1/3", 0, false},
		{"", 0, false},
		{".5", 0, false},
		{"5.", 0, false},
		{"$3", 0, false},
	}
	for _, tc := range tests {
		got, err := ParseRate(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseRate(%q) = %d, %v; want %d, ok %v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

func TestFormatRate(t *testing.T) {
	tests := []struct {
		micros int64
		want   string
	}{
		{0, "0"}, {1, "0.000001"}, {3_000_000, "3"}, {3_750_000, "3.75"}, {1_000_000_000_000, "1000000"},
	}
	for _, tc := range tests {
		if got := FormatRate(tc.micros); got != tc.want {
			t.Errorf("FormatRate(%d) = %q, want %q", tc.micros, got, tc.want)
		}
		if back, err := ParseRate(FormatRate(tc.micros)); err != nil || back != tc.micros {
			t.Errorf("ParseRate(FormatRate(%d)) = %d, %v; want it back", tc.micros, back, err)
		}
	}
}

func FuzzParseRate(f *testing.F) {
	for _, s := range []string{"3", "3.75", "0.000001", "1000000", "-1", "1e3", "1/3", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseRate(s)
		if err != nil {
			return
		}
		if n < 0 || n > MaxRate {
			t.Fatalf("ParseRate(%q) = %d, outside [0, MaxRate]", s, n)
		}
		if back, err := ParseRate(FormatRate(n)); err != nil || back != n {
			t.Fatalf("ParseRate(FormatRate(ParseRate(%q))) = %d, %v; want %d", s, back, err, n)
		}
	})
}

func FuzzParseUSD(f *testing.F) {
	for _, s := range []string{"0", "12.5", "0.000000000001", "0.0000000000001", "-1", "1e3", "1/3", "007.50", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		n, err := ParseUSD(s)
		if err != nil {
			return
		}
		if n.Sign() < 0 {
			t.Fatalf("ParseUSD(%q) = %s, negative", s, n)
		}
		if back, err := ParseUSD(USD(n)); err != nil || back.Cmp(n) != 0 {
			t.Fatalf("ParseUSD(USD(ParseUSD(%q))) = %v, %v; want %s", s, back, err, n)
		}
	})
}
