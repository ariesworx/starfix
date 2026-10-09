package secretscan

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// Every credential below is fake and built at run time, by concatenation
// or from a seeded generator, so that no secret scanner reading this file
// finds a real-looking one in it.

// fake returns n characters drawn from alphabet by a generator seeded
// with seed: high entropy, deterministic, and never in the source.
func fake(seed uint64, n int, alphabet string) string {
	r := rand.New(rand.NewPCG(seed, seed^0x5eed)) //nolint:gosec // test data, not security
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}

const (
	base62 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	base64 = base62 + "+/"
	upperD = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

func TestFindSecrets(t *testing.T) {
	tests := []struct {
		name string
		text string
		kind Kind
	}{
		{"rsa private key", "key:\n-----BEGIN " + "RSA PRIVATE KEY-----\nMIIfake\n-----END RSA PRIVATE KEY-----", KindPrivateKey},
		{"openssh private key", "-----BEGIN " + "OPENSSH PRIVATE KEY-----", KindPrivateKey},
		{"bare private key", "-----BEGIN " + "PRIVATE KEY-----", KindPrivateKey},
		{"encrypted private key", "-----BEGIN " + "ENCRYPTED PRIVATE KEY-----", KindPrivateKey},
		{"pgp private key block", "-----BEGIN " + "PGP PRIVATE KEY BLOCK-----", KindPrivateKey},
		{"aws access key id", "use " + "AKIA" + "FAKEFAKEFAKEFAKE" + " for deploys", KindAWSAccessKey},
		{"aws temporary key id", "ASIA" + fake(1, 16, upperD), KindAWSAccessKey},
		{"github classic token", "token gh" + "p_" + fake(2, 36, base62), KindGitHubToken},
		{"github oauth token", "gh" + "o_" + fake(3, 36, base62), KindGitHubToken},
		{"github fine-grained token", "github" + "_pat_" + fake(4, 22, base62) + "_" + fake(5, 59, base62), KindGitHubToken},
		{"gitlab personal token", "gl" + "pat-" + fake(6, 20, base62), KindGitLabToken},
		{"gitlab deploy token", "gl" + "dt-" + fake(7, 24, base62), KindGitLabToken},
		{"slack bot token", "xo" + "xb-" + "1234567890-" + fake(8, 24, base62), KindSlackToken},
		{"slack webhook", "https://hooks." + "slack.com/services/T0FAKE/B0FAKE/" + fake(9, 24, base62), KindSlackToken},
		{"stripe live key", "sk" + "_live_" + fake(10, 24, base62), KindStripeKey},
		{"stripe test key", "sk" + "_test_" + fake(11, 24, base62), KindStripeKey},
		{"stripe restricted key", "rk" + "_live_" + fake(12, 24, base62), KindStripeKey},
		{"stripe webhook secret", "wh" + "sec_" + fake(13, 32, base62), KindStripeKey},
		{"jwt", "Authorization: Bearer " + "eyJ" + fake(14, 20, base62) + ".eyJ" + fake(15, 40, base62) + "." + fake(16, 43, base62), KindJWT},
		{"password assignment", "db: password=" + "hunter22", KindAssignment},
		{"env password", "export DB_PASSWORD=" + "s3cr3t-value", KindAssignment},
		{"quoted secret", `client_secret = "` + "abc123def456" + `"`, KindAssignment},
		{"api key assignment", "API_KEY=" + fake(17, 12, base62), KindAssignment},
		{"json password", `{"user": "alice", "password": "` + "Tr0ub4dor&3" + `"}`, KindAssignment},
		{"yaml token at line start", "url: https://example.com\ntoken: " + "abc123xyz789", KindAssignment},
		{"yaml indented secret", "db:\n  secret: " + "x9y8z7w6", KindAssignment},
		{"url query token", "https://example.com/hook?token=" + "a1b2c3d4e5f6", KindAssignment},
		{"url password", "postgres://app:" + "S3cretPass99" + "@db.example.com/app", KindURLPassword},
		{"url password in prose", "connect with https://alice:" + "Tr0ub4dor-3" + "@git.example.com/repo.git today", KindURLPassword},
		{"quoted passphrase", `DB_PASSWORD="` + "correct horse " + "battery staple" + `"`, KindAssignment},
		{"single-quoted passphrase", "secret = '" + "four random " + "words here" + "'", KindAssignment},
		{"bearer header", "curl -H 'Authorization: Bearer " + fake(24, 28, base62) + "'", KindBearerToken},
		{"bearer opaque token", "bearer " + "abc123" + ".def456" + "-ghi789_jkl", KindBearerToken},
		{"pwd assignment", "pwd=" + "Hunter2" + "Secret!", KindAssignment},
		{"pass assignment", "SMTP_PASS=" + "Hunter2" + "Secret!", KindAssignment},
		{"pass flag", "--pass=" + "Hunter2" + "Secret!", KindAssignment},
		{"mysql password flag", "mysql -u root -p" + "Hunter2" + "Secret! app", KindCommandPassword},
		{"mysqldump password flag", "mysqldump -p" + "Hunter2" + "Secret! app > dump.sql", KindCommandPassword},
		{"long base62 token", "the value is " + fake(18, 40, base62), KindHighEntropy},
		{"long base64 secret", "aws_secret " + fake(19, 40, base64), KindHighEntropy},
		{"token in a sentence", "set X to " + fake(20, 48, base62) + " and restart", KindHighEntropy},
		{"token after a word ending in h1", "path1:" + fake(30, 40, base62), KindHighEntropy},
		{"token named like a hash", "mysha256-" + fake(31, 40, base62), KindHighEntropy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, ok := Find(tc.text)
			if !ok || f.Kind != tc.kind {
				t.Fatalf("Find(%s) = %+v, %v; want kind %q", tc.name, f, ok, tc.kind)
			}
			if f.Offset < 0 || f.Offset >= len(tc.text) {
				t.Errorf("Find(%s).Offset = %d, outside the text's %d bytes", tc.name, f.Offset, len(tc.text))
			}
		})
	}
}

