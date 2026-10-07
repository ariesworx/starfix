// Package proto is the wire protocol between starfix and starfixd.
//
// # Transport
//
// The client opens an SSH session to the server's own sshd. Each developer
// key's authorized_keys line forces `starfixd stdio --principal NAME`, so
// the principal comes from the key that authenticated, never from the
// client. That bridge connects to the daemon's unix socket, writes a bridge
// frame naming the principal, then copies bytes both ways. The daemon
// trusts the bridge frame only because the socket peer is local and runs
// as the daemon's own user (checked with SO_PEERCRED on Linux, and by the
// socket directory's 0700 mode everywhere).
//
// # Frames
//
// The stream is newline-delimited JSON, one Frame per line, at most
// MaxFrame bytes. Empty fields are omitted. The short keys are:
//
//	t       frame type: bridge, hello, welcome, req, res, evt
//	id      request id, chosen by the client; a res echoes it
//	op      operation name (req); see the Op constants
//	a       operation arguments (req), one of the *Args types
//	ok      result (res), one of the *Result types
//	err     typed error (res, or welcome when refused): {c, m, fix}
//	v       sender's build version (hello, welcome)
//	p       client protocol version (hello)
//	min,max protocol range the server accepts (welcome)
//	proj    project UUID (hello)
//	s       session id: the client's (hello), the effective one (welcome)
//	m       machine name (hello)
//	h       agent harness, such as claude-code (hello; may be empty)
//	latest  latest release the server knows of (welcome; may be empty)
//	pr      principal: the bridge's claim, and the welcome's echo of it
//
// # Sequence
//
//	bridge → daemon:  {"t":"bridge","pr":"ed"}
//	client → daemon:  {"t":"hello","v":"v0.1.0","p":1,"proj":"…","s":"…","m":"laptop"}
//	daemon → client:  {"t":"welcome","v":"v0.1.0","min":1,"max":1,"s":"…","pr":"ed"}
//	client → daemon:  {"t":"req","id":1,"op":"show","a":{"id":"sf-a1b2c3d4"}}
//	daemon → client:  {"t":"res","id":1,"ok":{…}}
//
// A client whose protocol version is outside [min,max] gets a welcome
// carrying err with code "version", and the connection closes.
//
// Frame type "evt" is reserved for server-pushed events (stage 3). A client
// ignores frame types it does not know, so the server can add them without
// a protocol bump; the server refuses unknown types from clients.
package proto
