package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
)

// costBys are the groupings the cost tool takes.
var costBys = []any{"account", "issue", "epic", "person", "model"}

// costGroups is how many groups the cost tool asks for; the server sums
// the rest into (other).
const costGroups = 20

// CostIn selects a cost report.
type CostIn struct {
	By    string `json:"by,omitempty"`
	Since string `json:"since" jsonschema:"7d or a date"`
	Until string `json:"until,omitempty"`
}

// Cost is a cost report in lines: each group's list-price cost and
// tokens, the total, and the models with no price.
type Cost struct {
	untrusted
	By        string   `json:"by"`
	Since     string   `json:"since"`
	Until     string   `json:"until"`
	Groups    []string `json:"groups"`
	Total     string   `json:"total"`
	Unpriced  []string `json:"unpriced,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

func (r *Cost) mark() { r.set(len(r.Groups) > 0) }

func (s *Server) registerCost() {
	add(s, tool{name: "cost", desc: "Token cost at list price.", ann: readOnly, retry: true,
		enums: enums{"by": costBys}},
		func(ctx context.Context, c Conn, in CostIn) (Cost, error) { return cost(ctx, c, in) })
}

// cost is the cost tool. A report that would pass MaxResultTokens is
// asked for again with fewer groups, so the server's (other) keeps the
// groups adding up to the total.
func cost(ctx context.Context, c Conn, in CostIn) (Cost, error) {
	args := proto.CostArgs{By: orDefault(in.By, "account"), Since: in.Since, Until: in.Until, Limit: costGroups}
	for {
		var r proto.CostResult
		if err := c.Call(ctx, proto.OpCost, args, &r); err != nil {
			return Cost{}, err
		}
		out := Cost{By: r.By, Since: stamp(r.Since), Until: stamp(r.Until), Groups: []string{}, Truncated: r.Truncated,
			Total: costText(r.Total.CostUSD, r.Total.Unpriced) + ", " + proto.TokenCount(tokenSum(r.Total.Tokens)) + " tokens"}
		for _, g := range r.Groups {
			out.Groups = append(out.Groups, groupLine(g))
		}
		for _, m := range r.Unpriced[:min(len(r.Unpriced), usageModels)] {
			name, _ := cut(m, usageModelLen)
			out.Unpriced = append(out.Unpriced, name)
		}
		if more := len(r.Unpriced) - usageModels; more > 0 {
			out.Unpriced = append(out.Unpriced, fmt.Sprintf("%d more models", more))
		}
		if size(out) <= MaxResultTokens || args.Limit <= 1 {
			return out, nil
		}
		args.Limit /= 2
	}
}

// groupLine is one group: "KEY TITLE: $8.35, 4.3M tokens, split".
func groupLine(g proto.CostGroup) string {
	head := g.Key
	if g.Title != "" {
		title, _ := cut(strings.Join(strings.Fields(g.Title), " "), usageModelLen)
		head += " " + title
	}
	line := head + ": " + costText(g.CostUSD, g.Unpriced) + ", " + proto.TokenCount(tokenSum(g.Tokens)) + " tokens"
	if g.Split {
		line += ", split"
	}
	return line
}

// costText is a cost for an agent: "$8.35", "$8.35, some unpriced", or
// "unpriced" when no token had a price, which is not free.
func costText(usd string, unpriced bool) string {
	switch {
	case unpriced && strings.Trim(usd, "0.") == "":
		return "unpriced"
	case unpriced:
		return proto.Dollars(usd) + ", some unpriced"
	}
	return proto.Dollars(usd)
}

// tokenSum is the counts t knows, added up.
func tokenSum(t proto.Tokens) int64 { return total(proto.ModelTokens{Tokens: t}) }
