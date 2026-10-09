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

// issueUsageLine is show's usage in one line: "held 1h; cost $8.35; opus
// 950 in, 12k out; split by time". An issue never held, with no tokens, has none.
func issueUsageLine(u proto.IssueUsage) string {
	if u.HeldSeconds == 0 && len(u.Models) == 0 {
		return ""
	}
	parts := []string{held(u.HeldSeconds)}
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
	parts := []string{held(u.HeldSeconds)}
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
