// Package mcpserver is `sfx mcp`: the Model Context Protocol server
// agents use, on stdin and stdout (design §5).
//
// It runs on the developer's machine as the developer, and talks to
// starfixd over the same pinned SSH connection as the CLI: one connection
// per MCP session, opened on the first tool call and redialed when it
// drops. Identity is implicit: the server learns the principal from the
// SSH key, and the session id comes from the harness's environment
// (client.SessionEnv) or is assigned by the server.
//
// Results are compact (design §9): writes return {id, rev}, lists return
// id, title, status and priority, and every result is held under
// MaxResultTokens. A refusal is a tool error carrying the server's code
// and message, with a fix line that names the agent's next step.
//
// Left out on purpose: anything admin (delete, import, settings), which is
// CLI only.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ariesworx/starfix/internal/proto"
)

// Conn is the part of *client.Conn the server uses, so tests can stand in
// for the network.
type Conn interface {
	// Call runs one operation; a server refusal is a *proto.Error.
	Call(ctx context.Context, op string, args, result any) error
	// Err is non-nil once the connection is lost.
	Err() error
	Close() error
	Principal() string
	Session() string
	// Notices are one-line version warnings for prime.
	Notices(clientVersion string) []string
	// Project names the project for people and agents.
	Project() string
}

// Options configure the server.
type Options struct {
	// Version is this binary's build version.
	Version string
	// Dial opens a connection. It is called on the first tool call and
	// again after a connection is lost.
	Dial func(ctx context.Context) (Conn, error)
	// RenewEvery is how often the claims this session took are renewed
	// while it runs. Default 60s; negative turns renewal off.
	RenewEvery time.Duration
}

// Lease is the claim an agent's start takes. Serve renews it every
// RenewEvery, so a session that ends lets its issues go within a lease.
const Lease = "15m"

// Instructions is what an agent reads when it connects.
const Instructions = "Issue tracker shared by every agent and person on this project. " +
	"Call prime at the start of a session. Take work with start (the top ready issue, or an id): " +
	"it claims the issue while this session runs and returns it, its last handoff and a branch name. Comment as you go. " +
	"End with finish: it closes the issue, records your handoff and files work you discovered. " +
	"To stop without closing, call handoff (release lets another start it). " +
	"For a standup or status report, call digest and write the narrative from it; who lists the agents at work. " +
	"A line \"inbox: N new\" after a result means call inbox: a lost claim, a handoff, a mention or an assignment. " +
	"Writes return {id, rev}; pass rev to update or close to refuse a stale edit. " +
	"On an error, follow its fix line."

// Server is one MCP server and its connection to starfixd.
type Server struct {
	mcp    *mcp.Server
	link   *link
	opts   Options
	claims claims
	pushed pushed
}

// claims are the issues this session took and their epochs, so finish and
// handoff are fenced, and the renewer knows what to keep alive. A claim
// the session lost reaches the agent as a claim.lost inbox item.
type claims struct {
	mu   sync.Mutex
	held map[string]int64
}

func (c *claims) take(id string, epoch int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.held[id] = epoch
}

// epoch returns the epoch this session took id at, or 0.
func (c *claims) epoch(id string) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.held[id]
}

func (c *claims) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.held, id)
}

func (c *claims) any() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.held) > 0
}

// keep replaces the held set with what the server says this session still
// holds; the rest were lost, and the server's inbox says so.
func (c *claims) keep(still []proto.Claim) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := map[string]int64{}
	for _, cl := range still {
		if e, ok := c.held[cl.ID]; ok && e == cl.Epoch {
			now[cl.ID] = e
		}
	}
	c.held = now
}

// New returns the server. Call Close when done with it. Wire the
// connections' pushed events (client.Options.OnPush) to Push.
func New(opts Options) *Server {
	if opts.RenewEvery == 0 {
		opts.RenewEvery = time.Minute
	}
	s := &Server{opts: opts, link: &link{dial: opts.Dial}, claims: claims{held: map[string]int64{}}}
	s.mcp = mcp.NewServer(&mcp.Implementation{Name: "starfix", Version: opts.Version},
		&mcp.ServerOptions{Instructions: Instructions, Capabilities: &mcp.ServerCapabilities{}})
	s.register()
	return s
}

