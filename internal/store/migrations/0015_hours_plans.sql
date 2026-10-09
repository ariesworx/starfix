-- Human hours (design §12.1, sfx log): a person's time on an issue, one
-- row per entry. principal is who logged it, for themselves; on_date is
-- the day worked (UTC) and seconds how long, at most a day. The account
-- comes from the issue when read, as for tokens, so moving an issue to
-- another account moves its hours too. Hours carry no money. Entries are
-- only inserted and deleted, never updated; each is an event on its
-- issue (hours.log, hours.delete), which keeps a deleted entry's history.
-- Reads go by issue (hours_issue), by person (hours_principal) and by day
-- (hours_on).

CREATE TABLE hours (
  id        VARCHAR(32)  NOT NULL,
  issue_id  VARCHAR(64)  NOT NULL,
  principal VARCHAR(255) NOT NULL,
  on_date   DATE         NOT NULL,
  seconds   INT          NOT NULL,
  note      TEXT         NOT NULL,
  logged_at DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  KEY hours_issue (issue_id, on_date),
  KEY hours_principal (principal, on_date),
  KEY hours_on (on_date)
);
