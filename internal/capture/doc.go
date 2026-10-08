// Package capture reads token usage from the records an agent harness
// keeps on this machine, for `sfx usage --hook` to send to starfixd
// (design §12.1).
//
// Only counts leave the machine: for each API request, the harness, its
// message id, the model, the time and the token counts. The conversation
// in a transcript is never kept: each line is decoded into a struct that
// holds those fields alone, and a line is dropped once read.
//
// Claude Code is the one harness read so far ([Claude]). A transcript is
// read from the offset the last run kept, in a state file the caller
// names, so each run reads only what is new; the server keeps one record
// per request, so reading something twice never counts it twice.
package capture
