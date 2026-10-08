-- Token usage (design §12.1): what harnesses report, one row per harness
-- request, keyed by the reporting principal and session and the harness's
-- own request id, so a batch sent again adds nothing. ("usage" is a
-- reserved word in SQL, hence token_usage.) Sessions are chosen by clients
-- and the CLI's is shared ("cli"), so the principal is part of the key.
--
-- at is the request's time from the source. A turn or session record
-- covers span_start to at; a request record has no span. A count is NULL
-- when the harness did not report it, which is not 0; cache_write_1h is
-- the part of cache_write written with a one-hour lifetime. added_at is
-- the server's time, which the daily cap (limits: usage_per_day) counts.
-- Which issue a row belongs to is computed when read, from the claim
-- events, and never stored.

CREATE TABLE token_usage (
  principal      VARCHAR(255) NOT NULL,
  session        VARCHAR(255) NOT NULL,
  request_id     VARCHAR(255) NOT NULL,
  machine        VARCHAR(255) NOT NULL,
  harness        VARCHAR(32)  NOT NULL,
  model          VARCHAR(128) NOT NULL,
  at             DATETIME(6)  NOT NULL,
  granularity    VARCHAR(16)  NOT NULL,
  span_start     DATETIME(6)  NULL,
  input          BIGINT       NULL,
  output         BIGINT       NULL,
  cache_write    BIGINT       NULL,
  cache_write_1h BIGINT       NULL,
  cache_read     BIGINT       NULL,
  added_at       DATETIME(6)  NOT NULL,
  PRIMARY KEY (principal, session, request_id),
  KEY token_usage_session (principal, session, at),
  KEY token_usage_at (at),
  KEY token_usage_added (principal, added_at)
);

-- An issue's account: a client's engagement code name or an internal
-- department. NULL inherits from the parent chain, then the server's
-- account: setting.

ALTER TABLE issues ADD COLUMN account VARCHAR(64) NULL;

-- Attribution finds the issues a session took from its claim.take events.

CREATE INDEX events_actor_op ON events (op, principal, session);