// MCP returns the underlying SDK server, for tests and other transports.
func (s *Server) MCP() *mcp.Server { return s.mcp }

// Close closes the connection to starfixd, if one is open.
func (s *Server) Close() error { return s.link.close() }

// Serve runs the server on in and out until the client disconnects.
// Nothing else may write to out: it carries the protocol.
func (s *Server) Serve(ctx context.Context, in io.ReadCloser, out io.WriteCloser) error {
	defer func() { _ = s.Close() }()
	if s.opts.RenewEvery > 0 {
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go s.renewLoop(rctx)
	}
	if err := s.mcp.Run(ctx, &mcp.IOTransport{Reader: in, Writer: out}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("mcp: %w", err)
	}
	return nil
}

func (s *Server) renewLoop(ctx context.Context) {
	t := time.NewTicker(s.opts.RenewEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Renew(ctx)
		}
	}
}

// Renew extends the claims this session holds, if any, and notes the ones
// it no longer holds. It also keeps the session in the server's agents
// registry, so it renews while connected even holding nothing; it does
// not dial just for that. A failure is left for the next tick: the lease
// is many ticks long.
func (s *Server) Renew(ctx context.Context) {
	if !s.claims.any() && !s.link.connected() {
		return
	}
	var r proto.ClaimsResult
	err := s.link.with(ctx, true, func(c Conn) error {
		return c.Call(ctx, proto.OpRenew, proto.RenewArgs{Lease: Lease}, &r)
	})
	if err == nil {
		s.claims.keep(r.Claims)
	}
}

// link holds the session's one connection, redialing after a loss. Each
// new connection watches the inbox, and watches again after a resync.
type link struct {
	dial func(ctx context.Context) (Conn, error)
	mu   sync.Mutex
	conn Conn
	// rewatch: the server stopped pushing (a resync); watch again on the
	// next call. Set from the reader goroutine, so not under mu.
	rewatch atomic.Bool
}

// lostError is a call that failed because the connection dropped.
type lostError struct {
	err error
	// retried: the call was retried on a new connection and failed again.
	// Otherwise it was a write that is not safe to repeat blindly.
	retried bool
}

func (e *lostError) Error() string { return e.err.Error() }
func (e *lostError) Unwrap() error { return e.err }

// dialError is a failure to connect at all; its fix is for the person.
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// get returns the open connection, dialing if there is none. l.mu is held.
func (l *link) get(ctx context.Context) (Conn, error) {
	if l.conn != nil && l.conn.Err() == nil {
		return l.conn, nil
	}
	if l.conn != nil {
		_ = l.conn.Close()
		l.conn = nil
	}
	c, err := l.dial(ctx)
	if err != nil {
		return nil, &dialError{err: err}
	}
	l.conn = c
	l.rewatch.Store(false)
	watch(ctx, c)
	return c, nil
}

// with runs fn on the connection. When the connection is lost during fn
// and retry is set, fn runs once more on a new connection; retry is for
// reads and for writes that are safe to repeat.
func (l *link) with(ctx context.Context, retry bool, fn func(Conn) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for attempt := 0; ; attempt++ {
		c, err := l.get(ctx)
		if err != nil {
			return err
		}
		if l.rewatch.Swap(false) {
			watch(ctx, c)
		}
		err = fn(c)
		if err == nil || c.Err() == nil {
			return err
		}
		_ = c.Close()
		l.conn = nil
		if !retry || attempt > 0 || ctx.Err() != nil {
			return &lostError{err: err, retried: retry && attempt > 0}
		}
	}
}

// connected reports whether a connection is open and not known lost.
func (l *link) connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil && l.conn.Err() == nil
}

func (l *link) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil
	}
	err := l.conn.Close()
	l.conn = nil
	return err
}

// Tool annotations: read-only tools say so, for clients that auto-approve
// them; no tool deletes anything.
var (
	readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
	write    = &mcp.ToolAnnotations{DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
	idem     = &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)}
)

