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
			if _, err := svc.Start(ctx, digest(byte(i+1)), serverID, returnTo); err == nil {
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
	if _, err := two.Start(ctx, digest(99), serverID, returnTo); err != nil {
		t.Fatalf("expired requests didn't free capacity: %v", err)
	}
	counts(t, db, 0, 0, 1)
}

func TestRegistrationDoesNotCapSuccessfulSeatsPerGitHubUser(t *testing.T) {
	ctx := context.Background()
	db, svc := open(t, filepath.Join(t.TempDir(), "registration.db"), registration.DefaultOptions())
	for i, suffix := range "123456789abc" {
		hash := digest(byte(i))
		id, err := svc.Start(ctx, hash, serverID[:35]+string(suffix), returnTo)
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
	if _, err := svc.Start(ctx, digest(1), "bad", returnTo); !errors.Is(err, registration.ErrInvalidRequest) {
		t.Fatal(err)
	}
	if _, err := db.Writer.Exec(`CREATE TRIGGER reject_request BEFORE INSERT ON registration_requests BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, digest(1), serverID, returnTo); err == nil {
		t.Fatal("failed request was confirmed")
	}
	counts(t, db, 0, 0, 0)
	if _, err := db.Writer.Exec(`DROP TRIGGER reject_request`); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx, digest(1), serverID, returnTo); err != nil {
		t.Fatal(err)
	}
	counts(t, db, 0, 0, 1)
}
