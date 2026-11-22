package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/leaderboard"
	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/statistics"
	"cpa-usage-ranking/internal/storage"
)

func cacheFixture(t *testing.T) (*storage.DB, *statistics.Reader) {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	_, err = db.Writer.Exec(`INSERT INTO clients(github_id,cpa_uuid,display_name,avatar_id,created_at_ms) VALUES(1,'550e8400-e29b-41d4-a716-446655440000','a',1,0);
 INSERT INTO client_writers(id,client_id,token_hash) VALUES(1,1,randomblob(32));
 INSERT INTO daily_reports(day,client_id,received_at_ms) VALUES('2026-12-31',1,0);
 INSERT INTO daily_writer_versions VALUES('2026-12-31',1,1,1,randomblob(32));
 INSERT INTO daily_stats VALUES('2026-12-31',1,0,12,12,0,0,0,0,1,1,0,0,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	r, err := statistics.NewReader(db.Reader, 100, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return db, r
}
func newCache(t *testing.T, r *statistics.Reader, items []cache.Item, interval time.Duration) *cache.Cache {
	t.Helper()
	opts := cache.DefaultOptions()
	opts.Interval = interval
	opts.ItemTimeout = time.Second
	c, err := cache.New(r, items, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func run(t *testing.T, c *cache.Cache) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("worker did not stop")
		}
	})
	return cancel, done
}
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if fn() {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out")
		case <-ticker.C:
		}
	}
}
func receive(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("event timed out")
	}
}
func TestNoItemsAndLegitimateEmptyResult(t *testing.T) {
	_, r := cacheFixture(t)
	c := newCache(t, r, nil, 20*time.Millisecond)
	cancel, done := run(t, c)
	time.Sleep(50 * time.Millisecond)
	if _, err := c.Current(); !errors.Is(err, cache.ErrUnavailable) {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// 空结果是有真实构建项的成功结果，不等于没有构建项。
	empty := cache.Item{ID: "empty", Kind: periods.Month, Encode: func(_ context.Context, s statistics.Snapshot, w io.Writer) error {
		if len(s.Rows) != 0 {
			return errors.New("expected empty")
		}
		return nil
	}}
	opts := cache.DefaultOptions()
	opts.Interval = time.Hour
	opts.Now = func() time.Time { return time.Date(2028, 1, 1, 0, 0, 0, 0, time.UTC) }
	c, err := cache.New(r, []cache.Item{empty}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = run(t, c)
	waitFor(t, func() bool { _, err := c.Current(); return err == nil })
	s, _ := c.Current()
	e, ok := s.Get("empty")
	if !ok || s.Len() != 1 || e.Content != "" || e.Start != "2028-01-01" {
		t.Fatal(e, ok)
	}
}
func TestCompleteRoundAtomicReplacementFailureAndImmutableReaders(t *testing.T) {
	_, r := cacheFixture(t)
	var round atomic.Int64
	var fail atomic.Bool
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var block atomic.Bool
	opts := cache.DefaultOptions()
	opts.Interval = 100 * time.Millisecond
	opts.Now = func() time.Time { n := round.Add(1); return time.Date(2026, 12, int(30+n), 0, 0, 0, 0, time.UTC) }
	encode := func(id string) func(context.Context, statistics.Snapshot, io.Writer) error {
		return func(ctx context.Context, s statistics.Snapshot, w io.Writer) error {
			if id == "one" && block.CompareAndSwap(true, false) {
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			if id == "two" && fail.Load() {
				return errors.New("fixture failure")
			}
			_, err := fmt.Fprintf(w, "%s:%s:%d", id, s.Start, s.Rows[1].TotalTokens)
			return err
		}
	}
	items := []cache.Item{{ID: "one", Kind: periods.Day, Encode: encode("one")}, {ID: "two", Kind: periods.Day, Encode: encode("two")}}
	c, err := cache.New(r, items, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = run(t, c)
	waitFor(t, func() bool { _, e := c.Current(); return e == nil })
	first, _ := c.Current()
	firstOne, _ := first.Get("one")
	firstTwo, _ := first.Get("two")
	if firstOne.Start != "2026-12-31" || firstTwo.Start != firstOne.Start || firstOne.Content != "one:2026-12-31:12" {
		t.Fatal(firstOne, firstTwo)
	}
	fail.Store(true)
	block.Store(true)
	receive(t, entered)
	// 阻塞下一轮时，并发读取仍只能观察已发布完整轮次。
	var wg sync.WaitGroup
	var bad atomic.Bool
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 1000 {
				s, e := c.Current()
				if e != nil {
					bad.Store(true)
					continue
				}
				a, ok := s.Get("one")
				b, ok2 := s.Get("two")
				if !ok || !ok2 || a.Start != b.Start {
					bad.Store(true)
				}
				// 覆盖本地副本不能改写发布对象或其他读者。
				s = cache.Snapshot{}
				s, e = c.Current()
				if _, ok := s.Get("one"); e != nil || !ok {
					bad.Store(true)
				}
			}
		}()
	}
	wg.Wait()
	if bad.Load() {
		t.Fatal("partial cache")
	}
	close(release)
	time.Sleep(70 * time.Millisecond)
	s, _ := c.Current()
	a, _ := s.Get("one")
	b, _ := s.Get("two")
	if a != firstOne || b != firstTwo {
		t.Fatal("failed round replaced cache", a, b)
	}
	fail.Store(false)
	waitFor(t, func() bool {
		s, e := c.Current()
		if e != nil {
			return false
		}
		a, _ := s.Get("one")
		return a.Start != firstOne.Start
	})
	s, _ = c.Current()
	a, _ = s.Get("one")
	b, _ = s.Get("two")
	if a.Start != b.Start {
		t.Fatal(a, b)
	}
	old, _ := first.Get("one")
	if old != firstOne {
		t.Fatal("published snapshot mutated")
	}
}
func TestCapacityTimeoutAndReleaseBeforeEncoding(t *testing.T) {
	db, r := cacheFixture(t)
	opts := cache.DefaultOptions()
	opts.Interval = 25 * time.Millisecond
	opts.ItemTimeout = 35 * time.Millisecond
	opts.MaxBytes = 3
	var mode atomic.Int32
	var calls atomic.Int32
	item := cache.Item{ID: "a", Kind: periods.All, Encode: func(ctx context.Context, s statistics.Snapshot, w io.Writer) error {
		sequence := calls.Add(1)
		if _, err := db.Writer.Exec(`UPDATE clients SET display_name='changed' WHERE id=1`); err != nil {
			return err
		}
		var busy, log, done int
		if err := db.Writer.QueryRow(`PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &done); err != nil {
			return err
		}
		if busy != 0 {
			return errors.New("read transaction still open")
		}
		switch mode.Load() {
		case 1:
			_, _ = io.WriteString(w, "oversized")
			return nil // 即便编码器忽略写错误也不能发布。
		case 2:
			<-ctx.Done()
			return nil
		}
		_, err := fmt.Fprintf(w, "%02d", sequence)
		return err
	}}
	c, err := cache.New(r, []cache.Item{item}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = run(t, c)
	waitFor(t, func() bool { _, e := c.Current(); return e == nil })
	original, _ := c.Current()
	originalEntry, _ := original.Get("a")
	mode.Store(1)
	before := calls.Load()
	waitFor(t, func() bool { return calls.Load() > before })
	time.Sleep(40 * time.Millisecond)
	now, _ := c.Current()
	entry, _ := now.Get("a")
	if entry != originalEntry {
		t.Fatal("overflow replaced cache")
	}
	mode.Store(2)
	before = calls.Load()
	waitFor(t, func() bool { return calls.Load() > before })
	time.Sleep(45 * time.Millisecond)
	now, _ = c.Current()
	entry, _ = now.Get("a")
	if entry != originalEntry {
		t.Fatal("timeout replaced cache")
	}
}
func TestRealBoardsPublishFixedPeriodsAcrossMidnight(t *testing.T) {
	db, r := cacheFixture(t)
	var day atomic.Int32
	day.Store(1)
	opts := cache.DefaultOptions()
	opts.Interval = 40 * time.Millisecond
	opts.Now = func() time.Time { return time.Date(2027, 1, int(day.Load()), 0, 0, 1, 0, time.UTC) }
	c, err := cache.New(r, leaderboard.Items(), opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Current(); !errors.Is(err, cache.ErrUnavailable) {
		t.Fatal(err)
	}
	_, _ = run(t, c)
	waitFor(t, func() bool { _, err := c.Current(); return err == nil })
	first, _ := c.Current()
	if first.Len() != 6 {
		t.Fatal(first.Len())
	}
	yesterday, _ := first.Get("yesterday")
	week, _ := first.Get("week")
	month, _ := first.Get("month")
	year, _ := first.Get("year")
	all, _ := first.Get("all")
	if week.Start != "2026-12-21" || week.End != "2026-12-28" || month.Start != "2026-12-01" || month.End != "2027-01-01" {
		t.Fatal(week, month)
	}
	if yesterday.Start != "2026-12-31" || yesterday.End != "2027-01-01" || yesterday.GeneratedAt != "2027-01-01T00:00:01Z" || year.Start != "2027-01-01" {
		t.Fatal(yesterday, year)
	}
	var y, a map[string][]leaderboard.Entry
	if err := json.Unmarshal([]byte(yesterday.Content), &y); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(all.Content), &a); err != nil {
		t.Fatal(err)
	}
	if len(y[leaderboard.Usage]) != 1 || y[leaderboard.Usage][0].Metric != "12" || len(a[leaderboard.Overall]) != 1 {
		t.Fatal(y, a)
	}
	if _, err := db.Writer.Exec(`UPDATE clients SET disabled=1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	day.Store(2)
	waitFor(t, func() bool {
		s, e := c.Current()
		if e != nil {
			return false
		}
		x, _ := s.Get("yesterday")
		return x.Start == "2027-01-01"
	})
	second, _ := c.Current()
	freshAll, _ := second.Get("all")
	if err := json.Unmarshal([]byte(freshAll.Content), &a); err != nil {
		t.Fatal(err)
	}
	if len(a[leaderboard.Overall]) != 0 || freshAll.GeneratedAt != "2027-01-02T00:00:01Z" {
		t.Fatal(a, freshAll)
	}
	old, _ := first.Get("yesterday")
	if old != yesterday {
		t.Fatal("old snapshot changed")
	}
	day.Store(4)
	waitFor(t, func() bool {
		s, err := c.Current()
		if err != nil {
			return false
		}
		x, _ := s.Get("week")
		return x.Start == "2026-12-28" && x.End == "2027-01-04"
	})
	third, _ := c.Current()
	rotatedMonth, _ := third.Get("month")
	if rotatedMonth.Start != month.Start || rotatedMonth.End != month.End {
		t.Fatal("week rotation changed month", rotatedMonth)
	}
}

func TestRealBoardFailureKeepsWholePublishedRound(t *testing.T) {
	db, r := cacheFixture(t)
	var fail atomic.Bool
	var tick atomic.Int32
	failed := make(chan struct{}, 2)
	items := leaderboard.Items()
	last := &items[len(items)-1]
	encode := last.Encode
	last.Encode = func(ctx context.Context, s statistics.Snapshot, w io.Writer) error {
		if fail.Load() {
			select {
			case failed <- struct{}{}:
			default:
			}
			return errors.New("injected final item failure")
		}
		return encode(ctx, s, w)
	}
	opts := cache.DefaultOptions()
	opts.Interval = 60 * time.Millisecond
	opts.Now = func() time.Time { return time.Date(2026, 12, 31, 0, 0, int(tick.Add(1)), 0, time.UTC) }
	c, err := cache.New(r, items, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = run(t, c)
	waitFor(t, func() bool { _, err := c.Current(); return err == nil })
	fail.Store(true)
	receive(t, failed)
	first, _ := c.Current()
	if _, err := db.Writer.Exec(`UPDATE daily_stats SET total_tokens=99 WHERE client_id=1`); err != nil {
		t.Fatal(err)
	}
	receive(t, failed)
	still, err := c.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		a, aok := first.Get(item.ID)
		b, bok := still.Get(item.ID)
		if !aok || !bok || a != b {
			t.Fatal("partial publication", item.ID, a, b)
		}
	}
	fail.Store(false)
	waitFor(t, func() bool {
		s, e := c.Current()
		if e != nil {
			return false
		}
		entry, _ := s.Get("all")
		var boards map[string][]leaderboard.Entry
		return json.Unmarshal([]byte(entry.Content), &boards) == nil && len(boards[leaderboard.Usage]) == 1 && boards[leaderboard.Usage][0].Metric == "99"
	})
	old, _ := first.Get("all")
	var boards map[string][]leaderboard.Entry
	if err := json.Unmarshal([]byte(old.Content), &boards); err != nil || boards[leaderboard.Usage][0].Metric != "12" {
		t.Fatal(err, boards)
	}
}

func TestTimerSkipsOverlapAndOldTicksAndStops(t *testing.T) {
	_, r := cacheFixture(t)
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var active, maxActive atomic.Int32
	item := cache.Item{ID: "only", Kind: periods.All, Encode: func(ctx context.Context, s statistics.Snapshot, w io.Writer) error {
		n := active.Add(1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		defer active.Add(-1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, err := io.WriteString(w, strconv.FormatInt(s.Rows[1].TotalTokens, 10))
		return err
	}}
	interval := 100 * time.Millisecond
	c := newCache(t, r, []cache.Item{item}, interval)
	cancel, done := run(t, c)
	receive(t, started)
	if err := c.Run(context.Background()); !errors.Is(err, cache.ErrRunning) {
		t.Fatal(err)
	}
	time.Sleep(220 * time.Millisecond)
	close(release)
	select {
	case <-started:
		t.Fatal("old tick replayed immediately")
	case <-time.After(35 * time.Millisecond):
	}
	receive(t, started)
	if maxActive.Load() != 1 {
		t.Fatal("overlapping builds")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
