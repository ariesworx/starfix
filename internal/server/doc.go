// Package server is starfixd: the daemon that owns the store and serves the
// protocol on a unix socket, and the stdio bridge that sshd runs as each
// developer key's forced command.
//
// # Trust
//
// The daemon never authenticates anyone itself. sshd authenticates the
// developer's key, and that key's authorized_keys line forces
//
//	restrict,command="starfixd stdio --principal NAME" ssh-ed25519 AAAA…
//
// so the principal is fixed by which key logged in; whatever command the
// client asks for is ignored. The bridge connects to the daemon's socket
// and sends a bridge frame naming that principal before any client byte.
// The daemon believes the bridge frame only because the socket peer is
// local and runs as the daemon's own Unix user: the socket lives in a 0700
// directory, and on Linux each connection's SO_PEERCRED uid must equal the
// daemon's. The SSH login account (server.user) is therefore the account
// starfixd runs as, and its authorized_keys holds only restricted lines.
// A bridge frame anywhere but first is refused.
//
// # Serving
//
// [New] checks a [Config] and returns a [Server]; [Listen] opens the
// socket, and [Server.Serve] accepts on it until its context ends. Each
// connection gets a goroutine that checks the peer, reads the bridge frame
// and the hello, answers with a welcome, then answers requests in order.
// [Server.Dispatch] decodes each op's arguments strictly and calls the
// store; a refusal is a [proto.Error] whose fix names the next action. A
// connection that sends watch gets a second goroutine, which pushes its
// new inbox items and never outlives the connection. A reaper goroutine
// ends expired claims and prunes old rows ([Server.Reap], [Server.Prune]).
// Serve waits for all of them before it returns.
//
// [Limits] bound what one principal can make the daemon do: its
// connections, their idle time, its write rate and its refusal log lines,
// besides the store's limits. They are counted in memory, per daemon.
//
// [ResolveSettings] builds the starfixd commands' [Settings] from flags,
// the environment and a config file. [Bridge] is `starfixd stdio`, and
// [Probe] the health check `starfixd upgrade` runs after a restart.
//
// The design is docs/design/starfix.md: §2 for the transport and the
// trust model, and the "As built" notes in §7 for claims, the inbox,
// authorization and the limits.
package server
