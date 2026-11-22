package test

import (
	"context"
	"path/filepath"
	"testing"

	"cpa-usage-ranking/internal/storage"
)

func TestPeriodSchemaKeysAndConstraints(t *testing.T) {
	db := openDatabase(t, filepath.Join(t.TempDir(), "period.db"))
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	if queryInt(t, db.Reader, `SELECT count(*) FROM sqlite_schema WHERE name='period_stats' AND sql LIKE '%WITHOUT ROWID%'`) != 1 || queryInt(t, db.Reader, `SELECT count(*) FROM pragma_table_info('period_stats') WHERE name='tier'`) != 0 {
		t.Fatal("period table missing")
	}
	execSQL(t, db.Writer, `INSERT INTO clients(github_id,cpa_uuid,display_name,avatar_id,created_at_ms) VALUES(1,'550e8400-e29b-41d4-a716-446655440000','one',1,0), (2,'550e8400-e29b-41d4-a716-446655440000','two',1,0)`)
	insert := `INSERT INTO period_stats VALUES(?,?,?,?,1,1,0,0,0,0,1,1,0,0,0,0)`
	for _, row := range []struct {
		kind, day     string
		model, client int
	}{
		{"day", "2026-01-01", 0, 1}, {"week", "2025-12-29", 0, 1}, {"week", "2025-12-29", 1, 1},
		{"week", "2025-12-29", 1, 2}, {"month", "2026-01-01", 1, 1}, {"all", "0001-01-01", 1, 1},
	} {
		if _, err := db.Writer.Exec(insert, row.kind, row.day, row.model, row.client); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		kind, day     string
		model, client int
	}{
		{"day", "2026-01-01", 0, 1}, {"day", "2026-01-01", 1, 1}, {"week", "2025-12-29", 1, 1},
		{"week", "2025-12-29", -1, 1}, {"all", "2026-01-01", 1, 1}, {"year", "2026-01-01", 1, 1}, {"week", "2025-12-29", 1, 99},
	} {
		if _, err := db.Writer.Exec(insert, row.kind, row.day, row.model, row.client); err == nil {
			t.Fatalf("accepted invalid key %+v", row)
		}
	}
	if _, err := db.Writer.Exec(`UPDATE period_stats SET total_tokens=9.5 WHERE kind='day'`); err == nil {
		t.Fatal("accepted non-integer count")
	}
}
