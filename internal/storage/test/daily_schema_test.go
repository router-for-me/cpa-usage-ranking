package test

import (
	"context"
	"path/filepath"
	"testing"

	"cpa-usage-ranking/internal/storage"
)

func TestDailySnapshotSchemaKeysAndIsolation(t *testing.T) {
	db := openDatabase(t, filepath.Join(t.TempDir(), "daily.db"))
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	execSQL(t, db.Writer, `INSERT INTO clients(github_id,cpa_uuid,display_name,avatar_id,created_at_ms) VALUES
		(1,'550e8400-e29b-41d4-a716-446655440000','one',1,0),(2,'550e8400-e29b-41d4-a716-446655440000','two',1,0);
		INSERT INTO client_writers(id,client_id,token_hash) VALUES(1,1,randomblob(32)),(2,2,randomblob(32)),(3,1,randomblob(32));
		INSERT INTO models(model_name) VALUES('model');
		INSERT INTO daily_reports VALUES('2026-11-01',1,123),('2026-11-01',2,124),('2026-10-31',1,125);
		INSERT INTO daily_writer_versions VALUES('2026-11-01',1,1,0,zeroblob(32)),('2026-11-01',2,2,1,zeroblob(32)),
		('2026-11-01',3,1,1,zeroblob(32)),('2026-10-31',1,1,2,zeroblob(32))`)
	insert := `INSERT INTO daily_stats(day,client_id,model_id,total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples) VALUES(?,?,?,1,1,0,0,0,0,1,1,0,0,0,0)`
	for _, row := range []struct {
		day           string
		client, model int
	}{
		{"2026-11-01", 1, 0}, {"2026-11-01", 1, 1}, {"2026-11-01", 2, 0}, {"2026-10-31", 1, 0},
	} {
		if _, err := db.Writer.Exec(insert, row.day, row.client, row.model); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		day           string
		client, model int
	}{
		{"2026-11-01", 1, 0}, {"2026-11-01", 1, 1}, {"2026-11-01", 1, -1}, {"2026-11-02", 1, 1}, {"2026-11-01", 99, 1},
	} {
		if _, err := db.Writer.Exec(insert, row.day, row.client, row.model); err == nil {
			t.Fatalf("invalid row accepted: %+v", row)
		}
	}
	if _, err := db.Writer.Exec(`INSERT INTO daily_writer_versions VALUES('2026-10-31',3,1,0,x'00')`); err == nil {
		t.Fatal("short digest accepted")
	}
	if _, err := db.Writer.Exec(`INSERT INTO daily_writer_versions VALUES('2026-10-31',2,1,0,zeroblob(32))`); err == nil {
		t.Fatal("writer was attached to a different seat's daily snapshot")
	}
	if queryInt(t, db.Reader, "SELECT COUNT(*) FROM daily_stats WHERE model_id=0") != 3 || queryInt(t, db.Reader, "SELECT COUNT(*) FROM models WHERE id=0") != 0 {
		t.Fatal("total is not independent of models dictionary")
	}
	if queryInt(t, db.Reader, "SELECT COUNT(*) FROM pragma_table_info('daily_stats') WHERE name='tier'") != 0 ||
		queryInt(t, db.Reader, "SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name='daily_stats_totals' AND sql LIKE '%WHERE model_id=0%'") != 1 {
		t.Fatal("unexpected tier column or missing partial total index")
	}
	execSQL(t, db.Writer, `DELETE FROM daily_reports WHERE day='2026-11-01' AND client_id=1`)
	if queryInt(t, db.Reader, "SELECT COUNT(*) FROM daily_stats") != 2 || queryInt(t, db.Reader, "SELECT COUNT(*) FROM daily_writer_versions") != 2 {
		t.Fatal("cascade crossed day/client boundary")
	}
}
