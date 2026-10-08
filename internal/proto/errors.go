package proto

// Code classifies an error for programs. People read Message and Fix.
type Code string

// Error codes.
const (
	// CodeNotFound: the issue (or other target) does not exist.
	CodeNotFound Code = "not_found"
	// CodeConflict: the target changed since the revision the caller read.
	CodeConflict Code = "conflict"
	// CodeExists: an issue with that ID already exists.
	CodeExists Code = "exists"
	// CodeCycle: the change would create a dependency cycle.
	CodeCycle Code = "cycle"
	// CodeInvalid: the request failed validation.
	CodeInvalid Code = "invalid"
	// CodeAcceptance: close or finish was refused because acceptance
	// items are neither ticked nor waived, or an update would drop such
	// items from the text.
	CodeAcceptance Code = "acceptance"
	// CodeForbidden: the caller may not make this change: another
	// principal holds the issue, or the change is for admins.
	CodeForbidden Code = "forbidden"
	// CodeUnavailable: the server, or the daemon behind it, cannot serve
	// the request now.
	CodeUnavailable Code = "unavailable"
	// CodeVersion: the client's protocol version is outside the server's
	// range.
	CodeVersion Code = "version"
	// CodeAuth: SSH authentication failed, or the server's host key does
	// not match the pinned one.
	CodeAuth Code = "auth"
	// CodeBusy: the caller reached one of the server's limits on
	// connections or writes; the fix says how long to wait (protocol 2).
	CodeBusy Code = "busy"
)

// Error is a typed, one-line error. Message names the cause; Fix names the
// next action (design §12 item 5).
type Error struct {
	Code    Code   `json:"c"`
	Message string `json:"m"`
	Fix     string `json:"fix,omitempty"`
}

// Error returns Message, followed by "; " and Fix when there is one.
func (e *Error) Error() string {
	if e.Fix == "" {
		return e.Message
	}
	return e.Message + "; " + e.Fix
}

// Errf returns an Error with code, fix and msg. Despite its name it does
// not format: callers build msg, and fix, with [fmt.Sprintf].
func Errf(code Code, fix, msg string) *Error {
	return &Error{Code: code, Message: msg, Fix: fix}
}

// Fix phrases. sfx mcp tells some refusals apart by their code and the
// phrase their fix opens with, and turns each into an agent's next step
// (internal/mcpserver). The server writes those fixes with these phrases,
// so the two cannot drift apart. They are wire text that released clients
// match too, so keep each as it is. FixNothing is a whole fix, and
// FixEditText the end of one.
const (
	// FixSeeBlocked: not_found, nothing is ready to start.
	FixSeeBlocked = "see what holds work back"
	// FixTakeNext: conflict, another principal holds the issue; the next
	// ready one is named.
	FixTakeNext = "take that one"
	// FixTakeOver: conflict, another session of the caller's principal
	// holds the issue.
	FixTakeOver = "take it over"
	// FixLeaveIt: conflict, another principal holds the issue, and nothing
	// else is ready.
	FixLeaveIt = "leave it to"
	// FixReread: conflict, the issue changed since the caller's rev.
	FixReread = "re-read"
	// FixReopen: invalid, the issue is closed.
	FixReopen = "reopen"
	// FixStart: invalid, update cannot set in_progress; start does.
	FixStart = "take it with"
	// FixRelease: invalid, a status or assignee change to a claimed issue.
	FixRelease = "finish it, or let it go"
	// FixNothing: invalid, the whole fix when the issue is already as
	// asked.
	FixNothing = "nothing to do"
	// FixUpgrade: invalid, the request does not fit this server's
	// protocol, such as an unknown operation or argument.
	FixUpgrade = "upgrade"
	// FixEditText: acceptance, the end of the fix when an edit drops
	// acceptance items still open.
	FixEditText = "then edit the text"
	// FixAsk: forbidden, another principal holds the issue, whom the fix
	// asks.
	FixAsk = "ask "
)
