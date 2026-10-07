-- Comment kinds: '' is a plain comment, 'handoff' a handoff note that start
-- returns. Existing comments become plain ones.

ALTER TABLE comments ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT '';
