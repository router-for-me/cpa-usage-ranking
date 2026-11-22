package test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

func seedClosedReports(t *testing.T, db *storage.DB, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for range n {
		id := createReportClient(t, db)
		ids = append(ids, id)
		if _, err := db.Writer.Exec(`INSERT INTO daily_reports VALUES('2026-11-01',?,0);
			INSERT INTO daily_stats VALUES('2026-11-01',?,0,4,4,0,0,0,0,1,1,1,1,0,0);
			INSERT INTO daily_stats VALUES('2026-11-01',?,1,4,4,0,0,0,0,1,1,1,1,0,0)`, id, id, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := reports.AdvanceWindow(context.Background(), db.Writer, instant("2026-11-03T12:00:00Z")); err != nil {
		t.Fatal(err)
	}
	return ids
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func startMaintenance(t *testing.T, db *storage.DB, options reports.MaintenanceOptions, logger *slog.Logger, processor reports.SnapshotProcessor) (*reports.Writer, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	w, err := reports.NewWriter(processor, reports.WriterOptions{QueueCapacity: 16, TransactionTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	m, err := reports.NewMaintenance(db.Reader, db.Writer, w, logger, options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		if err := w.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return w, cancel, done
}

func TestMaintenanceSmallBatchesAndIndexedCursor(t *testing.T) {
	db := reportDB(t)
	seedClosedReports(t, db, 7)
	var detail string
	rows, err := db.Reader.Query(`EXPLAIN QUERY PLAN SELECT day,client_id FROM daily_reports WHERE (day,client_id) > (?,?) AND day < (SELECT closed_before_day FROM report_window WHERE id=1) ORDER BY day,client_id LIMIT ?`, "", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent, unused int
		var d string
		if err := rows.Scan(&id, &parent, &unused, &d); err != nil {
			t.Fatal(err)
		}
		detail += d
	}
	rows.Close()
	if !strings.Contains(detail, "SEARCH daily_reports USING PRIMARY KEY") {
		t.Fatalf("cursor not indexed: %s", detail)
	}
	opts := reports.DefaultMaintenanceOptions()
	opts.BatchSize = 2
	opts.Rounds = 1
	opts.Interval = 2 * time.Second
	_, cancel, done := startMaintenance(t, db, opts, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), processorFunc(func(context.Context, int64, string, reports.Prepared) (int64, error) { return 0, nil }))
	waitFor(t, func() bool { return scalar(t, db.Reader, `SELECT count(*) FROM daily_reports`) == 5 })
	time.Sleep(100 * time.Millisecond)
	if got := scalar(t, db.Reader, `SELECT count(*) FROM daily_reports`); got != 5 {
		t.Fatalf("exceeded one batch: %d", got)
	}
	cancel()
	<-done
}

func TestMaintenanceContinuesPastFailuresAndCoolsDown(t *testing.T) {
	db := reportDB(t)
	ids := seedClosedReports(t, db, 8)
	_, err := db.Writer.Exec(fmt.Sprintf(`CREATE TRIGGER fail BEFORE INSERT ON period_stats WHEN NEW.kind='week' AND NEW.client_id IN (%d,%d) BEGIN SELECT RAISE(ABORT,'bad item'); END`, ids[0], ids[1]))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	opts := reports.DefaultMaintenanceOptions()
	opts.BatchSize = 2
	opts.Rounds = 2
	opts.Interval = 20 * time.Millisecond
	opts.Cooldown = 800 * time.Millisecond
	opts.ObservationInterval = time.Second
	opts.DailyRetentionDays = reports.MaxDailyRetentionDays
	w, cancel, done := startMaintenance(t, db, opts, slog.New(slog.NewTextHandler(&logs, nil)), processorFunc(func(context.Context, int64, string, reports.Prepared) (int64, error) { return 0, nil }))
	waitFor(t, func() bool { return scalar(t, db.Reader, `SELECT count(*) FROM daily_reports`) == 2 })
	if scalar(t, db.Reader, `SELECT count(*) FROM period_stats WHERE kind='day'`) != 6 {
		t.Fatal("healthy clients did not progress")
	}
	if _, err := db.Writer.Exec(`DROP TRIGGER fail`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	if scalar(t, db.Reader, `SELECT count(*) FROM daily_reports`) != 2 {
		t.Fatal("failed items retried before cooldown")
	}
	waitFor(t, func() bool { return scalar(t, db.Reader, `SELECT count(*) FROM daily_reports`) == 0 })
	cancel()
	<-done
	if err := w.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Count(logs.String(), "settlement item failed") != 1 || !strings.Contains(logs.String(), "settlement backlog") {
		t.Fatalf("unexpected logs: %s", logs.String())
	}
}

func TestMaintenanceAndRealReportsShareWriter(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	seedClosedReports(t, db, 12)
	id := createReportClient(t, db)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return instant("2026-11-03T12:00:00Z") })
	opts := reports.DefaultMaintenanceOptions()
	opts.BatchSize = 3
	opts.Rounds = 2
	opts.Interval = 10 * time.Millisecond
	opts.DailyRetentionDays = reports.MaxDailyRetentionDays
	w, _, _ := startMaintenance(t, db, opts, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), store)
	result, err := w.Enqueue(context.Background(), id, "2026-11-03", prepared(t, 1, usage("alpha", 9, 1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if r := resultWithin(t, result); r.Err != nil || r.Revision != 1 {
		t.Fatalf("report: %+v", r)
	}
	waitFor(t, func() bool {
		return scalar(t, db.Reader, `SELECT count(*) FROM daily_reports WHERE day='2026-11-01'`) == 0
	})
	if scalar(t, db.Reader, `SELECT total_tokens FROM daily_stats WHERE day='2026-11-03' AND client_id=? AND model_id=0`, id) != 9 {
		t.Fatal("report lost")
	}
	if scalar(t, db.Reader, `SELECT count(*) FROM period_stats WHERE kind='day'`) != 12 {
		t.Fatal("settlement lost")
	}
}

func TestMaintenanceShutdownCancelsRealSQLiteLock(t *testing.T) {
	db := reportDB(t)
	ids := seedClosedReports(t, db, 1)
	var path string
	if err := db.Reader.QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil {
		t.Fatal(err)
	}
	lock, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := lock.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	defer lock.Exec(`ROLLBACK`)
	w, err := reports.NewWriter(processorFunc(func(context.Context, int64, string, reports.Prepared) (int64, error) { return 0, nil }), reports.WriterOptions{QueueCapacity: 1, TransactionTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	r, err := w.EnqueueMaintenance(context.Background(), func(ctx context.Context) error {
		close(started)
		_, err := reports.Finalize(ctx, db.Writer, "2026-11-01", ids[0])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := w.Close(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("shutdown: %v elapsed %v", err, time.Since(start))
	}
	if err := <-r; !errors.Is(err, context.Canceled) {
		t.Fatalf("transaction: %v", err)
	}
	if scalar(t, db.Reader, `SELECT count(*) FROM daily_reports`) != 1 || scalar(t, db.Writer, `PRAGMA busy_timeout`) != 5000 {
		t.Fatal("source or timeout changed")
	}
}
