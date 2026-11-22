package test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/reports"
)

type processorFunc func(context.Context, int64, string, reports.Prepared) (int64, error)

func (f processorFunc) Apply(ctx context.Context, client int64, day string, p reports.Prepared) (int64, error) {
	return f(ctx, client, day, p)
}

func resultWithin(t *testing.T, result <-chan reports.WriteResult) reports.WriteResult {
	t.Helper()
	select {
	case r := <-result:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not return")
		return reports.WriteResult{}
	}
}

func TestWriterBoundedQueueCancellationAndShutdown(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	processor := processorFunc(func(ctx context.Context, _ int64, _ string, _ reports.Prepared) (int64, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return 7, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	writer, err := reports.NewWriter(processor, reports.WriterOptions{QueueCapacity: 1, TransactionTimeout: time.Second * 5})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	}()
	first, err := writer.Enqueue(context.Background(), 1, "2026-11-01", reports.Prepared{})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	second, err := writer.Enqueue(queuedCtx, 2, "2026-11-01", reports.Prepared{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Enqueue(context.Background(), 3, "2026-11-01", reports.Prepared{}); !errors.Is(err, reports.ErrQueueFull) {
		t.Fatalf("full queue: %v", err)
	}
	cancelQueued()
	close(release)
	if result := resultWithin(t, first); result.Err != nil || result.Revision != 7 {
		t.Fatalf("first result: %+v", result)
	}
	if result := resultWithin(t, second); !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("cancelled queued result: %+v", result)
	}
	if calls.Load() != 1 {
		t.Fatal("cancelled queued request was executed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Enqueue(context.Background(), 1, "2026-11-01", reports.Prepared{}); !errors.Is(err, reports.ErrWriterClosed) {
		t.Fatalf("enqueue after shutdown: %v", err)
	}
}

func TestWriterShutdownCancelsAndDrainsWithoutSendingOnClosedChannel(t *testing.T) {
	started := make(chan struct{})
	processor := processorFunc(func(ctx context.Context, _ int64, _ string, _ reports.Prepared) (int64, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	writer, err := reports.NewWriter(processor, reports.WriterOptions{QueueCapacity: 8, TransactionTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	first, err := writer.Enqueue(context.Background(), 1, "", reports.Prepared{})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := writer.Enqueue(context.Background(), 2, "", reports.Prepared{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writer.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("aborted shutdown: %v", err)
	}
	if r := resultWithin(t, first); !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("active write not cancelled: %+v", r)
	}
	if r := resultWithin(t, second); !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("queued write not cancelled: %+v", r)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = writer.Enqueue(context.Background(), 1, "", reports.Prepared{})
			_ = writer.Close(context.Background())
		}()
	}
	wg.Wait()
}

func TestQueuedRealSQLiteRechecksWindowAndDisabledClient(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	firstID, disabledID := createReportClient(t, db), createReportClient(t, db)
	var clock atomic.Int64
	clock.Store(instant("2026-10-31T23:59:59Z").UnixNano())
	store := reports.NewStore(db.Writer, registry, func() time.Time { return time.Unix(0, clock.Load()).UTC() })
	started := make(chan struct{})
	var once sync.Once
	processor := processorFunc(func(ctx context.Context, id int64, day string, p reports.Prepared) (int64, error) {
		once.Do(func() { close(started) })
		return store.Apply(ctx, id, day, p)
	})
	writer, err := reports.NewWriter(processor, reports.WriterOptions{QueueCapacity: 1, TransactionTimeout: time.Second * 5})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = writer.Close(ctx)
	}()
	tx, err := db.Writer.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE clients SET disabled=1 WHERE id=?`, disabledID); err != nil {
		t.Fatal(err)
	}
	payload := prepared(t, 1, usage("alpha", 1, 1, 1))
	first, err := writer.Enqueue(context.Background(), firstID, "2026-10-30", payload)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := writer.Enqueue(context.Background(), disabledID, "2026-10-31", payload)
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(instant("2026-11-01T00:00:00Z").UnixNano())
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if r := resultWithin(t, first); !errors.Is(r.Err, reports.ErrExpiredDay) {
		t.Fatalf("queued cutoff: %+v", r)
	}
	if r := resultWithin(t, second); !errors.Is(r.Err, clients.ErrDisabled) {
		t.Fatalf("queued disabled: %+v", r)
	}
	if scalar(t, db.Reader, `SELECT COUNT(*) FROM daily_reports`) != 0 {
		t.Fatal("rejected queued report persisted")
	}
}
