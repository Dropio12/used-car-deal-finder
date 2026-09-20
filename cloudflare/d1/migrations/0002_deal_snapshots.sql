-- carbuyer D1 schema, part 2: the daily deals snapshot written by the
-- Worker's cron trigger (cloudflare/worker/src/snapshot.rs). Not in the
-- local SQLite database: only the Worker has a scheduler.

-- One row per cron run.
CREATE TABLE IF NOT EXISTS deal_snapshots (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at  TEXT NOT NULL,     -- ISO-8601 UTC
  cron        TEXT,              -- the cron expression that fired
  min_comps   REAL,              -- scorer minComps override, NULL = default (8)
  comps       INTEGER NOT NULL,  -- active listings the baseline was built from
  scored      INTEGER NOT NULL,  -- listings that could be priced
  matched     INTEGER NOT NULL,  -- priced listings before the top-N cut
  appraiser   TEXT               -- scorer summary, JSON
);

-- The best deals of a run, best first. `deal` is the full JSON the API
-- returns (listing + score); the other columns are there for SQL queries.
CREATE TABLE IF NOT EXISTS deal_snapshot_items (
  snapshot_id   INTEGER NOT NULL REFERENCES deal_snapshots(id) ON DELETE CASCADE,
  rank          INTEGER NOT NULL,
  listing_id    TEXT NOT NULL,
  make          TEXT,
  model         TEXT,
  year          INTEGER,
  price         INTEGER,
  baseline      REAL,
  discount_pct  REAL,
  url           TEXT,
  deal          TEXT NOT NULL,
  PRIMARY KEY (snapshot_id, rank)
);

CREATE INDEX IF NOT EXISTS idx_snapshot_items_listing ON deal_snapshot_items(listing_id);
