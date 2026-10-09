package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// printIssueUsage prints show's account, time held and tokens by model.
// own is the issue's own account, empty when it inherits one. A server
// that sends no usage prints nothing.
func printIssueUsage(w io.Writer, own string, u *proto.IssueUsage) {
	if u == nil {
		return
	}
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("account: %s", esc(u.Account))
	switch {
	case own != "":
	case u.AccountFrom != "":
		p(" (from %s)", esc(u.AccountFrom))
	default:
		p(" (default)")
	}
	p("\n")
	if u.HeldSeconds != 0 || len(u.Models) != 0 {
		p("held %s", proto.Span(time.Duration(u.HeldSeconds)*time.Second))
		if u.Split {
			p("; some tokens split by time with other work")
		}
		if u.Capped {
			p("; tokens partial")
		}
		p("\n")
		printCostLine(w, u.CostUSD, u.Unpriced)
	}
	if u.LoggedSeconds > 0 {
		who := make([]string, len(u.Logged))
		for i, l := range u.Logged {
			who[i] = esc(l.Principal) + " " + proto.Hours(l.Seconds)
		}
		p("logged %s: %s\n", proto.Hours(u.LoggedSeconds), strings.Join(who, ", "))
	}
	printModels(w, "tokens", u.Models)
}

// printDigestUsage prints a digest's time held and tokens, with the
// tokens no issue was held for; nil prints nothing.
func printDigestUsage(w io.Writer, u *proto.DigestUsage) {
	if u == nil {
		return
	}
	if u.HeldSeconds != 0 || len(u.Models) != 0 {
		_, _ = fmt.Fprintf(w, "held %s", proto.Span(time.Duration(u.HeldSeconds)*time.Second))
		if u.Split {
			_, _ = fmt.Fprint(w, "; some tokens split by time")
		}
		_, _ = fmt.Fprintln(w)
		printCostLine(w, u.CostUSD, u.Unpriced)
	}
	if u.LoggedSeconds > 0 {
		_, _ = fmt.Fprintf(w, "logged %s\n", proto.Hours(u.LoggedSeconds))
	}
	printModels(w, "tokens", u.Models)
	printModels(w, "unattributed", u.Unattributed)
}

// printModels prints one line per model: what, the model, then each count
// it knows.
func printModels(w io.Writer, what string, ms []proto.ModelTokens) {
	for _, m := range ms {
		var parts []string
		for _, c := range []struct {
			v    *int64
			name string
		}{{m.Input, "in"}, {m.Output, "out"}, {m.CacheWrite, "cache write"}, {m.CacheRead, "cache read"}} {
			if c.v == nil {
				continue
			}
			part := proto.TokenCount(*c.v) + " " + c.name
			if c.name == "cache write" && m.CacheWrite1h != nil && *m.CacheWrite1h > 0 {
				part += " (" + proto.TokenCount(*m.CacheWrite1h) + " 1h)"
			}
			parts = append(parts, part)
		}
		if len(parts) == 0 {
			parts = []string{"unknown"}
		}
		_, _ = fmt.Fprintf(w, "%s %s: %s\n", what, esc(m.Model), strings.Join(parts, ", "))
	}
}
