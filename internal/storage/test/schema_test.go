package test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"cpa-usage-ranking/internal/storage"
)

func queryInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var value int
	if err := db.QueryRow(query).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestCurrentSchemaInitializesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ranking.db")
	db := openDatabase(t, path)
	if err := storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"clients", "client_writers", "registration_requests", "models", "report_window", "daily_reports", "daily_writer_versions", "daily_stats", "period_stats", "admin_accounts"} {
		var count int
		if err := db.Reader.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table' AND name=?", name).Scan(&count); err != nil || count != 1 {
			t.Fatalf("missing table %s: %v", name, err)
		}
	}
	if queryInt(t, db.Reader, "SELECT count(*) FROM sqlite_schema WHERE name='schema_migrations'") != 0 ||
		queryInt(t, db.Reader, "SELECT count(*) FROM clients") != 0 || queryInt(t, db.Reader, "SELECT count(*) FROM models") != 0 {
		t.Fatal("initialization created a ledger or seeded data")
	}
	execSQL(t, db.Writer, `INSERT INTO clients(github_id,server_id,display_name,avatar_id,created_at_ms) VALUES(1,'550e8400-e29b-41d4-a716-446655440000','preserved',1,123)`)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = openDatabase(t, path)
	for range 2 {
		if err := storage.InitSchema(ctx, db.Writer); err != nil {
			t.Fatal(err)
		}
	}
	if queryInt(t, db.Reader, "SELECT count(*) FROM clients WHERE display_name='preserved' AND ranking_hidden=0 AND created_at_ms=123") != 1 {
		t.Fatal("reinitialization changed existing data")
	}
}

func TestSchemaInitializationRollsBackOnError(t *testing.T) {
	db := openDatabase(t, filepath.Join(t.TempDir(), "ranking.db"))
	// A conflicting index name causes initialization to fail after creating tables.
	execSQL(t, db.Writer, `CREATE TABLE blocker(value INTEGER); CREATE TABLE clients_ranking_hidden_id(value INTEGER)`)
	if err := storage.InitSchema(context.Background(), db.Writer); err == nil {
		t.Fatal("initialization unexpectedly succeeded")
	}
	if queryInt(t, db.Reader, "SELECT count(*) FROM sqlite_schema WHERE name='clients'") != 0 {
		t.Fatal("failed initialization left partial schema")
	}
}

func TestFormerClientSchemaIsNotMigratedOrBackfilled(t *testing.T) {
	t.Run("legacy token_hash clients table", func(t *testing.T) {
		db := openDatabase(t, filepath.Join(t.TempDir(), "legacy.db"))
		execSQL(t, db.Writer, `CREATE TABLE clients(id INTEGER PRIMARY KEY, token_hash BLOB NOT NULL UNIQUE);
			INSERT INTO clients VALUES(42,zeroblob(32))`)
		if err := storage.InitSchema(context.Background(), db.Writer); err == nil {
			t.Fatal("former client table was accepted")
		}
		if queryInt(t, db.Reader, "SELECT count(*) FROM clients WHERE id=42 AND token_hash=zeroblob(32)") != 1 ||
			queryInt(t, db.Reader, "SELECT count(*) FROM sqlite_schema WHERE name IN ('client_writers','registration_requests')") != 0 {
			t.Fatal("failed initialization touched the old schema or data")
		}
	})

	// 仅含旧 cpa_uuid 列（无 server_id 列）的 clients 表必须被拒绝并完整回滚。
	t.Run("legacy cpa_uuid clients table", func(t *testing.T) {
		db := openDatabase(t, filepath.Join(t.TempDir(), "legacy-cpa-uuid.db"))
		execSQL(t, db.Writer, `CREATE TABLE clients(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			github_id INTEGER NOT NULL,
			cpa_uuid TEXT NOT NULL,
			display_name TEXT NOT NULL,
			avatar_id INTEGER NOT NULL,
			disabled INTEGER NOT NULL DEFAULT 0,
			created_at_ms INTEGER NOT NULL,
			ranking_hidden INTEGER NOT NULL DEFAULT 0,
			withdrawn INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO clients(id,github_id,cpa_uuid,display_name,avatar_id,created_at_ms)
			VALUES(7,100,'550e8400-e29b-41d4-a716-446655440000','legacy',1,456)`)
		if err := storage.InitSchema(context.Background(), db.Writer); err == nil {
			t.Fatal("legacy cpa_uuid client table was accepted")
		}
		if queryInt(t, db.Reader, "SELECT count(*) FROM clients WHERE id=7 AND github_id=100 AND cpa_uuid='550e8400-e29b-41d4-a716-446655440000' AND display_name='legacy' AND created_at_ms=456") != 1 ||
			queryInt(t, db.Reader, "SELECT count(*) FROM sqlite_schema WHERE name IN ('client_writers','registration_requests','clients_github_server_id')") != 0 {
			t.Fatal("failed initialization touched the legacy cpa_uuid schema or data")
		}
	})
}

func TestClientSchemaEnforcesCredentialAndStateShape(t *testing.T) {
	db := openDatabase(t, filepath.Join(t.TempDir(), "ranking.db"))
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	const insert = `INSERT INTO clients(github_id,server_id,display_name,avatar_id,disabled,withdrawn,created_at_ms) VALUES(?,?,'test',1,?,?,0)`
	const serverID = "550e8400-e29b-41d4-a716-446655440000"
	if _, err := db.Writer.Exec(insert, 42, serverID, 0, 0); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]any{{42, serverID, 0, 0}, {0, serverID, 0, 0}, {"nonnumeric", serverID, 0, 0}, {43, "bad", 0, 0}, {43, serverID, 2, 0}, {43, serverID, 0, 2}} {
		if _, err := db.Writer.Exec(insert, args...); err == nil {
			t.Fatal("invalid or duplicate seat persisted")
		}
	}
	for _, hash := range []any{nil, []byte{1}, "01234567890123456789012345678901"} {
		if _, err := db.Writer.Exec("INSERT INTO client_writers(client_id,token_hash) VALUES(1,?)", hash); err == nil {
			t.Fatal("invalid credential persisted")
		}
	}
	if queryInt(t, db.Reader, "SELECT COUNT(*) FROM clients") != 1 || queryInt(t, db.Reader, "SELECT COUNT(*) FROM client_writers") != 0 {
		t.Fatal("failed inserts left rows")
	}
	const insertReq = `INSERT INTO registration_requests(id,token_hash,server_id,return_to,avatar_id,expires_at_ms,stage) VALUES(?,?,?,?,?,1,'pending')`
	hash := make([]byte, 32)
	if _, err := db.Writer.Exec(insertReq, "valid-req", hash, serverID, "https://app.example/cb", 42); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(insertReq, "null-avatar-req", hash, serverID, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, avatar := range []any{0, -1, "nonnumeric", 1.5} {
		if _, err := db.Writer.Exec(insertReq, "bad-req", hash, serverID, "https://app.example/cb", avatar); err == nil {
			t.Fatalf("invalid registration_requests.avatar_id %#v persisted", avatar)
		}
	}
	if queryInt(t, db.Reader, "SELECT COUNT(*) FROM registration_requests") != 2 {
		t.Fatal("failed registration_requests inserts left rows")
	}
}
