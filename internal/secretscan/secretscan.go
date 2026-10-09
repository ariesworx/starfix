// Package secretscan finds text that looks like a credential: a private
// key, a cloud or service token, a password or secret assigned a value,
// or a long random token. It is the one place starfix decides what looks
// like a secret, so every kind of text the server keeps can be checked
// the same way (design §6: a secrets lint refuses anything that looks
// like a key or password).
//
// It errs toward letting text through: a memory refused for a secret it
// does not hold costs an agent a rewrite, so the patterns are specific
// and the entropy threshold conservative. Text that names where a secret
// lives (a vault path, a variable) passes.
package secretscan

import (
	"math"
	"regexp"
	"strings"
	"unicode"
)

// Kind names what a [Finding] looks like. It never holds the text found.
type Kind string

// Kinds of finding, in the order Find checks them.
const (
	KindPrivateKey      Kind = "private key"
	KindAWSAccessKey    Kind = "AWS access key id"
	KindGitHubToken     Kind = "GitHub token"
	KindGitLabToken     Kind = "GitLab token"
	KindSlackToken      Kind = "Slack token"
	KindStripeKey       Kind = "Stripe key"
	KindJWT             Kind = "JSON web token"
	KindBearerToken     Kind = "bearer token"
	KindURLPassword     Kind = "password in a URL"
	KindCommandPassword Kind = "password on a command line"
	KindAssignment      Kind = "password or secret assignment"
	KindHighEntropy     Kind = "long random token"
)

// Finding is the first credential-like text Find saw: what it looks like
// and the byte offset where it starts.
type Finding struct {
	Kind   Kind
	Offset int
}

// String names the finding's kind; it never echoes the text found.
func (f Finding) String() string { return string(f.Kind) }

// tokenPatterns are the formats with a recognizable shape, most specific
// first.
var tokenPatterns = []struct {
	kind Kind
	re   *regexp.Regexp
}{
	{KindPrivateKey, regexp.MustCompile(`-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY(?: BLOCK)?-----`)},
	{KindAWSAccessKey, regexp.MustCompile(`\b(?:A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}\b`)},
	{KindGitHubToken, regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,}|github_pat_[A-Za-z0-9_]{22,})`)},
	{KindGitLabToken, regexp.MustCompile(`\bgl(?:pat|dt|rt|ptt|soat|cbt|ft)-[A-Za-z0-9_-]{20,}`)},
	{KindSlackToken, regexp.MustCompile(`\bxox[abeoprs]-[A-Za-z0-9-]{10,}|hooks\.slack\.com/(?:services|workflows)/[A-Za-z0-9/_+-]{20,}`)},
	{KindStripeKey, regexp.MustCompile(`\b(?:(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|whsec_[A-Za-z0-9]{24,})`)},
	{KindJWT, regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{10,}`)},
}

// assignment is a name that holds a credential, then = or :, then a
// value. The name may have a prefix (DB_PASSWORD, client_secret) and a
// closing quote (JSON); the short names pwd and pass take a prefix only
// after a separator (SMTP_PASS), so a word such as compass is no name.
// Group 1 is the name, 2 the separator, 3 the value, quoted or not.
var assignment = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])([a-z0-9_.-]*(?:password|passwd|passphrase|secret|api[_-]?key|access[_-]?key|auth[_-]?token|access[_-]?token|token|private[_-]?key|credentials?)|(?:[a-z0-9_.-]*[_.-])?(?:pwd|pass))["']?[ \t]*(=|:)[ \t]*("[^"\n]*"|'[^'\n]*'|[^\s,;"'&)}\]]+)`)

// bearer is an HTTP bearer token: group 1 is the token.
var bearer = regexp.MustCompile(`(?i)\bbearer[ \t]+([A-Za-z0-9._~+/-]{20,}=*)`)

// urlPassword is a URL's userinfo with a password: group 1 is the
// password.
var urlPassword = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9+.-]*://[^\s/:@]+:([^\s/@]+)@`)

