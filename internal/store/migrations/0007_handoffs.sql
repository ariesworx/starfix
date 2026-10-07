-- Handoffs: the structured fields of a handoff note. The note itself stays
-- a comment of kind 'handoff', so comments, history and a bd export are
-- unchanged; this row, keyed by that comment, holds what the next person
-- needs in fields: the state of the work, the next step, the branch and
-- worktree it is on, and whom it is handed to. A note with none of them
-- has no row. Empty strings mean "not given".

CREATE TABLE handoffs (
  comment_id   VARCHAR(32)   NOT NULL,
  issue_id     VARCHAR(64)   NOT NULL,
  state        VARCHAR(16)   NOT NULL,
  next_step    VARCHAR(500)  NOT NULL,
  branch       VARCHAR(255)  NOT NULL,
  worktree     VARCHAR(1024) NOT NULL,
  to_principal VARCHAR(255)  NOT NULL,
  PRIMARY KEY (comment_id),
  KEY handoffs_issue (issue_id)
);