// enums maps an input field to its allowed values.
type enums map[string][]any

// tool describes one tool for add.
type tool struct {
	name, desc string
	ann        *mcp.ToolAnnotations
	enums      enums
	// retry: safe to run again on a new connection if the first one drops.
	retry bool
	// showsInbox: the result shows the inbox, so it resets the count of
	// pushed items instead of carrying the "inbox: N new" line.
	showsInbox bool
}

// add registers one tool. The input schema is inferred from In, with
// priority bounded to 0-4, limit to at least 1, and the given enums. No output
// schema is published: it would double the schema's token cost, and the
// result is self-describing JSON.
func add[In, Out any](s *Server, t tool, h func(context.Context, Conn, In) (Out, error)) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("mcpserver: schema for %s: %v", t.name, err)) // a programming error, caught by the tests
	}
	// Optional fields are pointers or slices, which infer as nullable.
	// Omitting a field already says "not given", and the null costs
	// tokens in every session.
	notNull(schema)
	for field, values := range t.enums {
		prop(schema, field).Enum = values
	}
	for _, field := range []string{"priority", "discovered.priority"} {
		if p := prop(schema, field); p != nil {
			p.Minimum, p.Maximum = ptr(0.0), ptr(4.0)
		}
	}
	if p := schema.Properties["limit"]; p != nil {
		p.Minimum = ptr(1.0) // the server clamps the maximum
	}
	mcp.AddTool(s.mcp, &mcp.Tool{Name: t.name, Description: t.desc, Annotations: t.ann, InputSchema: schema},
		func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			if p, ok := any(&in).(preparer); ok {
				if err := p.prepare(); err != nil {
					return toolError(err), nil, nil
				}
			}
			var out Out
			err := s.link.with(ctx, t.retry, func(c Conn) error {
				var err error
				out, err = h(ctx, c, in)
				return err
			})
			// Counted after the call: the server pushes what was committed
			// before it answers, so this result reports it.
			line := s.notice()
			if t.showsInbox && err == nil {
				line = ""
			}
			if err != nil {
				res := toolError(err)
				if line != "" {
					res.Content = append(res.Content, &mcp.TextContent{Text: line})
				}
				return res, nil, nil
			}
			if line == "" {
				return nil, out, nil
			}
			b, err := json.Marshal(out)
			if err != nil {
				return toolError(fmt.Errorf("encode %s result: %w", t.name, err)), nil, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}, &mcp.TextContent{Text: line}}}, out, nil
		})
}

// notNull makes nullable properties, at any depth, plain.
func notNull(s *jsonschema.Schema) {
	for _, p := range s.Properties {
		if len(p.Types) == 2 && p.Types[0] == "null" {
			p.Type, p.Types = p.Types[1], nil
		}
		notNull(p)
	}
	if s.Items != nil {
		notNull(s.Items)
	}
}

// prop finds a property by a dotted path, stepping into array items; a
// property that is an array yields its items. It returns nil if absent.
func prop(s *jsonschema.Schema, path string) *jsonschema.Schema {
	for name := range strings.SplitSeq(path, ".") {
		if s.Items != nil {
			s = s.Items
		}
		if s = s.Properties[name]; s == nil {
			return nil
		}
	}
	if s.Items != nil {
		s = s.Items
	}
	return s
}

// preparer is an input that sets itself up once per call, before any
// attempt.
type preparer interface{ prepare() error }

// errorDoc is the structured form of a tool error.
type errorDoc struct {
	Error toolErr `json:"error"`
}

type toolErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix"`
}

// toolError turns err into a tool error result: "code: message" and a
// "fix:" line naming the next step.
func toolError(err error) *mcp.CallToolResult {
	e := explain(err)
	doc, _ := json.Marshal(errorDoc{Error: e})
	return &mcp.CallToolResult{
		IsError:           true,
		Content:           []mcp.Content{&mcp.TextContent{Text: e.Code + ": " + e.Message + "\nfix: " + e.Fix}},
		StructuredContent: json.RawMessage(doc),
	}
}

func ptr[T any](v T) *T { return &v }
