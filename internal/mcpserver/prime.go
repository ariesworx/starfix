package mcpserver

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/ariesworx/starfix/internal/proto"
)

// Prime is a new session's orientation, held under MaxPrimeTokens.
type Prime struct {
	Project string `json:"project"`
	// You is the principal the server knows this key as.
	You     string `json:"you,omitempty"`
	Session string `json:"session"`
	// Working are your in_progress issues (assigned to you).
	Working []proto.Summary `json:"in_progress"`
	// Ready is the top of the ready queue.
	Ready []proto.Summary `json:"ready"`
	// More: an issue list was cut to fit the budget.
	More    bool     `json:"more,omitempty"`
	Notices []string `json:"notices,omitempty"`
	// Lost are issues whose claim this session lost, lapsed or taken
	// over, from its unread claim.lost inbox items: stop work on them.
	Lost []string `json:"lost,omitempty"`
	// Unread counts your unread inbox items; Inbox is the newest few.
	Unread int         `json:"unread,omitempty"`
	Inbox  []InboxItem `json:"inbox,omitempty"`
}

// How much prime shows.
const (
	primeWorking  = 5
	primeReady    = 5
	primeInbox    = 3
	primeTitleLen = 100
	// primeLostFrom is how many unread items prime reads to find lost
	// claims.
	primeLostFrom = 20
)

// BuildPrime reads the caller's in-progress issues, the top ready ones and
// the inbox. Titles are cut, and issues dropped from the end, until it
// fits MaxPrimeTokens. A server without the inbox leaves it out.
func BuildPrime(ctx context.Context, c Conn, clientVersion string) (*Prime, error) {
	p := &Prime{Project: c.Project(), You: c.Principal(), Session: c.Session(),
		Working: []proto.Summary{}, Ready: []proto.Summary{}, Notices: c.Notices(clientVersion)}
	if p.You != "" {
		var r proto.ListResult
		if err := c.Call(ctx, proto.OpList, proto.ListArgs{Status: []string{"in_progress"}, Assignee: p.You,
			Limit: primeWorking}, &r); err != nil {
			return nil, err
		}
		p.Working, p.More = r.Issues, r.Next != ""
	}
	var r proto.ListResult
	if err := c.Call(ctx, proto.OpReady, proto.LimitArgs{Limit: primeReady}, &r); err != nil {
		return nil, err
	}
	p.Ready = r.Issues
	var in proto.InboxResult
	switch err := c.Call(ctx, proto.OpInbox, proto.InboxArgs{Limit: primeLostFrom}, &in); {
	case err == nil:
		p.Unread = in.Unread
		for _, it := range in.Items {
			if it.Kind == "claim.lost" && it.Issue != "" && !slices.Contains(p.Lost, it.Issue) {
				p.Lost = append(p.Lost, it.Issue)
			}
		}
		p.Inbox = compactItems(in.Items[:min(len(in.Items), primeInbox)])
	case c.Err() != nil:
		return nil, err
	}
	p.fit()
	return p, nil
}

// fit holds p under MaxPrimeTokens.
func (p *Prime) fit() {
	for _, list := range [][]proto.Summary{p.Working, p.Ready} {
		for i := range list {
			list[i].Title, _ = cut(list[i].Title, primeTitleLen)
		}
	}
	for i := range p.Notices {
		p.Notices[i], _ = cut(p.Notices[i], 300)
	}
	for i := range p.Inbox {
		p.Inbox[i].Body, _ = cut(p.Inbox[i].Body, primeTitleLen)
	}
	for size(p) > MaxPrimeTokens {
		switch {
		case len(p.Inbox) > 0:
			p.Inbox = p.Inbox[:len(p.Inbox)-1]
		case len(p.Ready) > 0:
			p.Ready = p.Ready[:len(p.Ready)-1]
		case len(p.Working) > 0:
			p.Working = p.Working[:len(p.Working)-1]
		case len(p.Notices) > 0:
			p.Notices = p.Notices[:len(p.Notices)-1]
		default:
			return
		}
		p.More = true
	}
}

// Text renders prime for a person or a SessionStart hook.
func (p *Prime) Text() string {
	var b strings.Builder
	who := p.You
	if who == "" {
		who = "unknown"
	}
	fmt.Fprintf(&b, "starfix: project %s, you are %s, session %s\n", p.Project, who, p.Session)
	for _, n := range p.Notices {
		fmt.Fprintf(&b, "notice: %s\n", n)
	}
	if len(p.Lost) > 0 {
		fmt.Fprintf(&b, "lost claim (stop work on it): %s\n", strings.Join(p.Lost, " "))
	}
	if p.Unread > 0 {
		fmt.Fprintf(&b, "inbox: %d unread (call inbox)\n", p.Unread)
		for _, it := range p.Inbox {
			fmt.Fprintf(&b, "  #%d %s %s from %s: %s\n", it.ID, it.Kind, it.Issue, it.From, it.Body)
		}
	}
	section := func(name, empty string, list []proto.Summary) {
		if len(list) == 0 {
			fmt.Fprintf(&b, "%s: %s\n", name, empty)
			return
		}
		fmt.Fprintf(&b, "%s:\n", name)
		for _, s := range list {
			fmt.Fprintf(&b, "  %s P%d %s\n", s.ID, s.Priority, s.Title)
		}
	}
	section("in progress", "none", p.Working)
	section("ready", "nothing ready", p.Ready)
	if p.More {
		b.WriteString("(more not shown: use list and ready)\n")
	}
	b.WriteString("next: start (the top ready issue, or an id), then finish when done\n")
	return b.String()
}
