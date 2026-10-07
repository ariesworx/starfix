-- Claims: who holds an issue, until when, and the fencing epoch. One row
-- per issue that was ever taken; a released or expired claim keeps its row
-- with no holder, so the epoch only ever increases.

CREATE TABLE claims (
  issue_id   VARCHAR(64)  NOT NULL,
  principal  VARCHAR(255) NULL,
  session    VARCHAR(255) NULL,
  machine    VARCHAR(255) NULL,
  epoch      BIGINT       NOT NULL,
  claimed_at DATETIME(6)  NULL,
  expires_at DATETIME(6)  NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (issue_id),
  KEY claims_expires (expires_at),
  KEY claims_holder (principal, session)
);
