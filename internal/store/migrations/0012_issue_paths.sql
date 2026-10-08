-- Files to issues (design §12 item 3): the paths an issue's work touched,
-- as clients found them in git (source 'commit'), and those a person or
-- agent declared (source 'declared'; a trailing '/' makes a directory
-- prefix). source is part of the key, so a path can be both, and a later
-- kind (such as predictions from similar issues) is a new source value,
-- not a new table. principal and session are who last wrote the row;
-- first_at and last_at the server's times. show orders commit paths by
-- last_at (issue_paths_recent); ready finds an issue's paths by the key's
-- issue_id prefix. Paths compare byte for byte, as git's do.

CREATE TABLE issue_paths (
  issue_id  VARCHAR(64)   NOT NULL,
  path      VARCHAR(1024) COLLATE utf8mb4_0900_bin NOT NULL,
  source    VARCHAR(16)   NOT NULL,
  principal VARCHAR(255)  NOT NULL,
  session   VARCHAR(255)  NOT NULL,
  first_at  DATETIME(6)   NOT NULL,
  last_at   DATETIME(6)   NOT NULL,
  rev       BIGINT        NOT NULL,
  write_id  BIGINT        NOT NULL,
  PRIMARY KEY (issue_id, path, source),
  KEY issue_paths_recent (issue_id, source, last_at)
);
