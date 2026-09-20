-- carbuyer D1 schema, part 1: the crawler's tables.
--
-- Same tables, columns (in the same order), types and indexes as the SQLite
-- database the Go crawler creates (crawler/internal/store/sqlitestore), which
-- itself matches the JS project's src/db.js. The Go store adds the later
-- columns with ALTER TABLE; here they are written straight into CREATE TABLE,
-- after the original ones, so PRAGMA table_info is identical.
-- crawler/internal/store/sqlitestore/d1schema_test.go checks this.

CREATE TABLE IF NOT EXISTS listings (
  id             TEXT PRIMARY KEY,
  source         TEXT NOT NULL,
  url            TEXT,
  reference_id   TEXT,

  -- the query slugs this listing was found under
  make_slug      TEXT,
  model_slug     TEXT,

  make           TEXT,
  model          TEXT,
  model_detail   TEXT,
  trim_text      TEXT,
  year           INTEGER,
  km             INTEGER,
  price          INTEGER,
  first_price    INTEGER,
  transmission   TEXT,
  fuel           TEXT,
  engine_ccm     INTEGER,
  is_damaged     INTEGER,
  condition      TEXT,

  seller_type    TEXT,
  seller_id      TEXT,
  seller_name    TEXT,

  city           TEXT,
  postal_code    TEXT,
  province       TEXT,

  description    TEXT,
  image_count    INTEGER,

  first_seen     TEXT NOT NULL,
  last_seen      TEXT NOT NULL,
  price_changes  INTEGER NOT NULL DEFAULT 0,
  removed_at     TEXT,

  -- columns the JS/Go stores add by migration, in their order
  is_conditional_price INTEGER,
  listed_at      TEXT,
  price_rating   TEXT,
  vin            TEXT,
  carfax_url     TEXT,
  latitude       REAL,
  longitude      REAL,
  image_urls     TEXT,
  image_notes    TEXT,
  inspected_at   TEXT,
  is_parts       INTEGER,
  is_sold        INTEGER,
  sold_at        TEXT,
  page_read_at   TEXT,
  geo_slug       TEXT
);

-- One row per observed price, written only when the price changes.
CREATE TABLE IF NOT EXISTS price_history (
  listing_id  TEXT NOT NULL,
  seen_at     TEXT NOT NULL,
  price       INTEGER NOT NULL,
  km          INTEGER,
  PRIMARY KEY (listing_id, seen_at)
);

CREATE TABLE IF NOT EXISTS meta (
  key    TEXT PRIMARY KEY,
  value  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS crawls (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at    TEXT NOT NULL,
  finished_at   TEXT,
  scope         TEXT,
  shards        INTEGER NOT NULL DEFAULT 0,
  seen          INTEGER NOT NULL DEFAULT 0,
  added         INTEGER NOT NULL DEFAULT 0,
  price_drops   INTEGER NOT NULL DEFAULT 0,
  price_rises   INTEGER NOT NULL DEFAULT 0,
  removed       INTEGER NOT NULL DEFAULT 0,
  truncated     INTEGER NOT NULL DEFAULT 0,
  unpriced      INTEGER NOT NULL DEFAULT 0,
  error         TEXT
);

CREATE INDEX IF NOT EXISTS idx_listings_model  ON listings(make, model, year);
CREATE INDEX IF NOT EXISTS idx_listings_scope  ON listings(make_slug, model_slug);
CREATE INDEX IF NOT EXISTS idx_listings_active ON listings(removed_at, last_seen);
CREATE INDEX IF NOT EXISTS idx_listings_seller ON listings(seller_type);