// commandPassword is a MySQL client given its password with -p, which
// takes the value with no space: group 1 is the password.
var commandPassword = regexp.MustCompile(`\bmysql(?:dump|admin|import|show|check|binlog)?\b[^\n]*?[ \t]-p(\S+)`)

// placeholders are values that stand for a secret rather than being one.
var placeholders = map[string]bool{
	"redacted": true, "changeme": true, "change_me": true, "null": true, "none": true, "nil": true,
	"true": true, "false": true, "required": true, "optional": true, "example": true, "placeholder": true,
	"hidden": true, "secret": true, "password": true, "token": true, "unset": true, "empty": true,
}

// pointerPrefixes start values that name where a secret lives.
var pointerPrefixes = []string{"op://", "vault:", "env:", "sops:", "ref:", "secret://", "arn:aws:secretsmanager:", "projects/"}

// Assigned values shorter than minAssigned are not taken for secrets, nor
// are values of letters alone shorter than minLetters.
const (
	minAssigned = 6
	minLetters  = 12
)

// High-entropy tokens: at least minRandom characters of the base64
// alphabets, mixing upper and lower case and digits, at least
// minEntropy bits a character. Measured on seeded samples, 97% of random
// base62 tokens of 32 characters pass, and 99.9% of 40; identifiers and
// paths measured 4.0 to 4.3, and hex (hashes, commit ids) never mixes
// cases.
const (
	minRandom  = 32
	minEntropy = 4.3
)

// randomToken is a run of base64 (standard and URL-safe) characters.
var randomToken = regexp.MustCompile(`[A-Za-z0-9+/_=-]{32,}`)

// sshKeyType ends text that an SSH public key's body follows: a public
// key is no secret, though its body looks random.
var sshKeyType = regexp.MustCompile(`(?:ssh-(?:rsa|dss|ed25519)|ecdsa-sha2-nistp\d+|sk-(?:ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com) $`)

// Find returns the first credential-like text in s and true, or false
// when s holds none. Formats with a known shape are checked first, then
// bearer tokens, passwords in URLs and on a MySQL command line, then
// assignments, then long random tokens; within a check the earliest
// match wins.
func Find(s string) (Finding, bool) {
	for _, p := range tokenPatterns {
		if loc := p.re.FindStringIndex(s); loc != nil {
			return Finding{Kind: p.kind, Offset: loc[0]}, true
		}
	}
	for _, c := range []struct {
		kind Kind
		re   *regexp.Regexp
		ok   func(string) bool
	}{
		{KindBearerToken, bearer, func(v string) bool { return strings.ContainsAny(v, "0123456789") }},
		{KindURLPassword, urlPassword, plainValue},
		{KindCommandPassword, commandPassword, plainValue},
	} {
		if off, ok := findValue(s, c.re, c.ok); ok {
			return Finding{Kind: c.kind, Offset: off}, true
		}
	}
	if off, ok := findAssignment(s); ok {
		return Finding{Kind: KindAssignment, Offset: off}, true
	}
	if off, ok := findRandom(s); ok {
		return Finding{Kind: KindHighEntropy, Offset: off}, true
	}
	return Finding{}, false
}

// findValue returns the offset of the first match of re whose group 1
// passes ok.
func findValue(s string, re *regexp.Regexp, ok func(string) bool) (int, bool) {
	for _, m := range re.FindAllStringSubmatchIndex(s, -1) {
		if ok(s[m[2]:m[3]]) {
			return m[0], true
		}
	}
	return 0, false
}

// plainValue reports whether an unquoted value may be a credential.
func plainValue(v string) bool { return secretValue(v, false) }

