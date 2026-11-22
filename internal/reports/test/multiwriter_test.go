package test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/testfixture"
)

func TestConcurrentWritersReplaceAndSettleOneSnapshot(t *testing.T) {
	ctx := context.Background()
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}`)
	seat, err := testfixture.Seat(ctx, db.Writer, clients.CredentialHash{1}, "Shared", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := testfixture.Writer(ctx, db.Writer, seat.ID, clients.CredentialHash{2})
	if err != nil {
		t.Fatal(err)
	}
	const day = "2026-11-01"
	var now atomic.Int64
	now.Store(instant("2026-11-01T12:00:00Z").UnixMilli())
	store := reports.NewStore(db.Writer, registry, func() time.Time { return time.UnixMilli(now.Load()) })
	a := prepared(t, 91, usage("alpha", 40, 1, 1), usage("beta", 20, 1, 1))
	b := prepared(t, 1, usage("beta", 20, 1, 1))
	start, results := make(chan struct{}), make(chan error, 2)
	for _, x := range []struct {
		id int64
		p  reports.Prepared
	}{{seat.WriterID, a}, {second, b}} {
		go func() {
			<-start
			_, err := store.Apply(ctx, x.id, day, x.p)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if scalar(t, db.Reader, `SELECT count(*) FROM daily_reports`) != 1 ||
		scalar(t, db.Reader, `SELECT sum(revision) FROM daily_writer_versions`) != 92 {
		t.Fatal("concurrent writers did not retain both versions or wrote multiple snapshots")
	}
	total := scalar(t, db.Reader, `SELECT total_tokens FROM daily_stats WHERE model_id=0`)
	details := scalar(t, db.Reader, `SELECT sum(total_tokens) FROM daily_stats WHERE model_id>0`)
	if (total != 60 && total != 20) || details != total {
		t.Fatalf("committed stats mixed snapshots: %d %d", total, details)
	}
	if _, err := store.Apply(ctx, second, day, b); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, seat.WriterID, day, a); err != nil {
		t.Fatal(err)
	}
	if scalar(t, db.Reader, `SELECT total_tokens FROM daily_stats WHERE model_id=0`) != total {
		t.Fatal("cross-writer retry changed effective usage")
	}
	now.Store(instant("2026-11-03T12:00:00Z").UnixMilli())
	if err := reports.AdvanceWindow(ctx, db.Writer, time.UnixMilli(now.Load())); err != nil {
		t.Fatal(err)
	}
	if settled, err := reports.Finalize(ctx, db.Writer, day, seat.ID); err != nil || !settled {
		t.Fatal(settled, err)
	}
	if scalar(t, db.Reader, `SELECT total_tokens FROM period_stats WHERE kind='all' AND model_id=0`) != total ||
		scalar(t, db.Reader, `SELECT count(*) FROM daily_writer_versions`) != 0 {
		t.Fatal("settlement did not settle only the winner and clear both versions")
	}
}
