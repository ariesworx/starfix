-- Memory (design §6): scoped records that agents and people keep across
-- sessions. A key names one record per scope: scope is team, project or
-- user, and owner is the principal for user scope and '' otherwise, so
-- the same key in two scopes, or two users' user scopes, is two records.
-- A user-scope record is its owner's alone; the server, not the key,
-- enforces that. author is who first remembered it, and the per-scope
-- limit (limits: memories_per_scope) counts by author (memories_author);
-- updated_by is the last writer. rev and write_id make a replace a
-- compare-and-swap Dolt enforces, as for issues. Keys and tags compare
-- byte for byte. Recall and prime read the newest first (memories_recent)
-- and the memories linked to an issue (memories_issue).

CREATE TABLE memories (
  id         VARCHAR(32)  NOT NULL,
  scope      VARCHAR(16)  NOT NULL,
  owner      VARCHAR(255) NOT NULL,
  mem_key    VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL,
  body       TEXT         NOT NULL,
  issue_id   VARCHAR(64)  NULL,
  pinned     BOOLEAN      NOT NULL DEFAULT FALSE,
  author     VARCHAR(255) NOT NULL,
  updated_by VARCHAR(255) NOT NULL,
  created_at DATETIME(6)  NOT NULL,
  updated_at DATETIME(6)  NOT NULL,
  rev        BIGINT       NOT NULL,
  write_id   BIGINT       NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY memories_key (scope, owner, mem_key),
  KEY memories_recent (updated_at),
  KEY memories_issue (issue_id),
  KEY memories_author (author, scope)
);

-- A memory's tags, one row each, like an issue's labels; prime matches
-- them against the labels of the caller's issues.

CREATE TABLE memory_tags (
  memory_id VARCHAR(32)  NOT NULL,
  tag       VARCHAR(255) COLLATE utf8mb4_0900_bin NOT NULL,
  PRIMARY KEY (memory_id, tag),
  KEY memory_tags_tag (tag)
);
