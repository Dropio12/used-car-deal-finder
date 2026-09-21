// Package sqlitestore is the SQLite implementation of store.Store, using the
// pure-Go modernc.org/sqlite driver (no cgo). Port of src/db.js.
//
// The schema is created with the same statements, in the same order, as the
// JS openDb(): base tables, then the column migrations, then the indexes. A
// database made by either program has identical tables and columns.
package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"carbuyer/crawler/internal/listing"
	"carbuyer/crawler/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS listings (
  id             TEXT PRIMARY KEY,
  source         TEXT NOT NULL,
  url            TEXT,
  reference_id   TEXT,

  -- the query slugs this listing was found under, so a crawl can tell which
  -- rows it is responsible for when detecting removals
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
  removed_at     TEXT
);

-- One row per observed price. Written only when the price actually changes,
-- so this table stays small and every row is a real event.
CREATE TABLE IF NOT EXISTS price_history (
  listing_id  TEXT NOT NULL,
  seen_at     TEXT NOT NULL,
  price       INTEGER NOT NULL,
  km          INTEGER,
  PRIMARY KEY (listing_id, seen_at)
);

-- Small facts about the crawler itself, which have to outlive the process.
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
`

// Indexes are created after migrations, so an index on a migrated column works
// against an old database.
const indexes = `
CREATE INDEX IF NOT EXISTS idx_listings_model  ON listings(make, model, year);
CREATE INDEX IF NOT EXISTS idx_listings_scope  ON listings(make_slug, model_slug);
CREATE INDEX IF NOT EXISTS idx_listings_active ON listings(removed_at, last_seen);
CREATE INDEX IF NOT EXISTS idx_listings_seller ON listings(seller_type);
`

// Columns added after the first databases were created, in the JS order.
var migrations = [][3]string{
	{"listings", "is_conditional_price", "INTEGER"},
	{"listings", "listed_at", "TEXT"},
	{"listings", "price_rating", "TEXT"},
	{"listings", "vin", "TEXT"},
	{"listings", "carfax_url", "TEXT"},
	{"listings", "latitude", "REAL"},
	{"listings", "longitude", "REAL"},
	{"listings", "image_urls", "TEXT"},
	{"listings", "image_notes", "TEXT"},
	{"listings", "inspected_at", "TEXT"},
	{"listings", "is_parts", "INTEGER"},
	{"listings", "is_sold", "INTEGER"},
	{"listings", "sold_at", "TEXT"},
	{"listings", "page_read_at", "TEXT"},
	{"listings", "geo_slug", "TEXT"},
}

var columns = []string{
	"id", "source", "url", "reference_id", "make_slug", "model_slug", "geo_slug",
	"make", "model", "model_detail", "trim_text", "year", "km", "price",
	"first_price", "transmission", "fuel", "engine_ccm", "is_damaged", "is_parts",
	"is_conditional_price", "is_sold", "sold_at", "page_read_at",
	"condition", "seller_type", "seller_id", "seller_name", "city",
	"postal_code", "province", "description", "image_count",
	"listed_at", "price_rating", "vin", "carfax_url", "latitude", "longitude",
	"image_urls",
	"first_seen", "last_seen",
}

// mutable are the fields a re-crawl may overwrite (the seller edited the ad,
// or the parser got better).
var mutable = []string{
	"url", "price", "km", "trim_text", "description", "image_count",
	"seller_name", "city", "postal_code", "province",
	"make", "model", "model_detail", "year",
	"listed_at", "price_rating", "vin", "carfax_url", "latitude", "longitude",
	"image_urls",
	"geo_slug",
	"is_sold", "sold_at",
	"page_read_at",
}

// sticky columns may be turned on but never off; keepIfAbsent ones may be
// updated but not blanked.
var sticky = map[string]bool{"is_sold": true, "sold_at": true}
var keepIfAbsent = map[string]bool{"page_read_at": true}

// DB is a SQLite-backed store.Store.
type DB struct {
	db  *sql.DB
	now func() time.Time
}

var _ store.Store = (*DB)(nil)

// Open opens (or creates) the database at path and brings the schema up to date.
func Open(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; also keeps ":memory:" a single database
	for _, stmt := range []string{"PRAGMA journal_mode = WAL", "PRAGMA foreign_keys = ON", schema} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("init schema: %w", err)
		}
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(indexes); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db, now: time.Now}, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// SQL exposes the handle for read-only inspection (tests, the CLI summary).
func (d *DB) SQL() *sql.DB { return d.db }

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", m[0]))
		if err != nil {
			return err
		}
		exists := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, typ string
			var dflt any
			if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
				rows.Close()
				return err
			}
			if name == m[1] {
				exists = true
			}
		}
		rows.Close()
		if !exists {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", m[0], m[1], m[2])); err != nil {
				return err
			}
		}
	}
	return nil
}

// sqlNum binds a number the way SQLite would store it from JS: whole numbers
// as INTEGER, anything else as REAL.
func sqlNum(f *float64) any {
	if f == nil {
		return nil
	}
	if *f == math.Trunc(*f) && math.Abs(*f) < 1<<53 {
		return int64(*f)
	}
	return *f
}

func sqlStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func toRow(l listing.Listing, scope store.Scope) map[string]any {
	var imageURLs any
	if len(l.ImageURLs) > 0 {
		b, _ := json.Marshal(l.ImageURLs)
		imageURLs = string(b)
	}
	source := l.Source
	if source == "" {
		source = "autohebdo"
	}
	return map[string]any{
		"id": sqlStr(l.ID), "source": source, "url": sqlStr(l.URL), "reference_id": sqlStr(l.ReferenceID),
		"make_slug": sqlStr(scope.MakeSlug), "model_slug": sqlStr(scope.ModelSlug), "geo_slug": sqlStr(scope.GeoSlug),
		"make": sqlStr(l.Make), "model": sqlStr(l.Model), "model_detail": sqlStr(l.ModelDetail), "trim_text": sqlStr(l.TrimText),
		"year": sqlNum(l.Year), "km": sqlNum(l.Km), "price": sqlNum(l.Price), "first_price": sqlNum(l.Price),
		"transmission": sqlStr(l.Transmission), "fuel": sqlStr(l.Fuel), "engine_ccm": sqlNum(l.EngineCcm),
		"is_damaged": boolInt(l.IsDamaged), "is_parts": boolInt(l.IsParts),
		"is_conditional_price": boolInt(l.IsConditionalPrice),
		// AutoHebdo publishes no sold flag, page-read time, dates, VIN or coordinates.
		"is_sold": int64(0), "sold_at": nil, "page_read_at": nil,
		"condition": sqlStr(l.Condition), "seller_type": sqlStr(l.SellerType), "seller_id": sqlStr(l.SellerID),
		"seller_name": sqlStr(l.SellerName), "city": sqlStr(l.City), "postal_code": sqlStr(l.PostalCode),
		"province": sqlStr(l.Province), "description": sqlStr(l.Description), "image_count": int64(l.ImageCount),
		"listed_at": nil, "price_rating": nil, "vin": nil, "carfax_url": nil, "latitude": nil, "longitude": nil,
		"image_urls": imageURLs,
		"first_seen": scope.SeenAt, "last_seen": scope.SeenAt,
	}
}

var (
	insertSQL = fmt.Sprintf("INSERT INTO listings (%s) VALUES (%s)",
		strings.Join(columns, ", "), "$"+strings.Join(columns, ", $"))
	updateSQL = func() string {
		sets := make([]string, len(mutable))
		for i, c := range mutable {
			switch {
			case sticky[c]:
				sets[i] = fmt.Sprintf("%s = COALESCE(NULLIF(%s, 0), $%s)", c, c, c)
			case keepIfAbsent[c]:
				sets[i] = fmt.Sprintf("%s = COALESCE($%s, %s)", c, c, c)
			default:
				sets[i] = fmt.Sprintf("%s = $%s", c, c)
			}
		}
		return "UPDATE listings SET " + strings.Join(sets, ", ") +
			", last_seen = $last_seen, price_changes = $price_changes, removed_at = NULL WHERE id = $id"
	}()
)

func named(m map[string]any, keys []string) []any {
	args := make([]any, 0, len(keys))
	for _, k := range keys {
		args = append(args, sql.Named(k, m[k]))
	}
	return args
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// UpsertResult says what one write did.
type UpsertResult struct {
	Added      bool
	PriceDelta *float64
	Relisted   bool
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// upsert inserts or updates one listing (JS createWriter()).
func upsert(ctx context.Context, x execer, l listing.Listing, scope store.Scope) (UpsertResult, error) {
	row := toRow(l, scope)
	var existingPrice any
	var priceChanges int
	var removedAt sql.NullString
	err := x.QueryRowContext(ctx, "SELECT price, price_changes, removed_at FROM listings WHERE id = ?", row["id"]).
		Scan(&existingPrice, &priceChanges, &removedAt)
	if err == sql.ErrNoRows {
		if _, err := x.ExecContext(ctx, insertSQL, named(row, columns)...); err != nil {
			return UpsertResult{}, err
		}
		if row["price"] != nil {
			if err := insertPrice(ctx, x, row, scope.SeenAt); err != nil {
				return UpsertResult{}, err
			}
		}
		return UpsertResult{Added: true}, nil
	}
	if err != nil {
		return UpsertResult{}, err
	}

	newPrice, hasPrice := asFloat(row["price"])
	oldPrice, hadPrice := asFloat(existingPrice)
	changed := hasPrice && (!hadPrice || newPrice != oldPrice)
	args := named(row, mutable)
	next := priceChanges
	if changed {
		next++
	}
	args = append(args, sql.Named("id", row["id"]), sql.Named("last_seen", scope.SeenAt), sql.Named("price_changes", next))
	if _, err := x.ExecContext(ctx, updateSQL, args...); err != nil {
		return UpsertResult{}, err
	}
	res := UpsertResult{Relisted: removedAt.Valid}
	if changed {
		if err := insertPrice(ctx, x, row, scope.SeenAt); err != nil {
			return UpsertResult{}, err
		}
		delta := newPrice - oldPrice // a missing old price counts as 0, as in JS
		res.PriceDelta = &delta
	}
	return res, nil
}

func insertPrice(ctx context.Context, x execer, row map[string]any, seenAt string) error {
	_, err := x.ExecContext(ctx, "INSERT OR REPLACE INTO price_history (listing_id, seen_at, price, km) VALUES (?, ?, ?, ?)",
		row["id"], seenAt, row["price"], row["km"])
	return err
}

// Upsert writes one listing outside a batch.
func (d *DB) Upsert(ctx context.Context, l listing.Listing, scope store.Scope) (UpsertResult, error) {
	return upsert(ctx, d.db, l, scope)
}

// Save implements store.Writer: one transaction per batch, as crawlScope does per shard.
func (d *DB) Save(ctx context.Context, listings []listing.Listing, scope store.Scope) (store.SaveStats, error) {
	var stats store.SaveStats
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, err
	}
	for _, l := range listings {
		r, err := upsert(ctx, tx, l, scope)
		if err != nil {
			tx.Rollback()
			return store.SaveStats{}, err
		}
		stats.Seen++
		if r.Added {
			stats.Added++
		}
		if r.Relisted {
			stats.Relisted++
		}
		if r.PriceDelta != nil {
			if *r.PriceDelta < 0 {
				stats.PriceDrops++
			} else {
				stats.PriceRises++
			}
		}
	}
	return stats, tx.Commit()
}

// MarkRemoved implements store.Remover. `IS` rather than `=` so null slugs match.
func (d *DB) MarkRemoved(ctx context.Context, s store.RemovalScope) (int, error) {
	source := s.Source
	if source == "" {
		source = "autohebdo"
	}
	clauses := []string{
		"removed_at IS NULL", "last_seen < $before", "source = $source",
		"make_slug IS $make_slug", "geo_slug IS $geo_slug", "model_slug IS $model_slug",
	}
	args := []any{
		sql.Named("at", s.At), sql.Named("before", s.Before), sql.Named("source", source),
		sql.Named("make_slug", sqlStr(s.MakeSlug)), sql.Named("geo_slug", sqlStr(s.GeoSlug)),
		sql.Named("model_slug", sqlStr(s.ModelSlug)),
	}
	if s.SellerType != "" {
		st := "Dealer"
		if s.SellerType == "P" {
			st = "PrivateSeller"
		}
		clauses = append(clauses, "seller_type = $seller_type")
		args = append(args, sql.Named("seller_type", st))
	}
	res, err := d.db.ExecContext(ctx, "UPDATE listings SET removed_at = $at WHERE "+strings.Join(clauses, " AND "), args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// StartCrawl implements store.CrawlLog.
func (d *DB) StartCrawl(ctx context.Context, scope any) (int64, error) {
	b, err := json.Marshal(scope)
	if err != nil {
		return 0, err
	}
	res, err := d.db.ExecContext(ctx, "INSERT INTO crawls (started_at, scope) VALUES (?, ?)", store.ISOTime(d.now()), string(b))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishCrawl implements store.CrawlLog.
func (d *DB) FinishCrawl(ctx context.Context, id int64, s store.CrawlStats) error {
	_, err := d.db.ExecContext(ctx, `UPDATE crawls SET
       finished_at = ?, shards = ?, seen = ?, added = ?, price_drops = ?, price_rises = ?,
       removed = ?, truncated = ?, unpriced = ?, error = ?
     WHERE id = ?`,
		store.ISOTime(d.now()), s.Shards, s.Seen, s.Added, s.PriceDrops, s.PriceRises, s.Removed, s.Truncated, s.Unpriced,
		sqlStr(s.Error), id)
	return err
}

// LoadListings implements store.Reader, in insertion (rowid) order so the
// scorer sees rows in the same order the JS loadListings() returned them.
func (d *DB) LoadListings(ctx context.Context, f store.Filter) ([]store.Stored, error) {
	var clauses []string
	var args []any
	if !f.IncludeRemoved {
		clauses = append(clauses, "removed_at IS NULL")
	}
	add := func(col, val string) {
		if val != "" {
			clauses = append(clauses, col+" = ?")
			args = append(args, val)
		}
	}
	add("source", f.Source)
	add("seller_type", f.SellerType)
	add("make", f.Make)
	add("model", f.Model)
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id, source, url, make, model, year, km, price, first_price, trim_text,
       seller_type, seller_name, city, postal_code, province, is_damaged, is_parts, is_conditional_price,
       description, image_urls, image_count, price_changes, first_seen, last_seen
     FROM listings `+where+` ORDER BY rowid`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []store.Stored{}
	for rows.Next() {
		var (
			id, source                                                   string
			url, mk, model, trim, seller, sellerName, city, postal, prov sql.NullString
			desc, imageURLs                                              sql.NullString
			year, km, price, firstPrice                                  sql.NullFloat64
			damaged, parts, conditional, imageCount                      sql.NullInt64
			s                                                            store.Stored
		)
		if err := rows.Scan(&id, &source, &url, &mk, &model, &year, &km, &price, &firstPrice, &trim,
			&seller, &sellerName, &city, &postal, &prov, &damaged, &parts, &conditional,
			&desc, &imageURLs, &imageCount, &s.PriceChanges, &s.FirstSeen, &s.LastSeen); err != nil {
			return nil, err
		}
		str := func(n sql.NullString) *string {
			if !n.Valid {
				return nil
			}
			v := n.String
			return &v
		}
		num := func(n sql.NullFloat64) *float64 {
			if !n.Valid {
				return nil
			}
			v := n.Float64
			return &v
		}
		s.Listing = listing.Listing{
			ID: &id, Source: source, URL: str(url), Make: str(mk), Model: str(model),
			Year: num(year), Km: num(km), Price: num(price), TrimText: str(trim),
			SellerType: str(seller), SellerName: str(sellerName), City: str(city), PostalCode: str(postal),
			Province: str(prov), Description: str(desc),
			IsDamaged: damaged.Int64 == 1, IsParts: parts.Int64 == 1, IsConditionalPrice: conditional.Int64 == 1,
			ImageCount: int(imageCount.Int64), ImageURLs: []string{},
		}
		if imageURLs.Valid {
			_ = json.Unmarshal([]byte(imageURLs.String), &s.ImageURLs)
		}
		s.FirstPrice = num(firstPrice)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Counts is a quick summary of the database (JS stats()).
type Counts struct {
	Listings, Removed, PriceEvents, Private, Crawls int
}

// Stats counts listings, removals, price events, private listings and finished crawls.
func (d *DB) Stats(ctx context.Context) (Counts, error) {
	var c Counts
	err := d.db.QueryRowContext(ctx, `SELECT
      (SELECT COUNT(*) FROM listings), (SELECT COUNT(removed_at) FROM listings),
      (SELECT COUNT(*) FROM price_history),
      (SELECT COUNT(*) FROM listings WHERE seller_type = 'PrivateSeller'),
      (SELECT COUNT(*) FROM crawls WHERE finished_at IS NOT NULL)`).
		Scan(&c.Listings, &c.Removed, &c.PriceEvents, &c.Private, &c.Crawls)
	return c, err
}

// VocabModel is one make/model pair as stored.
type VocabModel struct{ Make, Model string }

// Vocabulary lists the makes and make/model pairs stored by the given sources
// (JS vehicleVocabulary, without canonical names). Sources that only have a
// free-text title (LesPAC, Craigslist, Marketplace) read make and model out
// of it by matching against this list.
func (d *DB) Vocabulary(ctx context.Context, sources ...string) (makes []string, models []VocabModel, err error) {
	if len(sources) == 0 {
		sources = []string{"autohebdo", "kijiji"}
	}
	in := strings.TrimSuffix(strings.Repeat("?, ", len(sources)), ", ")
	args := make([]any, len(sources))
	for i, s := range sources {
		args[i] = s
	}
	rows, err := d.db.QueryContext(ctx, `SELECT DISTINCT make FROM listings
      WHERE make IS NOT NULL AND source IN (`+in+`)`, args...)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			rows.Close()
			return nil, nil, err
		}
		makes = append(makes, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	rows, err = d.db.QueryContext(ctx, `SELECT DISTINCT make, model FROM listings
      WHERE make IS NOT NULL AND model IS NOT NULL AND source IN (`+in+`)`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m VocabModel
		if err := rows.Scan(&m.Make, &m.Model); err != nil {
			return nil, nil, err
		}
		models = append(models, m)
	}
	return makes, models, rows.Err()
}