// Ordinary memories, including text that mentions keys and passwords
// without holding one, pass.
func TestFindNothing(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"prose", "Deploys go out on weekday mornings; ask in #ops before a Friday release."},
		{"mentions a password", "The staging password is in the team vault under db/staging."},
		{"password colon prose", "Password: stored in the vault, never in the repo."},
		{"token colon prose", "Remember the token: it expires after 15m, so renew it."},
		{"secret in a sentence", "Keep the secret = something nobody writes down."},
		{"placeholder angle", "password=<your password>"},
		{"placeholder variable", "DB_PASSWORD=$DB_PASSWORD"},
		{"placeholder braces", "token={{ .Token }}"},
		{"placeholder template", "API_KEY=${API_KEY}"},
		{"placeholder stars", "password=********"},
		{"placeholder x", "secret=xxxxxxxx"},
		{"placeholder word", "password=changeme"},
		{"redacted", "token=REDACTED"},
		{"vault pointer", "password=vault:secret/data/db#password"},
		{"1password pointer", "api_key=op://Engineering/stripe/credential"},
		{"code reading env", `password = os.Getenv("DB_PASSWORD")`},
		{"short value", "pwd=abc"},
		{"empty value", `password=""`},
		{"uuid", "project 3f2b8c1e-9d4a-4e7b-8c3d-2a1b0f9e8d7c"},
		{"sha256 hex", "sha256 9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"},
		{"git sha", "fixed in 0fd2149a7c3e5b1d9f8e2c4a6b0d1e3f5a7c9e1b"},
		{"upper hex", "digest 9F86D081884C7D659A2FEAA0C55AD015A3BF4F1B2B0B822CD15D6C15B0F00A08"},
		{"snake identifier", "rename this_is_a_very_long_identifier_name_2024_Version to something short"},
		{"camel identifier", "TestDispatchUsageLimitsRefusesOversizedBatches2 failed once"},
		{"go path", "see internal/store/migrations/0012_issue_paths.sql and internal/mcpserver/tools.go"},
		{"mixed path", "src/Components/Header2/IndexPage.tsx renders the menu"},
		{"url", "https://github.com/example/starfix/pull/47/files#diff-1234"},
		{"module path", "import github.com/ExampleOrg/Starfix/internal/mcpserver/v2 here"},
		{"ssh public key", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI" + fake(22, 43, base64) + " alice@example.com"},
		{"ssh rsa public key", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQ" + fake(23, 300, base64)},
		{"aws key id prefix only", "AKIA is the prefix of an access key id"},
		{"issue ids", "blocked by sf-a1b2c3d4 and sf-q4m7x2k9; see bd-a3f.2"},
		{"stripe publishable key", "pk" + "_live_" + fake(21, 24, base62)},
		{"url without password", "ssh://git@git.example.com:22/repo.git and https://alice@example.com/"},
		{"url with a port", "https://db.example.com:5432/app"},
		{"url password variable", "postgres://app:$DB_PASSWORD@db.example.com/app"},
		{"url password template", "postgres://app:{{ .Password }}@db.example.com/app"},
		{"url password placeholder", "postgres://app:<password>@db.example.com/app"},
		{"url password word", "postgres://user:password@localhost/app"},
		{"quoted pointer with spaces", `password: "in the team vault"`},
		{"bearer variable", "Authorization: Bearer $TOKEN"},
		{"bearer placeholder", "Authorization: Bearer <token>"},
		{"bearer prose", "Send a bearer token in the Authorization header."},
		{"pwd path", "PWD=/home/alice/src/starfix"},
		{"password file path", "password=/run/secrets/db_password"},
		{"bypass flag", "bypass=true"},
		{"compass word", "compass=N0rth1234"},
		{"mysql prompt", "mysql -u root -p app, then type the password"},
		{"mysql password variable", "mysql -p$MYSQL_PWD app"},
		{"sri sha384", `<script integrity="sha384-` + fake(25, 64, base64) + `">`},
		{"sri sha512", "integrity sha512-" + fake(26, 86, base64) + "=="},
		{"sri sha256", "sha256-" + fake(27, 43, base64) + "="},
		{"go.sum line", "golang.org/x/term v0.30.0 h1:" + fake(28, 43, base64) + "="},
		{"go.sum mod line", "golang.org/x/term v0.30.0/go.mod h1:" + fake(29, 43, base64) + "="},
		{"empty", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if f, ok := Find(tc.text); ok {
				t.Errorf("Find(%q) = %+v, want nothing", tc.text, f)
			}
		})
	}
}

