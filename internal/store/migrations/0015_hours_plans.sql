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

-- Subscription plans (design §12.1): a flat-rate plan's fee per seat per
-- month, its seats, and the principals whose usage it pays for, from a
-- month on until a later row of the same name supersedes it; a row with
-- no fee or no seats ends the plan. fee is integer micro-dollars (10^-6
-- USD), as prices' rates are, so the amortized cost a report splits from
-- fee x seats is exact. Costs are computed when read, never stored.
-- set_by and set_at say who last set the row; write_id makes a replace a
-- write Dolt sees as a conflict. A row's principals are in plan_principals,
-- replaced with it.

CREATE TABLE plans (
  name       VARCHAR(64)  COLLATE utf8mb4_0900_bin NOT NULL,
  from_month DATE         NOT NULL,
  fee        BIGINT       NOT NULL,
  seats      INT          NOT NULL,
  set_by     VARCHAR(255) NOT NULL,
  set_at     DATETIME(6)  NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (name, from_month)
);

CREATE TABLE plan_principals (
  name       VARCHAR(64)  COLLATE utf8mb4_0900_bin NOT NULL,
  from_month DATE         NOT NULL,
  principal  VARCHAR(255) NOT NULL,
  PRIMARY KEY (name, from_month, principal)
);
