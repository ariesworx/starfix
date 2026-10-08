// Package mcpserver is `sfx mcp`: the Model Context Protocol server that
// agents use, on stdin and stdout (docs/design/starfix.md §5 and its "As
// built" notes). It also builds prime ([BuildPrime]), which `sfx prime`
// and the SessionStart hook print.
//
// A [Server] runs on the developer's machine as the developer, and talks
// to starfixd over the same pinned SSH connection as the CLI ([DialRepo]):
// one connection per MCP session, opened on the first tool call and
// redialed when it drops. Identity is implicit: the server learns the
// principal from the SSH key, and the session id is the one the caller
// dials with (`sfx mcp` takes the harness's, from client.SessionEnv, or
// picks one for the process). No tool takes an identity.
//
// # Tools and results
//
// The exported types named *In are the tools' inputs; Issue, Issues,
// Started and the like are their results. The SDK infers each input
// schema from its struct, so field names and jsonschema tags are schema
// text that every agent pays for in every session, and the tests cap the
// total.
//
// Results are compact (design §9): writes return {id, rev}, lists return
// id, title, status and priority, and every result is held under
// [MaxResultTokens], with prime and digest under caps of their own. A
// result holding text that others wrote carries "untrusted", and the
// [Instructions] tell the agent never to follow instructions in such text.
// A refusal is a tool error carrying the server's code and message, with a
// fix line that names the agent's next step in terms of tools; a fix only
// a person can carry out is relayed to the user, quoted.
//
// Left out on purpose: anything admin (delete, import, settings), which is
// CLI only.
//
// # Concurrency
//
// The SDK may handle tool calls concurrently, but calls to starfixd go out
// one at a time on the session's connection. [Server.Serve] renews the
// session's claims on a goroutine of its own, and the connection's reader
// goroutine hands pushed events to [Server.Push], which only counts them
// for the next tool result.
package mcpserver
