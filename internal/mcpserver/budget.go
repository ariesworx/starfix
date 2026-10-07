package mcpserver

import (
	"encoding/json"
	"unicode/utf8"
)

// Token budgets (design §1 principle 7, §9).
const (
	// MaxResultTokens caps every tool result.
	MaxResultTokens = 2000
	// MaxPrimeTokens caps prime.
	MaxPrimeTokens = 1500
	// MaxDigestTokens caps digest (design §5).
	MaxDigestTokens = 1500
	// DefaultLimit is how many issues a list returns when the agent does
	// not say.
	DefaultLimit = 10
)

// Tokens estimates how many tokens b costs a model. It counts one token per
// three bytes, which overestimates for English and JSON (about four bytes a
// token), so a result under budget here is under budget in practice.
func Tokens(b []byte) int { return (len(b) + 2) / 3 }

// size is the estimated token cost of v as JSON.
func size(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return Tokens(b)
}

// cut shortens s to at most n bytes on a rune boundary, adding "…".
func cut(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…", true
}

// fitTexts halves the longest of fields until fits reports true or every
// field is short. It reports whether anything was cut.
func fitTexts(fits func() bool, fields ...*string) bool {
	cutAny := false
	for !fits() {
		var longest *string
		for _, f := range fields {
			if len(*f) > 64 && (longest == nil || len(*f) > len(*longest)) {
				longest = f
			}
		}
		if longest == nil {
			return cutAny
		}
		*longest, _ = cut(*longest, len(*longest)/2)
		cutAny = true
	}
	return cutAny
}
