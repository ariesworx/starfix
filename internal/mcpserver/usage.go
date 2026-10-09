package mcpserver

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ariesworx/starfix/internal/proto"
)

// Usage lines bound what one result spends on usage, however many
// models a principal reported.
const (
	// usageModels is how many models a line names, the largest first.
	usageModels = 5
	// usageModelLen is how much of a model name a line keeps.
	usageModelLen = 64
)

// issueUsageLine is show's usage in one line: "held 1h; logged 1.5h;
// cost $8.35; opus 950 in, 12k out; split by time". An issue never held,
// with no tokens and no hours logged, has none.
func issueUsageLine(u proto.IssueUsage) string {
	parts := heldAndLogged(u.HeldSeconds, len(u.Models) > 0, u.LoggedSeconds)
	if len(parts) == 0 {
		return ""
	}
	if u.CostUSD != "" {
		parts = append(parts, "cost "+costText(u.CostUSD, u.Unpriced))
	}
	if m := modelsLine(u.Models); m != "" {
		parts = append(parts, m)
	}
	if u.Split {
		parts = append(parts, "split by time")
	}
	if u.Capped {
		parts = append(parts, "partial")
	}
	return strings.Join(parts, "; ")
}

// digestUsageLine is digest's usage in one line, with the tokens no
// issue was held for.
func digestUsageLine(u proto.DigestUsage) string {
	parts := heldAndLogged(u.HeldSeconds, len(u.Models) > 0, u.LoggedSeconds)
	if u.CostUSD != "" {
		parts = append(parts, "cost "+costText(u.CostUSD, u.Unpriced))
	}
	if m := modelsLine(u.Models); m != "" {
		parts = append(parts, m)
	}
	if m := modelsLine(u.Unattributed); m != "" {
		parts = append(parts, "unattributed: "+m)
	}
	if u.Split {
		parts = append(parts, "split by time")
	}
	return strings.Join(parts, "; ")
}

func held(s int64) string { return "held " + proto.Span(time.Duration(s)*time.Second) }

// heldAndLogged starts a usage line: the time held, when anything was
// held or tokens reported, then the hours people logged, if any.
func heldAndLogged(heldSeconds int64, tokens bool, logged int64) []string {
	var parts []string
	if heldSeconds > 0 || tokens {
		parts = append(parts, held(heldSeconds))
	}
	if logged > 0 {
		parts = append(parts, "logged "+proto.Hours(logged))
	}
	return parts
}

// modelsLine names the usageModels largest models and their known
// counts, and how many more there are.
func modelsLine(ms []proto.ModelTokens) string {
	ms = slices.Clone(ms)
	slices.SortStableFunc(ms, func(a, b proto.ModelTokens) int { return cmp.Compare(total(b), total(a)) })
	var parts []string
	for _, m := range ms[:min(len(ms), usageModels)] {
		var counts []string
		for _, c := range []struct {
			v    *int64
			name string
		}{{m.Input, "in"}, {m.Output, "out"}, {m.CacheWrite, "cache write"}, {m.CacheRead, "cache read"}} {
			if c.v != nil {
				counts = append(counts, proto.TokenCount(*c.v)+" "+c.name)
			}
		}
		if len(counts) == 0 {
			counts = []string{"unknown"}
		}
		name, _ := cut(m.Model, usageModelLen)
		parts = append(parts, name+" "+strings.Join(counts, ", "))
	}
	if more := len(ms) - usageModels; more > 0 {
		parts = append(parts, fmt.Sprintf("%d more models", more))
	}
	return strings.Join(parts, "; ")
}

// total is a model's known tokens, to rank it.
func total(m proto.ModelTokens) int64 {
	var n int64
	for _, c := range []*int64{m.Input, m.Output, m.CacheWrite, m.CacheRead} {
		if c != nil {
			n += *c
		}
	}
	return n
}
