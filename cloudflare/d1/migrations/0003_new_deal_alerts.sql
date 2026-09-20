-- carbuyer D1 schema, part 3: snipe alerts written by the Worker when an
-- ingest brings a new (or relisted, or cheaper) listing that prices as a
-- strong deal (cloudflare/worker/src/alerts.rs). Only on the D1 side.

-- One row per (listing, asking price): a car is alerted once, and once more
-- only if its price drops again. INSERT OR IGNORE makes repeats impossible.
CREATE TABLE IF NOT EXISTS new_deal_alerts (
  listing_id    TEXT NOT NULL,
  price         INTEGER NOT NULL,  -- asking price when alerted
  created_at    TEXT NOT NULL,     -- ISO-8601 UTC
  discount_pct  REAL NOT NULL,     -- % under the local baseline
  baseline      REAL,
  comps         INTEGER,           -- comparable cars behind the baseline
  make          TEXT,
  model         TEXT,
  year          INTEGER,
  url           TEXT,
  deal          TEXT NOT NULL,     -- listing + score JSON, as /api/deals returns it
  notified_at   TEXT,              -- set when a Notifier delivered it (NULL = not sent)
  PRIMARY KEY (listing_id, price)
);

CREATE INDEX IF NOT EXISTS idx_new_deal_alerts_created ON new_deal_alerts(created_at);
