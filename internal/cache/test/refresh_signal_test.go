package test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/statistics"
)

func TestManagementRefreshCoalescesAndRetainsChangesDuringBuild(t *testing.T) {
	_, reader := cacheFixture(t)
	started := make(chan int64, 8)
	permits := make(chan struct{}, 8)
	var rounds, active, maxActive atomic.Int64
	var fail atomic.Bool
	item := cache.Item{ID: "round", Kind: periods.Day, Encode: func(ctx context.Context, _ statistics.Snapshot, w io.Writer) error {
		n := rounds.Add(1)
		count := active.Add(1)
		defer active.Add(-1)
		if count > maxActive.Load() {
			maxActive.Store(count)
		}
		started <- n
		select {
		case <-permits:
		case <-ctx.Done():
			return ctx.Err()
		}
		if fail.Load() {
			return errors.New("isolated fixture failure")
		}
		_, err := fmt.Fprintf(w, "%d", n)
		return err
	}}
	c := newCache(t, reader, []cache.Item{item}, time.Hour)
	_, _ = run(t, c)
	getRound := func(want int64) {
		t.Helper()
		select {
		case got := <-started:
			if got != want {
				t.Fatalf("round %d want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("management refresh did not wake worker")
		}
	}
	getRound(1)
	for range 100 {
		c.RequestRefresh()
	}
	permits <- struct{}{}
	getRound(2)
	if _, err := c.Current(); !errors.Is(err, cache.ErrUnavailable) {
		t.Fatal("superseded pre-change build was published")
	}
	// Signal while the follow-up build is in progress: it must not be discarded
	// with old timer ticks, and a burst must still produce only one later round.
	for range 100 {
		c.RequestRefresh()
	}
	permits <- struct{}{}
	getRound(3)
	if _, err := c.Current(); !errors.Is(err, cache.ErrUnavailable) {
		t.Fatal("second superseded build was published")
	}
	permits <- struct{}{}
	waitFor(t, func() bool {
		snapshot, err := c.Current()
		if err != nil {
			return false
		}
		entry, _ := snapshot.Get("round")
		return entry.Content == "3"
	})
	select {
	case n := <-started:
		t.Fatalf("uncoalesced extra round %d", n)
	case <-time.After(30 * time.Millisecond):
	}
	if maxActive.Load() != 1 {
		t.Fatal("overlapping cache builds")
	}
	fail.Store(true)
	c.RequestRefresh()
	getRound(4)
	permits <- struct{}{}
	waitFor(t, func() bool { return active.Load() == 0 })
	snapshot, _ := c.Current()
	entry, _ := snapshot.Get("round")
	if entry.Content != "3" {
		t.Fatal("failed management refresh replaced published cache")
	}
	fail.Store(false)
	c.RequestRefresh()
	getRound(5)
	permits <- struct{}{}
	waitFor(t, func() bool {
		snapshot, _ := c.Current()
		entry, _ := snapshot.Get("round")
		return entry.Content == "5"
	})
}
