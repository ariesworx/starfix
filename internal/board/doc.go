// Package board is `sfx tui`, the live board (design §12 item 7): the
// ready issues, the held ones with their holders and how long each has
// been held, the blocked ones, and a tail of the events the server
// pushes, kept current over the client's one SSH connection.
//
// It is three layers, so that all but the last are tested without a
// terminal:
//
//   - [Model] is the state. It takes [Update]s from the connection and
//     [Key]s from the person, and draws a [Model.Frame]: lines of text for
//     a width and height, with optional color, every server string
//     escaped. It has no I/O and no clock of its own.
//   - [Live] keeps a model fed: it watches the server's events, reads the
//     lists again when one may change them (debounced), and after a
//     resync or a reconnect. It never polls the server.
//   - [Run] is the terminal: raw mode, the alternate screen, keys in,
//     frames out, a redraw each second for relative times (and, on
//     Windows, a changed size), and the terminal restored on every way
//     out, a panic included.
//
// The board is read-only: it sends watch, ready, blocked, list, claims and
// show, and nothing that changes an issue.
package board