// findAssignment returns the offset of the first name assigned a value
// that may be a credential, one that is no placeholder and holds a digit
// or a symbol, as an English word does not. With =, a value of letters
// alone counts too once it is minLetters long, and a quoted value may
// hold spaces, as a passphrase does. With :, which prose uses too, the
// name must be quoted or start a line, as in JSON or YAML, and the value
// must be one word.
func findAssignment(s string) (int, bool) {
	for _, m := range assignment.FindAllStringSubmatchIndex(s, -1) {
		name, sep, raw := m[2], s[m[4]:m[5]], s[m[6]:m[7]]
		value := unquote(raw)
		if !secretValue(value, sep == "=" && value != raw) {
			continue
		}
		symbol := hasDigitOrSymbol(value)
		switch {
		case sep == "=" && (symbol || len(value) >= minLetters):
		case sep == ":" && symbol && keyed(s, m[2], m[3]):
		default:
			continue
		}
		return name, true
	}
	return 0, false
}

// keyed reports whether the name s[start:end] is a key, not a word in a
// sentence: quoted, or first on its line after indentation or a YAML
// list dash.
func keyed(s string, start, end int) bool {
	if start > 0 && (s[start-1] == '"' || s[start-1] == '\'') && end < len(s) && s[end] == s[start-1] {
		return true
	}
	line := s[strings.LastIndexByte(s[:start], '\n')+1 : start]
	return strings.Trim(line, " \t-") == ""
}

// unquote drops one pair of matching quotes around v.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// secretValue reports whether an assigned value may be a credential: long
// enough, one word unless quoted is set, and not a placeholder, a
// variable, a template, a call, a file path or a pointer to where the
// secret lives.
func secretValue(v string, quoted bool) bool {
	if len(v) < minAssigned || (!quoted && strings.ContainsAny(v, " \t(")) {
		return false
	}
	if placeholders[strings.ToLower(v)] || strings.ContainsAny(v[:1], "$<{%*[/") ||
		strings.HasPrefix(v, "~/") || strings.HasPrefix(v, "./") {
		return false
	}
	if strings.Trim(v, "xX*.•-_") == "" {
		return false
	}
	lower := strings.ToLower(v)
	for _, p := range pointerPrefixes {
		if strings.HasPrefix(lower, p) {
			return false
		}
	}
	return true
}

// hasDigitOrSymbol reports whether v holds a digit or a character other
// than a letter, which an English word does not.
func hasDigitOrSymbol(v string) bool {
	for _, r := range v {
		if !unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// findRandom returns the offset of the first long token that mixes
// cases and digits with high entropy, other than an SSH public key's
// body.
func findRandom(s string) (int, bool) {
	for _, loc := range randomToken.FindAllStringIndex(s, -1) {
		tok := strings.TrimRight(s[loc[0]:loc[1]], "=")
		if len(tok) < minRandom || !mixed(tok) || entropy(tok) < minEntropy {
			continue
		}
		if sshKeyType.MatchString(s[max(0, loc[0]-64):loc[0]]) || publicKey(tok) {
			continue
		}
		return loc[0], true
	}
	return 0, false
}

// publicKey reports whether tok is a key made to be published, such as a
// Stripe publishable key, which looks random but is no secret.
func publicKey(tok string) bool {
	return strings.HasPrefix(tok, "pk_live_") || strings.HasPrefix(tok, "pk_test_")
}

// mixed reports whether tok holds an uppercase letter, a lowercase letter
// and a digit.
func mixed(tok string) bool {
	var upper, lower, digit bool
	for i := range len(tok) {
		switch c := tok[i]; {
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		case c >= '0' && c <= '9':
			digit = true
		}
	}
	return upper && lower && digit
}

// entropy is the Shannon entropy of s's bytes, in bits a byte.
func entropy(s string) float64 {
	if s == "" {
		return 0
	}
	var counts [256]int
	for i := range len(s) {
		counts[s[i]]++
	}
	n := float64(len(s))
	h := 0.0
	for _, c := range counts {
		if c > 0 {
			p := float64(c) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}
