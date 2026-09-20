package sqlitestore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The D1 migrations (cloudflare/d1/migrations) must build the same tables as
// Open, so the Worker and the local crawler share one schema.
func TestD1MigrationsMatchSchema(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "..", "cloudflare", "d1", "migrations")
	files, _ := filepath.Glob(filepath.Join(dir, "*.sql"))
	if len(files) == 0 {
		t.Skip("no D1 migrations next to this checkout")
	}

	d1, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d1.Close()
	d1.SetMaxOpenConns(1)
	for _, f := range files { // Glob sorts, so 0001 runs before 0002
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d1.Exec(string(b)); err != nil {
			t.Fatalf("%s: %v", filepath.Base(f), err)
		}
	}

	local, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()

	for _, table := range []string{"listings", "price_history", "meta", "crawls"} {
		want, got := describe(t, local.SQL(), table), describe(t, d1, table)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("%s differs\n go: %v\n d1: %v", table, want, got)
		}
	}
	// And the snapshot and alert tables exist only on the D1 side.
	for _, table := range []string{"deal_snapshots", "deal_snapshot_items", "new_deal_alerts"} {
		if len(describe(t, d1, table)) == 0 {
			t.Errorf("D1 migrations do not create %s", table)
		}
	}
}

// describe lists columns (name, type, not null, default, pk) and indexes.
func describe(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	var out []string
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d %s %s nn=%d d=%v pk=%d", cid, name, typ, notnull, dflt.String, pk))
	}
	rows.Close()
	idx, err := db.Query("SELECT name, sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL ORDER BY name", table)
	if err != nil {
		t.Fatal(err)
	}
	for idx.Next() {
		var name, stmt string
		idx.Scan(&name, &stmt)
		out = append(out, "index "+name)
	}
	idx.Close()
	return out
}
