-- Acceptance checklist: an issue's items are parsed from its acceptance
-- text (a Markdown list, or the whole text as one item), so they never go
-- stale when the text is edited or imported. This table holds only what
-- people did to them: one row per issue and item text (item_key is the
-- SHA-256 of the item's text), so a tick follows its item when the list
-- is reordered. state is 'ticked', 'waived' (with a reason) or 'open' (an
-- item ticked in the text, unticked since). n is the item's position when
-- the row was written, for reading the table by hand.

CREATE TABLE acceptance_state (
  issue_id     VARCHAR(64)  NOT NULL,
  item_key     CHAR(64)     NOT NULL,
  n            INT          NOT NULL,
  state        VARCHAR(16)  NOT NULL,
  reason       VARCHAR(500) NOT NULL,
  by_principal VARCHAR(255) NOT NULL,
  at           DATETIME(6)  NOT NULL,
  write_id     BIGINT       NOT NULL,
  PRIMARY KEY (issue_id, item_key)
);
