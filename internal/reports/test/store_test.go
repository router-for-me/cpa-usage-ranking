package test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
	"cpa-usage-ranking/internal/testfixture"
)

func createReportClient(t *testing.T, db *storage.DB) int64 {
	t.Helper()
	var hash clients.CredentialHash
	rand.Read(hash[:])
	writer, err := testfixture.Seat(context.Background(), db.Writer, hash, "client", 1)
	if err != nil {
		t.Fatal(err)
	}
	if writer.ID != writer.WriterID {
		t.Fatal("single-writer fixture requires equal IDs")
	}
	return writer.ID
}

func prepared(t *testing.T, revision int64, items ...reports.Item) reports.Prepared {
	t.Helper()
	p, err := reports.Prepare(reports.Report{Revision: revision, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func scalar(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var result int64
	if err := db.QueryRow(query, args...).Scan(&result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAtomicReplacementDiffsAndEmptySnapshots(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":["alias"],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}`)
	client, second := createReportClient(t, db), createReportClient(t, db)
	now := instant("2026-11-01T12:00:00Z")
	store := reports.NewStore(db.Writer, registry, func() time.Time { return now })
	ctx, day := context.Background(), "2026-11-01"
	alpha, beta := usage("alpha", 10, 2, 3), usage("beta", 20, 1, 4)
	first := prepared(t, 1, alpha, beta)
	if revision, err := store.Apply(ctx, client, day, first); err != nil || revision != 1 {
		t.Fatalf("first apply: %d %v", revision, err)
	}
	if _, err := store.Apply(ctx, second, day, prepared(t, 4, alpha)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`CREATE TABLE audit(action TEXT,model_id INTEGER);
		CREATE TRIGGER stats_insert AFTER INSERT ON daily_stats BEGIN INSERT INTO audit VALUES('insert',NEW.model_id); END;
		CREATE TRIGGER stats_update AFTER UPDATE ON daily_stats BEGIN INSERT INTO audit VALUES('update',NEW.model_id); END;
		CREATE TRIGGER stats_delete AFTER DELETE ON daily_stats BEGIN INSERT INTO audit VALUES('delete',OLD.model_id); END`); err != nil {
		t.Fatal(err)
	}
	received := scalar(t, db.Reader, `SELECT received_at_ms FROM daily_reports WHERE day=? AND client_id=?`, day, client)
	now = now.Add(time.Hour)
	if _, err := store.Apply(ctx, client, day, first); err != nil {
		t.Fatal(err)
	}
	if received != scalar(t, db.Reader, `SELECT received_at_ms FROM daily_reports WHERE day=? AND client_id=?`, day, client) || scalar(t, db.Reader, `SELECT COUNT(*) FROM audit`) != 0 {
		t.Fatal("identical retry rewrote snapshot")
	}
	if _, err := store.Apply(ctx, client, day, prepared(t, 6, alpha, beta)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db.Reader, `SELECT COUNT(*) FROM audit`) != 0 {
		t.Fatal("higher revision rewrote unchanged statistics")
	}
	if scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE day=? AND client_id=?`, day, client) != 6 {
		t.Fatal("new revision not persisted")
	}
	var conflict *reports.RevisionConflict
	if _, err := store.Apply(ctx, client, day, first); !errors.As(err, &conflict) || conflict.Current != 6 {
		t.Fatalf("old version: %v", err)
	}
	if _, err := store.Apply(ctx, client, day, prepared(t, 6, alpha)); !errors.As(err, &conflict) || conflict.Current != 6 {
		t.Fatalf("changed same version: %v", err)
	}
	alpha.TotalTokens, alpha.InputTokens = 3, 3
	if _, err := store.Apply(ctx, client, day, prepared(t, 9, alpha)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db.Reader, `SELECT COUNT(*) FROM audit WHERE action='update'`) != 2 || scalar(t, db.Reader, `SELECT COUNT(*) FROM audit WHERE action='delete'`) != 1 {
		t.Fatal("did not update exactly changed total/detail and delete missing detail")
	}
	if scalar(t, db.Reader, `SELECT total_tokens FROM daily_stats WHERE day=? AND client_id=? AND model_id=0`, day, client) != 3 || scalar(t, db.Reader, `SELECT total_tokens FROM daily_stats WHERE day=? AND client_id=? AND model_id=0`, day, second) != 10 {
		t.Fatal("decrease or client isolation failed")
	}
	if _, err := store.Apply(ctx, client, day, prepared(t, 10, usage("unknown", 4, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db.Reader, `SELECT COUNT(*) FROM daily_stats WHERE day=? AND client_id=?`, day, client) != 0 || scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE day=? AND client_id=?`, day, client) != 10 {
		t.Fatal("empty filtered snapshot did not clear rows and retain version")
	}
	if _, err := store.Apply(ctx, client, day, prepared(t, 11)); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db.Reader, `SELECT COUNT(*) FROM daily_stats WHERE day=? AND client_id=?`, day, client) != 0 {
		t.Fatal("empty items created total")
	}
}

func TestIdempotentRetryDoesNotAggregateNewCatalog(t *testing.T) {
	db := reportDB(t)
	old := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	client := createReportClient(t, db)
	clock := func() time.Time { return instant("2026-11-01T12:00:00Z") }
	input := []reports.Item{usage("new1", math.MaxInt64, 1, 1), usage("new2", math.MaxInt64, 1, 1)}
	p := prepared(t, 3, input...)
	if _, err := reports.NewStore(db.Writer, old, clock).Apply(context.Background(), client, "2026-11-01", p); err != nil {
		t.Fatal(err)
	}
	changed := reportRegistry(t, db, `{"model_name":"alpha","alias":["new1","new2"],"prices":{"default":{"input":"2"}}}`)
	newStore := reports.NewStore(db.Writer, changed, clock)
	if _, err := newStore.Apply(context.Background(), client, "2026-11-01", p); err != nil {
		t.Fatalf("retry reaggregated: %v", err)
	}
	if _, err := newStore.Apply(context.Background(), client, "2026-11-01", prepared(t, 4, input...)); !errors.Is(err, reports.ErrInvalid) {
		t.Fatalf("new aggregation overflow accepted: %v", err)
	}
	if scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE client_id=?`, client) != 3 || scalar(t, db.Reader, `SELECT COUNT(*) FROM daily_stats`) != 0 {
		t.Fatal("failed new replacement changed old snapshot")
	}
}

func TestSQLiteFailureRollsBackVersionAndEveryRow(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	client := createReportClient(t, db)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return instant("2026-11-01T12:00:00Z") })
	ctx, day := context.Background(), "2026-11-01"
	if _, err := store.Apply(ctx, client, day, prepared(t, 1, usage("alpha", 10, 1, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`CREATE TRIGGER fail_detail BEFORE UPDATE ON daily_stats WHEN NEW.model_id > 0 BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, client, day, prepared(t, 2, usage("alpha", 20, 1, 1))); err == nil {
		t.Fatal("injected SQLite failure reported success")
	}
	if scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE client_id=?`, client) != 1 || scalar(t, db.Reader, `SELECT SUM(total_tokens) FROM daily_stats WHERE client_id=?`, client) != 20 {
		t.Fatal("partial metadata or stats escaped rollback")
	}
}

func TestConcurrentRevisionsDisabledAndCancellation(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	client := createReportClient(t, db)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return instant("2026-11-01T12:00:00Z") })
	ctx, day := context.Background(), "2026-11-01"
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		p := prepared(t, int64(i), usage("alpha", int64(i+1), 1, 1))
		wg.Add(1)
		go func() { defer wg.Done(); _, errs[i] = store.Apply(ctx, client, day, p) }()
	}
	wg.Wait()
	for _, err := range errs {
		var conflict *reports.RevisionConflict
		if err != nil && !errors.As(err, &conflict) {
			t.Fatal(err)
		}
	}
	if scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE client_id=?`, client) != n-1 || scalar(t, db.Reader, `SELECT total_tokens FROM daily_stats WHERE client_id=? AND model_id=0`, client) != n {
		t.Fatal("concurrent revisions lost latest snapshot")
	}
	if _, err := db.Writer.Exec(`UPDATE clients SET disabled=1 WHERE id=?`, client); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, client, day, prepared(t, n)); !errors.Is(err, clients.ErrDisabled) {
		t.Fatalf("disabled client: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Apply(cancelled, client, day, prepared(t, n)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write: %v", err)
	}
	if scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE client_id=?`, client) != n-1 {
		t.Fatal("disabled/cancelled write changed revision")
	}
}

func TestTransactionCrossingMidnightDoesNotCommit(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	client := createReportClient(t, db)
	before, after := instant("2026-10-31T23:59:59Z"), instant("2026-11-01T00:00:00Z")
	calls := 0
	store := reports.NewStore(db.Writer, registry, func() time.Time {
		calls++
		if calls <= 3 {
			return before
		}
		return after
	})
	_, err := store.Apply(context.Background(), client, "2026-10-30", prepared(t, 1, usage("alpha", 5, 1, 1)))
	if !errors.Is(err, reports.ErrExpiredDay) {
		t.Fatalf("cross-midnight write: %v", err)
	}
	if scalar(t, db.Reader, `SELECT COUNT(*) FROM daily_reports`) != 0 || scalar(t, db.Reader, `SELECT COUNT(*) FROM daily_stats`) != 0 {
		t.Fatal("cross-midnight write persisted")
	}
	var watermark string
	if err := db.Reader.QueryRow(`SELECT closed_before_day FROM report_window WHERE id=1`).Scan(&watermark); err != nil || watermark != "2026-10-31" {
		t.Fatalf("watermark not committed separately: %s %v", watermark, err)
	}
}
