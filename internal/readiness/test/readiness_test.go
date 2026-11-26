package test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/readiness"
	"cpa-usage-ranking/internal/statistics"
	"cpa-usage-ranking/internal/storage"
)

func TestReadinessTracksRealDependenciesAndCompleteCache(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ready.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err = os.WriteFile(path, []byte(`{"currency":"USD","price_unit":"per_million_tokens","models":[{"model_name":"alpha","alias":[],"prices":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cat, err := models.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := models.Register(ctx, db.Writer, cat)
	if err != nil {
		t.Fatal(err)
	}
	avatar, err := avatars.Load(avatars.BuiltinFS())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := statistics.NewReader(db.Reader, 100, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	var fail atomic.Bool
	fail.Store(true)
	item := cache.Item{ID: "fixture", Kind: periods.All, Encode: func(_ context.Context, _ statistics.Snapshot, w io.Writer) error {
		sequence := calls.Add(1)
		if fail.Load() {
			return errors.New("failed fixture")
		}
		_, err := io.WriteString(w, strconv.FormatInt(sequence, 10))
		return err
	}}
	opts := cache.DefaultOptions()
	opts.Interval = 25 * time.Millisecond
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := cache.New(reader, []cache.Item{item}, opts, logger)
	if err != nil {
		t.Fatal(err)
	}
	check := readiness.New(db, reg, avatar, c)
	router := httpserver.NewRouterWithReadiness(logger, check.Ready)
	status := func(path string) int {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if status("/healthz") != 200 || status("/readyz") != 503 {
		t.Fatal("initial health/readiness")
	}
	if _, err = c.Current(); !errors.Is(err, cache.ErrUnavailable) {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	wait := func(fn func() bool) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			if fn() {
				return
			}
			select {
			case <-deadline:
				t.Fatal("timed out")
			case <-tick.C:
			}
		}
	}
	wait(func() bool { return calls.Load() >= 1 })
	if status("/readyz") != 503 {
		t.Fatal("failed first build ready")
	}
	fail.Store(false)
	wait(func() bool { return status("/readyz") == 200 })
	if readiness.New(db, nil, avatar, c).Ready(ctx) || readiness.New(db, reg, nil, c).Ready(ctx) || readiness.New(nil, reg, avatar, c).Ready(ctx) || readiness.New(db, reg, avatar, nil).Ready(ctx) {
		t.Fatal("missing dependency accepted")
	}
	cancelled, stop := context.WithCancel(ctx)
	stop()
	if check.Ready(cancelled) {
		t.Fatal("cancelled DB probe accepted")
	}
	fail.Store(true)
	n := calls.Load()
	wait(func() bool { return calls.Load() > n })
	old, err := c.Current()
	if err != nil {
		t.Fatal(err)
	}
	n = calls.Load()
	wait(func() bool { return calls.Load() > n })
	if status("/readyz") != 200 {
		t.Fatal("failed refresh discarded successful cache")
	}
	current, _ := c.Current()
	oldEntry, _ := old.Get("fixture")
	currentEntry, _ := current.Get("fixture")
	if currentEntry != oldEntry {
		t.Fatal("failed refresh replaced old cache")
	}
	// 写连接持锁期间 /readyz 不应阻塞或误报未就绪，且支持并发探针合并。
	writerTx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if status("/readyz") != 200 {
				t.Error("readyz failed while writer held transaction")
			}
		})
	}
	wg.Wait()
	if err := writerTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	// 等待 1s 探针缓存过期后，关闭只读库应被准确检出为 503。
	time.Sleep(1100 * time.Millisecond)
	if status("/readyz") != 503 || status("/healthz") != 200 {
		t.Fatal("closed database was ready or liveness failed")
	}
}
func TestReadinessNoBuildItemsNotReady(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	r, err := statistics.NewReader(db.Reader, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := cache.New(r, nil, cache.DefaultOptions(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	// 即使依赖指针存在，没有构建项也不会产生成功缓存。
	check := readiness.New(db, &models.Registry{}, &avatars.Catalog{}, c)
	if check.Ready(ctx) {
		t.Fatal("no items marked ready")
	}
	if _, err := c.Current(); !errors.Is(err, cache.ErrUnavailable) {
		t.Fatal("missing unavailable error")
	}
}

func TestReadinessFirstProbeHonorsCallerCancellation(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "models.json")
	if err = os.WriteFile(path, []byte(`{"models":[{"model_name":"alpha"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cat, err := models.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := models.Register(ctx, db.Writer, cat)
	if err != nil {
		t.Fatal(err)
	}
	avatar, err := avatars.Load(avatars.BuiltinFS())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := statistics.NewReader(db.Reader, 10, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	item := cache.Item{ID: "fixture", Kind: periods.All, Encode: func(_ context.Context, _ statistics.Snapshot, w io.Writer) error {
		_, err := io.WriteString(w, "ok")
		return err
	}}
	opts := cache.DefaultOptions()
	opts.Interval = time.Hour
	c, err := cache.New(reader, []cache.Item{item}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopCache := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- c.Run(runCtx) }()
	defer func() { stopCache(); <-done }()
	for {
		if _, err := c.Current(); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}

	// 占满读池唯一连接使首个探针阻塞，验证首个调用方取消请求时可立即返回而非等满 1s。
	db.Reader.SetMaxOpenConns(1)
	held, err := db.Reader.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	check := readiness.New(db, reg, avatar, c)
	callerCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	if check.Ready(callerCtx) {
		t.Fatal("cancelled first probe unexpectedly succeeded")
	}
	if elapsed := time.Since(start); elapsed >= 400*time.Millisecond {
		t.Fatalf("first probe ignored caller cancellation; took %v", elapsed)
	}
	_ = held.Close()
	if !check.Ready(ctx) {
		t.Fatal("probe did not recover after reader connection released")
	}
}
