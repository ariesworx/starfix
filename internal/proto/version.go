package proto

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Protocol versions. A client speaks Proto; a server accepts any client in
// [ProtoMin, ProtoMax]. Principle 9 keeps the range one version back and
// one forward once there is more than one.
//
// Protocol 2 adds claims: start's lease and claim, the epoch on finish and
// handoff, show's claim, renew, and who (the agents registry); then the
// inbox (inbox, ack, watch and the evt frames it pushes) and the handoff
// fields on finish and handoff; then idempotency keys on comment, finish
// and handoff, the acceptance checklist (accept, finish's ticked and
// waived, close's force) and similar closed issues in create's and
// show's results; then start's take and the forbidden code (the
// authorization review). Everything after claims was added before protocol 2
// shipped in a release (v0.1.0 speaks 1), so without another bump. A
// protocol 1 client sends none of them and gets the default lease.
//
// Protocol 3 adds token capture (design §12.1): the usage op, and account
// on create and update. Protocol 2 shipped in v0.2.x, so ProtoMin moves
// to 2 and protocol 1 clients are refused. A protocol 2 client never
// sends usage and omits account, which leaves an issue inheriting its
// account; show's usage and digest's usage are result fields, which it
// ignores. Then files to issues (design §12 item 3): paths on renew,
// finish and handoff (the client's git), and declared paths on create and
// update; ready's overlaps and show's files are result fields. Protocol 3
// had not shipped in a release (v0.2.2 speaks 2), so they join it without
// another bump; a protocol 2 client sends no paths, and nothing is
// recorded for it. The live board's reads join it the same way: watch's
// events, the event push (EvEvent) and the claims op, none of which a
// protocol 2 client sends.
//
// Protocol 4 adds memory (design §6): the remember, recall, forget and
// pin ops, and start's memories, a result field older clients ignore;
// then prices and cost reports (design §12.1): the price.set, prices and
// cost ops, and show's and digest's cost, result fields; then human hours
// and subscription plans: the hours.log, hours.delete, hours, plan.set
// and plans ops, and the logged time and amortized cost in show, digest
// and cost, result fields. Protocol 4 had not shipped in a release
// (v0.3.0 speaks 3), so they join it without another bump. ProtoMin stays at 2: v0.2.x clients speak 2 and v0.3.x
// clients speak 3, and this server accepts both. Neither sends a memory,
// price, cost, hours or plan op, and both ignore start's memories and the
// cost and hours fields, so a protocol 3 client keeps working against a protocol 4
// server exactly as against a protocol 3 one, and a protocol 2 client as
// it did before protocol 3. Raising ProtoMin to 3 would refuse v0.2.x
// clients; TestReleasedClients pins the range.
const (
	Proto    = 4
	ProtoMin = 2
	ProtoMax = 4
)

// CheckProto returns nil when the client protocol version p is in
// [lo, hi], and otherwise a CodeVersion error that tells the person which
// side to upgrade. A range that is empty, or starts below 1, refuses every
// client.
func CheckProto(p, lo, hi int, serverVersion string) *Error {
	switch {
	case lo < 1 || hi < lo:
		return Errf(CodeVersion, "ask the server admin to check starfixd's protocol range",
			fmt.Sprintf("server advertises an empty protocol range [%d,%d]", lo, hi))
	case p < lo:
		return Errf(CodeVersion, "upgrade this client: `sfx upgrade`",
			fmt.Sprintf("starfix speaks protocol %d; the server (%s) needs %d to %d", p, orDev(serverVersion), lo, hi))
	case p > hi:
		return Errf(CodeVersion, "ask the server admin to run `starfixd upgrade`, or install the starfix release that matches the server",
			fmt.Sprintf("starfix speaks protocol %d; the server (%s) is older and accepts %d to %d", p, orDev(serverVersion), lo, hi))
	}
	return nil
}

// orDev names a server version in a message. A string that is neither a
// version nor "dev" came from a server that may be hostile, so it is never
// echoed (C-5).
func orDev(v string) string {
	if v == "dev" || ValidVersion(v) {
		return v
	}
	return "unknown version"
}

// maxVersion bounds a version string. Release tags are far shorter
// (release.TagPattern); the bound keeps a server-sent string from growing a
// message.
const maxVersion = 64

// versionPattern is a semantic version 2.0 with a "v" prefix: numeric
// identifiers without leading zeros, prerelease and build identifiers
// non-empty and drawn from [0-9A-Za-z-]. Anything else, including control
// characters, spaces and non-ASCII, is not a version.
var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?` +
	`(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// ValidVersion reports whether s is a release version that CompareVersions
// orders: vMAJOR.MINOR.PATCH, an optional -prerelease and +build, at most
// 64 bytes. Callers treat any other string from a server as unknown.
func ValidVersion(s string) bool {
	_, ok := parseVersion(s)
	return ok
}

// CompareVersions compares two release versions of the form
// vMAJOR.MINOR.PATCH, with an optional -prerelease that sorts before the
// release and +build metadata that is ignored, by semantic versioning 2.0
// precedence. ok is false when either is not a valid version (for example
// "dev", or a hostile string from a server); callers then stay quiet.
func CompareVersions(a, b string) (cmp int, ok bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range 3 {
		if c := cmpInt(pa.n[i], pb.n[i]); c != 0 {
			return c, true
		}
	}
	switch {
	case len(pa.pre) == 0 && len(pb.pre) == 0:
		return 0, true
	case len(pa.pre) == 0:
		return 1, true
	case len(pb.pre) == 0:
		return -1, true
	}
	for i := range min(len(pa.pre), len(pb.pre)) {
		if c := cmpIdent(pa.pre[i], pb.pre[i]); c != 0 {
			return c, true
		}
	}
	return cmpInt(len(pa.pre), len(pb.pre)), true
}

// cmpIdent orders two prerelease identifiers: numeric ones numerically and
// below alphanumeric ones, which compare in ASCII order. The pattern bounds
// a numeric identifier only by maxVersion, so numbers compare by length
// first (no leading zeros) rather than by parsing.
func cmpIdent(a, b string) int {
	na, nb := isNumeric(a), isNumeric(b)
	switch {
	case na && nb:
		if c := cmpInt(len(a), len(b)); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	case na:
		return -1
	case nb:
		return 1
	}
	return strings.Compare(a, b)
}

func isNumeric(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// semver is a parsed version: major, minor and patch, and the prerelease
// identifiers. Build metadata is dropped, since it does not affect
// precedence.
type semver struct {
	n   [3]int
	pre []string
}

// parseVersion parses s, reporting false unless it is a version as
// ValidVersion defines one.
func parseVersion(s string) (semver, bool) {
	var v semver
	if len(s) > maxVersion {
		return v, false
	}
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := range 3 {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return v, false
		}
		v.n[i] = n
	}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
	}
	return v, true
}

// OlderClientWarning returns the one-line warning for a client older than
// its server, or "" when there is nothing to say.
func OlderClientWarning(client, server string) string {
	if c, ok := CompareVersions(client, server); ok && c < 0 {
		return fmt.Sprintf("starfix %s is older than the server (%s); run `sfx upgrade`", client, server)
	}
	return ""
}

// OlderServerWarning returns the one-line warning for a server older than
// the latest release it knows of, or "".
func OlderServerWarning(server, latest string) string {
	if c, ok := CompareVersions(server, latest); ok && c < 0 {
		return fmt.Sprintf("the server runs starfixd %s; %s is out: ask the admin to run `starfixd upgrade`", server, latest)
	}
	return ""
}
