package proto

import (
	"fmt"
	"strconv"
	"strings"
)

// Protocol versions. A client speaks Proto; a server accepts any client in
// [ProtoMin, ProtoMax]. Principle 9 keeps the range one version back and
// one forward once there is more than one.
//
// Protocol 2 adds claims: start's lease and claim, the epoch on finish and
// handoff, show's claim, and renew. A protocol 1 client sends none of
// them and gets the default lease.
const (
	Proto    = 2
	ProtoMin = 1
	ProtoMax = 2
)

// CheckProto refuses a client protocol version p outside [lo, hi]. The
// error tells the person which side to upgrade.
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

func orDev(v string) string {
	if v == "" {
		return "unknown version"
	}
	return v
}

// CompareVersions compares two release versions of the form vMAJOR.MINOR.PATCH,
// with an optional -prerelease that sorts before the release. ok is false
// when either is not a release version (for example "dev"); callers then
// stay quiet.
func CompareVersions(a, b string) (cmp int, ok bool) {
	pa, oka := parseVersion(a)
	pb, okb := parseVersion(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range 3 {
		if pa.n[i] != pb.n[i] {
			if pa.n[i] < pb.n[i] {
				return -1, true
			}
			return 1, true
		}
	}
	switch {
	case pa.pre == pb.pre:
		return 0, true
	case pa.pre == "":
		return 1, true
	case pb.pre == "":
		return -1, true
	case pa.pre < pb.pre:
		return -1, true
	default:
		return 1, true
	}
}

type semver struct {
	n   [3]int
	pre string
}

func parseVersion(s string) (semver, bool) {
	var v semver
	s, ok := strings.CutPrefix(s, "v")
	if !ok {
		return v, false
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre = s[i+1:]
		if v.pre == "" {
			return v, false
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" || (len(p) > 1 && p[0] == '0') {
			return v, false
		}
		v.n[i] = n
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
