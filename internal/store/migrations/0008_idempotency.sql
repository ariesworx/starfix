-- Idempotency: a client key on the last event of a create-type operation
-- (create, finish, comment, handoff), so a retried request returns the
-- first one's result instead of writing again. A key belongs to the
-- principal that sent it, so the unique index moves from the key alone to
-- (principal, idem_key); NULL keys never collide. idem_args is a SHA-256
-- of the request, to refuse a key reused for a different one; idem_result
-- is the result to replay.

DROP INDEX events_idem ON events;
CREATE UNIQUE INDEX events_idem ON events (principal, idem_key);
ALTER TABLE events ADD COLUMN idem_args CHAR(64) NULL;
ALTER TABLE events ADD COLUMN idem_result JSON NULL;
