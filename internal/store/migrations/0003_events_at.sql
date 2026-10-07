-- Digest reads the events in a time window (closes, starts, handoffs,
-- creates and discovered-from links since a given time), so it ranges
-- over at rather than scanning the log.

CREATE INDEX events_at ON events (at);
