-- Issues, dependencies, labels, comments, events and settings.
-- Mutable rows carry rev (incremented on every write) and write_id (a value
-- unique to each write), which together make Dolt detect concurrent writes.
-- Statements end with a semicolon at the end of a line.

CREATE TABLE issues (
  id           VARCHAR(64)   NOT NULL,
  parent_id    VARCHAR(64)   NULL,
  title        VARCHAR(500)  NOT NULL,
  body         TEXT          NOT NULL,
  design       TEXT          NOT NULL,
  acceptance   TEXT          NOT NULL,
  notes        TEXT          NOT NULL,
  status       VARCHAR(32)   NOT NULL,
  priority     TINYINT       NOT NULL,
  type         VARCHAR(32)   NOT NULL,
  assignee     VARCHAR(255)  NULL,
  owner        VARCHAR(255)  NULL,
  due_at       DATETIME(6)   NULL,
  defer_until  DATETIME(6)   NULL,
  ephemeral    BOOLEAN       NOT NULL DEFAULT FALSE,
  expires_at   DATETIME(6)   NULL,
  pinned       BOOLEAN       NOT NULL DEFAULT FALSE,
  template     BOOLEAN       NOT NULL DEFAULT FALSE,
  metadata     JSON          NULL,
  close_reason VARCHAR(2000) NULL,
  created_by   VARCHAR(255)  NOT NULL,
  created_at   DATETIME(6)   NOT NULL,
  updated_at   DATETIME(6)   NOT NULL,
  closed_at    DATETIME(6)   NULL,
  rev          BIGINT        NOT NULL,
  write_id     BIGINT        NOT NULL,
  PRIMARY KEY (id),
  KEY issues_parent (parent_id),
  KEY issues_ready (status, priority, created_at),
  KEY issues_created (created_at, id)
);

CREATE TABLE deps (
  from_id    VARCHAR(64)  NOT NULL,
  to_id      VARCHAR(64)  NOT NULL,
  type       VARCHAR(32)  NOT NULL,
  metadata   JSON         NULL,
  created_by VARCHAR(255) NOT NULL,
  created_at DATETIME(6)  NOT NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (from_id, to_id, type),
  KEY deps_to (to_id)
);

CREATE TABLE labels (
  issue_id   VARCHAR(64) NOT NULL,
  label      VARCHAR(64) NOT NULL,
  created_at DATETIME(6) NOT NULL,
  PRIMARY KEY (issue_id, label),
  KEY labels_label (label)
);

CREATE TABLE comments (
  id         VARCHAR(32)  NOT NULL,
  issue_id   VARCHAR(64)  NOT NULL,
  author     VARCHAR(255) NOT NULL,
  session    VARCHAR(255) NOT NULL,
  body       TEXT         NOT NULL,
  created_at DATETIME(6)  NOT NULL,
  PRIMARY KEY (id),
  KEY comments_issue (issue_id, created_at)
);

CREATE TABLE events (
  seq          BIGINT       NOT NULL,
  at           DATETIME(6)  NOT NULL,
  principal    VARCHAR(255) NOT NULL,
  session      VARCHAR(255) NOT NULL,
  machine      VARCHAR(255) NOT NULL,
  op           VARCHAR(64)  NOT NULL,
  target       VARCHAR(64)  NOT NULL,
  before_state JSON         NULL,
  after_state  JSON         NULL,
  idem_key     VARCHAR(128) NULL,
  PRIMARY KEY (seq),
  UNIQUE KEY events_idem (idem_key),
  KEY events_target (target, seq)
);

CREATE TABLE settings (
  name       VARCHAR(128) NOT NULL,
  value      JSON         NOT NULL,
  source     VARCHAR(32)  NOT NULL,
  updated_at DATETIME(6)  NOT NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (name)
);
