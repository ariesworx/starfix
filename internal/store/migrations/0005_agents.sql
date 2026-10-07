-- Agents: the live registry behind `who`. One row per (principal,
-- session), touched when a session connects and while it renews; who
-- lists the rows seen recently. harness is empty when the client did not
-- say which agent it runs under.

CREATE TABLE agents (
  principal VARCHAR(255) NOT NULL,
  session   VARCHAR(255) NOT NULL,
  machine   VARCHAR(255) NOT NULL,
  harness   VARCHAR(32)  NOT NULL,
  started   DATETIME(6)  NOT NULL,
  last_seen DATETIME(6)  NOT NULL,
  rev       BIGINT       NOT NULL,
  write_id  BIGINT       NOT NULL,
  PRIMARY KEY (principal, session),
  KEY agents_last_seen (last_seen)
);