// The first finding is reported, and the kind names what it is without
// echoing it.
func TestFindReportsFirstAndNeverTheValue(t *testing.T) {
	key := "AKIA" + "FAKEFAKEFAKEFAKE"
	text := "first " + key + " then password=" + "hunter22"
	f, ok := Find(text)
	if !ok || f.Kind != KindAWSAccessKey || f.Offset != strings.Index(text, key) {
		t.Fatalf("Find = %+v, %v; want the access key at %d", f, ok, strings.Index(text, key))
	}
	if strings.Contains(string(f.Kind), key) || strings.Contains(f.String(), key) {
		t.Errorf("finding %q echoes the value", f.String())
	}
}

// Entropy is in bits per character.
func TestEntropy(t *testing.T) {
	tests := []struct {
		in   string
		want float64
	}{
		{"", 0},
		{"aaaa", 0},
		{"abab", 1},
		{"abcd", 2},
	}
	for _, tc := range tests {
		if got := entropy(tc.in); got != tc.want {
			t.Errorf("entropy(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func FuzzFind(f *testing.F) {
	for _, s := range []string{
		"", "password=", "password=\"", "-----BEGIN ", "eyJ.eyJ.", "token: ", "\n  secret: x\n",
		"AKIA" + "FAKEFAKEFAKEFAKE", "ghp_", "xoxb-", strings.Repeat("aB3", 20), "\xff\xfe password=hunter22",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		fd, ok := Find(s)
		if !ok {
			return
		}
		if fd.Offset < 0 || fd.Offset >= len(s) || fd.Kind == "" {
			t.Fatalf("Find(%q) = %+v: offset outside the text or no kind", s, fd)
		}
	})
}
