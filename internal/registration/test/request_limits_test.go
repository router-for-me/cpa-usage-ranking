package test

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"
	"weak"

	"cpa-usage-ranking/internal/registration"
)

func requireLimit(t *testing.T, err error, code string, after time.Duration) {
	t.Helper()
	var limit *registration.LimitError
	if !errors.As(err, &limit) || limit.Code != code || limit.RetryAfter != after {
		t.Fatalf("limit = %v, want %s after %s", err, code, after)
	}
}

func TestRequestLimitsSourcesGlobalAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 9, 23, 59, 30, 0, time.UTC)
	limits := registration.RequestLimits{SourceRequests: 2, GlobalRequests: 3, Window: time.Minute, MaxSources: 2}
	limiter, err := registration.NewRequestLimiter(limits, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := limiter.Admit("a"); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(time.Second / 2)
	for i := 0; i < 5; i++ {
		requireLimit(t, limiter.Admit("a"), "registration_rate_limited", time.Minute-time.Second/2)
	}
	// 已超单源上限的重复请求不能耗尽其他来源的全站剩余预算。
	if err := limiter.Admit("b"); err != nil {
		t.Fatal(err)
	}
	requireLimit(t, limiter.Admit("b"), "registration_rate_limited", time.Minute-time.Second/2)
	now = now.Add(time.Minute)
	if err := limiter.Admit("c"); err != nil {
		t.Fatalf("expired state not reclaimed: %v", err)
	}
}

func TestRequestCapacityNeverEvictsActiveSources(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	limiter, err := registration.NewRequestLimiter(registration.RequestLimits{SourceRequests: 2, GlobalRequests: 100, Window: time.Minute, MaxSources: 2}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Admit("a"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	if err := limiter.Admit("b"); err != nil {
		t.Fatal(err)
	}
	requireLimit(t, limiter.Admit("c"), "registration_capacity", 50*time.Second)
	if err := limiter.Admit("a"); err != nil {
		t.Fatal(err)
	}
	requireLimit(t, limiter.Admit("a"), "registration_rate_limited", 50*time.Second)
	now = now.Add(50 * time.Second)
	if err := limiter.Admit("c"); err != nil {
		t.Fatalf("earliest window not reclaimed: %v", err)
	}
	if err := limiter.Admit("b"); err != nil {
		t.Fatal(err)
	}
	requireLimit(t, limiter.Admit("b"), "registration_rate_limited", 10*time.Second)
	// 空间又满，不逐出仍有效的 b 或 c。
	requireLimit(t, limiter.Admit("a"), "registration_capacity", 10*time.Second)
}

func TestConcurrentRequestAdmission(t *testing.T) {
	limiter, err := registration.NewRequestLimiter(registration.RequestLimits{SourceRequests: 5, GlobalRequests: 1000, Window: time.Minute, MaxSources: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.Admit("same") == nil {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 5 {
		t.Fatalf("admitted %d", successes.Load())
	}
}

func TestRequestRetryAfterCoversOverlappingWindows(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	limiter, err := registration.NewRequestLimiter(registration.RequestLimits{SourceRequests: 1, GlobalRequests: 2, Window: time.Minute, MaxSources: 3}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := limiter.Admit("a"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(50 * time.Second)
	if err := limiter.Admit("b"); err != nil {
		t.Fatal(err)
	}
	// 全站窗口仅剩 10s，但 b 的来源窗口还剩 60s，Retry-After 须覆盖两者较晚时间。
	requireLimit(t, limiter.Admit("b"), "registration_rate_limited", time.Minute)
	now = now.Add(10 * time.Second)
	// 全站新窗口为 [60,120)，b 仍在 [50,110) 的有效来源窗口，且不消耗全站新配额。
	requireLimit(t, limiter.Admit("b"), "registration_rate_limited", 50*time.Second)
	if err := limiter.Admit("a"); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Admit("c"); err != nil {
		t.Fatal(err)
	}
	requireLimit(t, limiter.Admit("b"), "registration_rate_limited", time.Minute)
}

func TestRequestLimitValidationAndRetryAfter(t *testing.T) {
	for _, limits := range []registration.RequestLimits{{}, {SourceRequests: 1, GlobalRequests: 1, Window: time.Second}, {SourceRequests: 1, GlobalRequests: 1, Window: -time.Second, MaxSources: 1}} {
		if _, err := registration.NewRequestLimiter(limits, nil); err == nil {
			t.Fatal("unbounded request limits accepted")
		}
	}
	for _, tc := range []struct {
		duration time.Duration
		seconds  int64
	}{{0, 1}, {time.Nanosecond, 1}, {time.Second, 1}, {time.Second + time.Nanosecond, 2}} {
		if got := (&registration.LimitError{RetryAfter: tc.duration}).RetryAfterSeconds(); got != tc.seconds {
			t.Fatalf("Retry-After %d, want %d", got, tc.seconds)
		}
	}
}

func TestRequestLimiterDetachesSubstringBackingBuffer(t *testing.T) {
	limiter, err := registration.NewRequestLimiter(registration.RequestLimits{SourceRequests: 2, GlobalRequests: 10, Window: time.Minute, MaxSources: 10}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var ptr weak.Pointer[byte]
	func() {
		raw := make([]byte, 1<<20)
		copy(raw, "short-request-id-key")
		ptr = weak.Make(&raw[0])
		sub := unsafe.String(&raw[0], len(raw))[:20]
		if err := limiter.Admit(sub); err != nil {
			t.Fatal(err)
		}
	}()
	for range 4 {
		runtime.GC()
		if ptr.Value() == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if ptr.Value() != nil {
		t.Fatal("limiter retained caller URI backing buffer")
	}
	// Cloned key continues tracking subsequent requests accurately.
	if err := limiter.Admit(strings.Repeat("short-request-id-key", 1)); err != nil {
		t.Fatal(err)
	}
	if err := limiter.Admit("short-request-id-key"); err == nil {
		t.Fatal("expected third request on cloned key to be rate-limited")
	}
}
