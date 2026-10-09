package proto

import "time"

// Memory operations (protocol 4, design §6). A memory is a scoped record
// that agents and people keep across sessions: a key, a body, tags, an
// optional issue link and a pin. Scope is team, project or user; user
// scope is the caller's alone. An empty scope is project.
const (
	OpRemember = "remember" // RememberArgs → WriteResult
	OpRecall   = "recall"   // RecallArgs → RecallResult
	OpForget   = "forget"   // ForgetArgs → WriteResult
	OpPin      = "pin"      // PinArgs → WriteResult
)

// RememberArgs writes the memory Key in Scope. Rev 0 creates the key and
// is refused with conflict when it exists; otherwise Rev is the stored
// revision, which the write replaces, and a stale one is refused with
// conflict. Pinned nil leaves the pin as it is (a new memory unpinned).
// Idem makes a retry return the first result.
type RememberArgs struct {
	Scope  string   `json:"scope,omitempty"`
	Key    string   `json:"key"`
	Body   string   `json:"body"`
	Tags   []string `json:"tags,omitempty"`
	Issue  string   `json:"issue,omitempty"`
	Pinned *bool    `json:"pinned,omitempty"`
	Rev    int64    `json:"rev,omitempty"`
	Idem   string   `json:"idem,omitempty"`
}

// RecallArgs selects memories the caller can see; empty fields match
// everything. Text matches the key or body in any case. Prime instead
// ranks every memory for a new session, pinned first, then those relevant
// to the caller's in-progress issues, then the newest; it takes no
// filter but Limit. Limit 0 takes the server's default, 20; at most 500.
type RecallArgs struct {
	Scope string `json:"scope,omitempty"`
	Key   string `json:"key,omitempty"`
	Tag   string `json:"tag,omitempty"`
	Text  string `json:"text,omitempty"`
	Limit int    `json:"limit,omitempty"`
	Prime bool   `json:"prime,omitempty"`
}

// RecallResult lists memories, pinned first and then newest first (or
// in prime's order), and how many more matched past the limit.
type RecallResult struct {
	Memories []Memory `json:"memories"`
	More     int      `json:"more,omitempty"`
}

// ForgetArgs deletes the memory Key in Scope. Rev, when not 0, must be
// the stored revision.
type ForgetArgs struct {
	Scope string `json:"scope,omitempty"`
	Key   string `json:"key"`
	Rev   int64  `json:"rev,omitempty"`
	Idem  string `json:"idem,omitempty"`
}

// PinArgs pins or unpins the memory Key in Scope.
type PinArgs struct {
	Scope  string `json:"scope,omitempty"`
	Key    string `json:"key"`
	Pinned bool   `json:"pinned"`
}

// Memory is one memory record. Author first remembered it and UpdatedBy
// wrote it last; UpdatedAt is when its content last changed, which a pin
// leaves. Relevant marks, in prime's and start's lists, a memory linked
// to one of the caller's issues or tagged with one of their labels.
type Memory struct {
	ID        string    `json:"id"`
	Scope     string    `json:"scope"`
	Key       string    `json:"key"`
	Body      string    `json:"body"`
	Tags      []string  `json:"tags,omitempty"`
	Issue     string    `json:"issue,omitempty"`
	Pinned    bool      `json:"pinned,omitempty"`
	Author    string    `json:"author"`
	UpdatedBy string    `json:"updated_by"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Rev       int64     `json:"rev"`
	Relevant  bool      `json:"relevant,omitempty"`
}
