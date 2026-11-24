package test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/registration"
)

func TestConcurrentStartsCannotExceedOutstandingCapacity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "capacity.db")
	now := time.Date(2026, 11, 9, 23, 50, 0, 0, time.UTC)
	opts := registration.DefaultOptions()
	opts.TTL, opts.Capacity, opts.Now = 2*time.Minute, 3, func() time.Time { return now }
	db, one := open(t, path, opts)
	_, two := open(t, path, opts)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			svc := []*registration.Service{one, two}[i%2]
			if _, err := svc.Start(ctx, digest(byte(i+1)), serverID, returnTo, nil); err == nil {
				admitted.Add(1)
			} else {
				requireLimit(t, err, "registration_capacity", 2*time.Minute)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 3 {
		t.Fatalf("admitted %d starts", admitted.Load())
	}
	counts(t, db, 0, 0, 3)
	now = now.Add(opts.TTL)
	if _, err := two.Start(ctx, digest(99), serverID, returnTo, nil); err != nil {
		t.Fatalf("expired requests didn't free capacity: %v", err)
	}
	counts(t, db, 0, 0, 1)
}

func TestRegistrationDoesNotCapSuccessfulSeatsPerGitHubUser(t *testing.T) {
	ctx := context.Background()
	db, svc := open(t, filepath.Join(t.TempDir(), "registration.db"), registration.DefaultOptions())
	for i, suffix := range "123456789abc" {
		hash := digest(byte(i))
		id, err := svc.Start(ctx, hash, serverID[:35]+string(suffix), returnTo, nil)
		if err != nil {
			t.Fatal(err)
		}
		approve(t, db, id)
		if result, err := svc.Complete(ctx, hash, id); err != nil || !result.Created {
			t.Fatalf("authorized seat %d failed: %+v %v", i, result, err)
		}
	}
	counts(t, db, 12, 12, 12)
}

func TestFailedStartsDoNotAllocateRequests(t *testing.T) {
	ctx := context.Background()
	db, svc := open(t, filepath.Join(t.TempDir(), "registration.db"), registration.DefaultOptions())
	if _, err := svc.Start(ctx, digest(1), "bad", returnTo, nil); !errors.Is(err, registration.ErrInvalidRequest) {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, digest(1), serverID, returnTo, avatarPtr(999)); !errors.Is(err, registration.ErrInvalidAvatar) {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`CREATE TRIGGER reject_request BEFORE INSERT ON registration_requests BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, digest(1), serverID, returnTo, nil); err == nil {
		t.Fatal("failed request was confirmed")
	}
	counts(t, db, 0, 0, 0)
	if _, err := db.Writer.Exec(`DROP TRIGGER reject_request`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, digest(1), serverID, returnTo, nil); err != nil {
		t.Fatal(err)
	}
	counts(t, db, 0, 0, 1)
}

func TestStartReclaimsTerminalCapacityBeforeRejecting(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 9, 23, 50, 0, 0, time.UTC)
	opts := registration.DefaultOptions()
	opts.TTL, opts.Capacity, opts.Now = 10*time.Minute, 3, func() time.Time { return now }
	db, svc := open(t, filepath.Join(t.TempDir(), "reclaim.db"), opts)

	// 1. 满容量中存在 denied / failed 终态行时，优先清理异常终态并保留 completed 行。
	c1, err := svc.Start(ctx, digest(1), serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	approve(t, db, c1)
	if _, err := svc.Complete(ctx, digest(1), c1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	d1, err := svc.Start(ctx, digest(2), serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`UPDATE registration_requests SET stage='denied' WHERE id=?`, d1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	f1, err := svc.Start(ctx, digest(3), serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`UPDATE registration_requests SET stage='failed' WHERE id=?`, f1); err != nil {
		t.Fatal(err)
	}

	now = now.Add(time.Second)
	c2, err := svc.Start(ctx, digest(4), serverID, returnTo, nil)
	if err != nil {
		t.Fatalf("failed to reclaim denied/failed rows: %v", err)
	}
	// c1 尚未被淘汰，仍支持幂等查询。
	if res, err := svc.Complete(ctx, digest(1), c1); err != nil || res.Client.ID == 0 {
		t.Fatalf("completed row c1 should remain recoverable: %+v %v", res, err)
	}
	approve(t, db, c2)
	if _, err := svc.Complete(ctx, digest(4), c2); err != nil {
		t.Fatal(err)
	}

	// 2. 填满容量（c1 completed、c2 completed、p1 pending），再次 Start 仅淘汰最早完成的 c1。
	now = now.Add(time.Second)
	p1, err := svc.Start(ctx, digest(5), serverID, returnTo, nil)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	p2, err := svc.Start(ctx, digest(6), serverID, returnTo, nil)
	if err != nil {
		t.Fatalf("failed to evict oldest completed row: %v", err)
	}
	if _, err := svc.Complete(ctx, digest(1), c1); !errors.Is(err, registration.ErrExpired) {
		t.Fatalf("oldest completed c1 should have been evicted, got %v", err)
	}
	if res, err := svc.Complete(ctx, digest(4), c2); err != nil || res.Client.ID == 0 {
		t.Fatalf("newer completed c2 should still remain: %+v %v", res, err)
	}

	// 3. 再填满最后一个活跃槽位（淘汰 c2），此时 3 条均为活跃未完成请求，再申请必须返回 registration_capacity。
	now = now.Add(time.Second)
	if _, err := svc.Start(ctx, digest(7), serverID, returnTo, nil); err != nil {
		t.Fatalf("failed to evict second completed row: %v", err)
	}
	if _, err := svc.Start(ctx, digest(8), serverID, returnTo, nil); err == nil {
		t.Fatal("active capacity overflow should be rejected")
	} else {
		requireLimit(t, err, "registration_capacity", 10*time.Minute-2*time.Second)
	}
	// 确认活跃请求未被误淘汰。
	if _, err := svc.Complete(ctx, digest(5), p1); !errors.Is(err, registration.ErrPending) {
		t.Fatalf("active p1 lost: %v", err)
	}
	if _, err := svc.Complete(ctx, digest(6), p2); !errors.Is(err, registration.ErrPending) {
		t.Fatalf("active p2 lost: %v", err)
	}
}
