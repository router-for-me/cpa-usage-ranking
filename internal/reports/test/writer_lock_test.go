package test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
	"github.com/mattn/go-sqlite3"
)

func TestWriterDeadlineInterruptsSQLiteWriteLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	client := createReportClient(t, db)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return instant("2026-11-01T12:00:00Z") })
	day := "2026-11-01"
	if _, err := store.Apply(context.Background(), client, day, prepared(t, 1, usage("alpha", 1, 1, 1))); err != nil {
		t.Fatal(err)
	}
	writer, err := reports.NewWriter(store, reports.WriterOptions{QueueCapacity: 1, TransactionTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	lock, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer lock.Exec("ROLLBACK")
	result, err := writer.Enqueue(context.Background(), client, day, prepared(t, 2, usage("alpha", 2, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-result:
		if !errors.Is(outcome.Err, context.DeadlineExceeded) {
			t.Fatalf("locked write returned %v, want deadline exceeded", outcome.Err)
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("SQLite lock ignored the report deadline")
	}
	if got := scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE client_id=?`, client); got != 1 {
		t.Fatalf("timed-out report changed revision to %d", got)
	}
	if got := scalar(t, db.Reader, `SELECT total_tokens FROM daily_stats WHERE client_id=? AND model_id=0`, client); got != 1 {
		t.Fatalf("timed-out report changed total to %d", got)
	}
	if got := scalar(t, db.Writer, `PRAGMA busy_timeout`); got != 5000 {
		t.Fatalf("writer pool busy timeout was not restored: %d", got)
	}
}

func TestSQLiteLockReleaseAndWriterShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locked.db")
	db, err := storage.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	client := createReportClient(t, db)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return instant("2026-11-01T12:00:00Z") })
	writer, err := reports.NewWriter(store, reports.WriterOptions{QueueCapacity: 1, TransactionTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	lock, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer lock.Exec("ROLLBACK")
	day := "2026-11-01"
	first, err := writer.Enqueue(context.Background(), client, day, prepared(t, 1, usage("alpha", 1, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-first:
		t.Fatalf("write completed while locked: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := lock.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	if result := resultWithin(t, first); result.Err != nil || result.Revision != 1 {
		t.Fatalf("transient lock prevented commit: %+v", result)
	}
	if _, err := lock.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	second, err := writer.Enqueue(context.Background(), client, day, prepared(t, 2, usage("alpha", 2, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-second:
		t.Fatalf("write completed while locked: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := writer.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown returned %v", err)
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatal("shutdown waited for SQLite's original busy timeout")
	}
	if result := resultWithin(t, second); !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("shutdown did not cancel locked write: %+v", result)
	}
	if got := scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE client_id=?`, client); got != 1 {
		t.Fatalf("cancelled report changed revision to %d", got)
	}
	if got := scalar(t, db.Writer, `PRAGMA busy_timeout`); got != 5000 {
		t.Fatalf("writer pool busy timeout was not restored: %d", got)
	}
}

func TestSQLiteInFlightTransactionCancellationRollsBack(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	client := createReportClient(t, db)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return instant("2026-11-01T12:00:00Z") })
	day := "2026-11-01"
	if _, err := store.Apply(context.Background(), client, day, prepared(t, 1, usage("alpha", 1, 1, 1))); err != nil {
		t.Fatal(err)
	}
	// 测试回调确认事务已更新元数据及合计，再让真实 SQLite 语句运行至关停中断。
	started := make(chan struct{}, 1)
	conn, err := db.Writer.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Raw(func(raw any) error {
		return raw.(*sqlite3.SQLiteConn).RegisterFunc("report_test_started", func() int {
			started <- struct{}{}
			return 0
		}, false)
	}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`CREATE TRIGGER slow_detail BEFORE UPDATE ON daily_stats WHEN NEW.model_id>0 BEGIN
		SELECT report_test_started();
		SELECT (WITH RECURSIVE n(i) AS (VALUES(0) UNION ALL SELECT i+1 FROM n WHERE i<100000000) SELECT sum(i) FROM n);
	END`); err != nil {
		t.Fatal(err)
	}
	writer, err := reports.NewWriter(store, reports.WriterOptions{QueueCapacity: 1, TransactionTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	result, err := writer.Enqueue(context.Background(), client, day, prepared(t, 2, usage("alpha", 2, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("transaction did not reach the detail update")
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := writer.Close(shutdown); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown returned %v", err)
	}
	if outcome := resultWithin(t, result); !errors.Is(outcome.Err, context.Canceled) {
		t.Fatalf("in-flight write returned %+v", outcome)
	}
	if got := scalar(t, db.Reader, `SELECT revision FROM daily_writer_versions WHERE client_id=?`, client); got != 1 {
		t.Fatalf("cancelled transaction changed revision to %d", got)
	}
	if got := scalar(t, db.Reader, `SELECT SUM(total_tokens) FROM daily_stats WHERE client_id=?`, client); got != 2 {
		t.Fatalf("cancelled transaction changed statistics sum to %d", got)
	}
}
