package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Idempotency (design §7, §8): a create-type operation (create, finish,
// comment, handoff) may carry a client key. The operation's last event
// records the key, a hash of the request and the result. The same
// principal sending the same key again within IdemTTL gets that result
// back and nothing is written; the same key with a different request, or
// after IdemTTL, is refused with an *IdemError. Keys are never deleted:
// the unique index on (principal, idem_key) keeps an expired key from
// being reused.

// IdemTTL is how long a key replays its request.
const IdemTTL = 24 * time.Hour

// IdemPattern is what an idempotency key may look like: 1-64 letters,
// digits and ._:-, starting with a letter or digit.
var IdemPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// idemStamp is an operation's key, request hash and, once the closure
// sets it, its result; flush writes them on the operation's last event.
type idemStamp struct {
	key, args string
	result    json.RawMessage
}

func validIdem(key string) error {
	if key != "" && !IdemPattern.MatchString(key) {
		return fmt.Errorf("%w: idempotency key must be 1-64 letters, digits and ._:-, starting with a letter or digit", ErrInvalid)
	}
	return nil
}

// idemHash identifies a request: the operation and its arguments.
func idemHash(op string, args any) (string, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("hash request: %w", err)
	}
	h := sha256.New()
	h.Write([]byte(op))
	h.Write([]byte{0})
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// replay looks key up for w's principal. When the key's request was
// already applied it fills out with that request's result and reports
// true. Otherwise it arms w to stamp this operation with key and reports
// false. An empty key does nothing. op and args identify the request.
func (w *wtx) replay(ctx context.Context, key, op string, args, out any) (bool, error) {
	w.idem = nil
	if key == "" {
		return false, nil
	}
	hash, err := idemHash(op, args)
	if err != nil {
		return false, err
	}
	var at time.Time
	var prev sql.NullString
	var result []byte
	err = w.tx.QueryRowContext(ctx, `SELECT at, idem_args, idem_result FROM events WHERE principal = ? AND idem_key = ?`,
		w.actor.Principal, key).Scan(&at, &prev, &result)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		w.idem = &idemStamp{key: key, args: hash}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("idempotency lookup: %w", err)
	case w.now.Sub(at) > IdemTTL:
		return false, &IdemError{Key: key, Expired: true}
	case prev.String != hash || len(result) == 0:
		return false, &IdemError{Key: key}
	}
	if err := json.Unmarshal(result, out); err != nil {
		return false, fmt.Errorf("idempotency result: %w", err)
	}
	return true, nil
}

// settle records the operation's result for its idempotency stamp. It
// does nothing for an operation without a key.
func (w *wtx) settle(result any) error {
	if w.idem == nil {
		return nil
	}
	b, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode idempotency result: %w", err)
	}
	w.idem.result = b
	return nil
}
