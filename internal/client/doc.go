// Package client connects to starfixd over SSH and speaks the protocol.
//
// The SSH client runs in-process (golang.org/x/crypto/ssh); no system ssh
// is used. The server's ED25519 host key is pinned in .starfix.yaml and
// checked on every connection: a mismatch is refused, and an unpinned host
// is never trusted on first use. The developer authenticates with a key
// from ssh-agent (SSH_AUTH_SOCK, or the OpenSSH named pipe on Windows) or
// the key file named in the config.
//
// [LoadConfig] finds and checks the repository's [Config]. [Dial] opens a
// [Conn] over TCP or, when the config names a Compute Engine instance
// ([IAP]), over `gcloud compute start-iap-tunnel`, with the same host key
// pin and key authentication. A Conn's calls are serialized, and a reader
// goroutine hands each response to its call and each pushed event to
// Options.OnPush. A refusal, the server's or the client's own, is a
// [proto.Error] with a fix.
//
// [SessionFromEnv] and [HarnessFromEnv] read the session and the agent
// harness a process runs under, so the server can tell one agent's claims
// from another's.
//
// The design is docs/design/starfix.md: §2, with its note on the IAP
// transport, and §5's note on security review batch 3 for bounded config
// discovery.
package client
