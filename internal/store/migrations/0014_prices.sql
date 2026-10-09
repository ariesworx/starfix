-- Prices (design §12.1): a model's list rates from an effective time on,
-- one row per (model, effective_at). Rates are integer micro-dollars
-- (10^-6 USD) per million tokens, so 3 USD per million is 3000000 and a
-- cost is an exact integer sum, never a float. cache_write is the rate of
-- a five-minute cache write and cache_write_1h of a one-hour one. Costs
-- are computed when read, never stored, so a new price never rewrites
-- history: a usage record is priced by the newest row for its model whose
-- effective_at is no later than the record's time. Models compare byte
-- for byte, as harnesses report them. set_by and set_at say who last set
-- the row; write_id makes a replace a write Dolt sees as a conflict.

CREATE TABLE prices (
  model          VARCHAR(128) COLLATE utf8mb4_0900_bin NOT NULL,
  effective_at   DATETIME(6)  NOT NULL,
  input          BIGINT       NOT NULL,
  output         BIGINT       NOT NULL,
  cache_write    BIGINT       NOT NULL,
  cache_write_1h BIGINT       NOT NULL,
  cache_read     BIGINT       NOT NULL,
  set_by         VARCHAR(255) NOT NULL,
  set_at         DATETIME(6)  NOT NULL,
  write_id       BIGINT       NOT NULL,
  PRIMARY KEY (model, effective_at)
);
