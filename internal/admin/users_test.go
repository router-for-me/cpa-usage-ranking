package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/leaderboard"
	"cpa-usage-ranking/internal/statistics"
)

func seedRankedUsers(t *testing.T, s *Service) {
	t.Helper()
	_, err := s.writer.Exec(`INSERT INTO clients(id,github_id,cpa_uuid,display_name,avatar_id,created_at_ms) VALUES(1,1,'550e8400-e29b-41d4-a716-446655440000','one',1,123),(2,2,'550e8400-e29b-41d4-a716-446655440000','two',2,456);
	INSERT INTO client_writers(id,client_id,token_hash) VALUES(1,1,randomblob(32)),(2,2,randomblob(32));
	INSERT INTO daily_reports VALUES('2026-11-17',1,789),('2026-11-18',1,789),('2026-11-17',2,789),('2026-11-18',2,789);
	INSERT INTO daily_writer_versions(day,writer_id,client_id,revision,digest)
	SELECT day,client_id,client_id,CASE day WHEN '2026-11-17' THEN 7 ELSE 8 END,zeroblob(32) FROM daily_reports;
	INSERT INTO daily_stats SELECT day,client_id,0,1500,1000,500,200,0,0,100,90,10000,100,20000,100 FROM daily_reports`)
	if err != nil {
		t.Fatal(err)
	}
}

// Synthetic settled rows let the management refresh tests cover complete
// previous periods as well as the two open daily snapshots.
func seedPreviousRankedPeriods(t *testing.T, s *Service) {
	t.Helper()
	_, err := s.writer.Exec(`INSERT INTO period_stats
		SELECT 'week','2026-11-09',0,id,1500,1000,500,200,0,0,100,90,10000,100,20000,100 FROM clients
		UNION ALL SELECT 'month','2026-10-01',0,id,1500,1000,500,200,0,0,100,90,10000,100,20000,100 FROM clients`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestBanOnlyUpdatesTargetStateAndRechecksDeletedActor(t *testing.T) {
	ctx := context.Background()
	s, _ := serviceDB(t)
	seedRankedUsers(t, s)
	var before string
	query := `SELECT hex(w.token_hash)||github_id||cpa_uuid||display_name||avatar_id||created_at_ms FROM clients c JOIN client_writers w ON c.id=w.client_id WHERE c.id=1`
	if err := s.reader.QueryRow(query).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{true, true, false, false} {
		if err := s.SetDisabled(ctx, s.owner, 1, disabled); err != nil {
			t.Fatal(err)
		}
		var state bool
		var after string
		if err := s.reader.QueryRow("SELECT disabled FROM clients WHERE id=1").Scan(&state); err != nil || state != disabled {
			t.Fatal("target state", err)
		}
		if err := s.reader.QueryRow(query).Scan(&after); err != nil || before != after {
			t.Fatal("ban changed identity", err)
		}
	}
	var revision, total, other int
	if err := s.reader.QueryRow(`SELECT r.revision,d.total_tokens,c.disabled FROM daily_writer_versions r JOIN daily_stats d USING(day,client_id) JOIN clients c ON c.id=2 WHERE r.client_id=1 AND r.day='2026-11-18'`).Scan(&revision, &total, &other); err != nil || revision != 8 || total != 1500 || other != 0 {
		t.Fatal("ban changed stats or another client", err)
	}
	account, err := s.CreateAccount(ctx, s.owner, "staff", "ordinary-test-password")
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := s.Login(ctx, "staff", "ordinary-test-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, session.Identity, 2, true); err != nil {
		t.Fatal(err)
	}
	// A real pending write waits behind the deletion transaction. Once deletion
	// commits it must recheck the account, not trust earlier HTTP authentication.
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM admin_accounts WHERE id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	beforeWait := s.writer.Stats().WaitCount
	result := make(chan error, 1)
	go func() { result <- s.SetDisabled(ctx, session.Identity, 1, true) }()
	deadline := time.Now().Add(time.Second)
	for s.writer.Stats().WaitCount == beforeWait && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.writer.Stats().WaitCount == beforeWait {
		t.Fatal("write did not queue")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, ErrSession) {
		t.Fatal("deleted actor wrote state", err)
	}
	if err := s.SetDisabled(ctx, s.owner, 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing client", err)
	}
}

func waitCacheState(t *testing.T, c *cache.Cache, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := c.Current()
		match := err == nil
		for _, period := range []string{"today", "yesterday", "week", "month", "year", "all"} {
			entry, ok := snapshot.Get(period)
			if !ok {
				match = false
				break
			}
			var boards map[string][]leaderboard.Entry
			if err := json.Unmarshal([]byte(entry.Content), &boards); err != nil {
				t.Fatal(err)
			}
			for _, board := range []string{"overall", "usage", "cache", "success", "ttft"} {
				if len(boards[board]) != count {
					match = false
				}
			}
		}
		if match {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("five boards/six periods did not reach %s candidates", strconv.Itoa(count))
}

func TestBanRefreshesAllPublicBoardsWithoutWaitingForTimer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, db := serviceDB(t)
	seedRankedUsers(t, s)
	seedPreviousRankedPeriods(t, s)
	reader, err := statistics.NewReader(db.Reader, 100, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opts := cache.DefaultOptions()
	opts.Interval = time.Hour
	opts.Now = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	c, err := cache.New(reader, leaderboard.Items(), opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitCacheState(t, c, 2)
	if err := s.SetDisabled(ctx, s.owner, 1, true); err != nil {
		t.Fatal(err)
	}
	c.RequestRefresh()
	waitCacheState(t, c, 1)
	if err := s.SetDisabled(ctx, s.owner, 1, false); err != nil {
		t.Fatal(err)
	}
	c.RequestRefresh()
	waitCacheState(t, c, 2)
}
