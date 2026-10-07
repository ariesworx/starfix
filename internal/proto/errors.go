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
	// items are neither ticked nor waived.
	CodeAcceptance Code = "acceptance"
	// CodeUnavailable: the server, or the daemon behind it, cannot serve
	// the request now.
	CodeUnavailable Code = "unavailable"
	// CodeVersion: the client's protocol version is outside the server's
	// range.
	CodeVersion Code = "version"
	// CodeAuth: SSH authentication failed, or the server's host key does
	// not match the pinned one.
	CodeAuth Code = "auth"
)

// Error is a typed, one-line error. Message names the cause; Fix names the
// next action (design §12 item 5).
type Error struct {
	Code    Code   `json:"c"`
	Message string `json:"m"`
	Fix     string `json:"fix,omitempty"`
}

func (e *Error) Error() string {
	if e.Fix == "" {
		return e.Message
	}
	return e.Message + "; " + e.Fix
}

// Errf returns an Error.
func Errf(code Code, fix, msg string) *Error {
	return &Error{Code: code, Message: msg, Fix: fix}
}
