package admin

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/leaderboard"
	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
)

func TestIndependentVisibilityBanPersistenceAndNaturalRetention(t *testing.T) {
	ctx := context.Background()
	s, db := serviceDB(t)
	seedRankedUsers(t, s)
	for range 2 {
		if err := s.SetRankingHidden(ctx, s.owner, 1, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetDisabled(ctx, s.owner, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRankingHidden(ctx, s.owner, 1, false); !errors.Is(err, ErrBanned) {
		t.Fatal("restored a banned user", err)
	}
	if err := s.SetDisabled(ctx, s.owner, 1, false); err != nil {
		t.Fatal(err)
	}
	client, err := s.clients.ByID(ctx, 1)
	if err != nil || client.Disabled || !client.RankingHidden {
		t.Fatal("unban cleared independent hidden flag", err)
	}
	var count, revision, total int
	if err := db.Reader.QueryRow(`SELECT count(*),sum(revision) FROM daily_writer_versions`).Scan(&count, &revision); err != nil || count != 4 || revision != 30 {
		t.Fatal("management rewrote daily revisions", err)
	}
	if err := db.Reader.QueryRow(`SELECT sum(total_tokens) FROM daily_stats`).Scan(&total); err != nil || total != 6000 {
		t.Fatal("management rewrote usage", err)
	}
	hidden := true
	page, err := s.Users(ctx, clients.ListQuery{Hidden: &hidden})
	if err != nil || len(page.Clients) != 1 || page.Clients[0].ID != 1 {
		t.Fatal("global excluded list", err)
	}
	restarted, err := New(Owner{Username: "owner", Password: "isolated-test-password"}, db)
	if err != nil {
		t.Fatal(err)
	}
	client, err = restarted.clients.ByID(ctx, 1)
	if err != nil || !client.RankingHidden {
		t.Fatal("restart lost exclusion", err)
	}
	// Reader works without any administrator service or enabled configuration.
	reader, err := statistics.NewReader(db.Reader, 100, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-11-18", "2026-12-01", "2027-01-04"} {
		for _, kind := range []string{periods.Day, periods.Week, periods.Month, periods.Year, periods.All} {
			data, err := reader.Read(ctx, statistics.Selection{Kind: kind, Day: day, Public: true})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := data.Rows[1]; ok {
				t.Fatal("exclusion bypassed across periods")
			}
		}
	}
	if err := reports.AdvanceWindow(ctx, db.Writer, time.Date(2026, 12, 4, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-11-17", "2026-11-18"} {
		if finalized, err := reports.Finalize(ctx, db.Writer, day, 1); err != nil || !finalized {
			t.Fatal("hidden user settlement stopped", err)
		}
	}
	pruned, err := reports.Prune(ctx, db.Writer, time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), 30, periods.Day, 100)
	if err != nil || pruned != 2 {
		t.Fatal("natural retention stopped for hidden user", err)
	}
	client, err = s.clients.ByID(ctx, 1)
	if err != nil || !client.RankingHidden {
		t.Fatal("retention cleared hidden flag", err)
	}
	for range 2 {
		if err := s.SetRankingHidden(ctx, s.owner, 1, false); err != nil {
			t.Fatal(err)
		}
	}
	data, err := reader.Read(ctx, statistics.Selection{Kind: periods.All, Day: "2027-02-01", Public: true})
	if err != nil || data.Rows[1].TotalTokens != 3000 {
		t.Fatal("restoration did not use retained cumulative data", err)
	}
	if err := s.SetRankingHidden(ctx, s.owner, 999, true); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing target", err)
	}
	if err := s.SetRankingHidden(ctx, s.owner, 999, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("missing restore target", err)
	}
}

func TestExclusionRefreshesFiveBoardsSixPeriods(t *testing.T) {
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
	if err := s.SetRankingHidden(ctx, s.owner, 1, true); err != nil {
		t.Fatal(err)
	}
	c.RequestRefresh()
	waitCacheState(t, c, 1)
	if err := s.SetDisabled(ctx, s.owner, 1, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, s.owner, 1, false); err != nil {
		t.Fatal(err)
	}
	c.RequestRefresh()
	waitCacheState(t, c, 1)
	if err := s.SetRankingHidden(ctx, s.owner, 1, false); err != nil {
		t.Fatal(err)
	}
	c.RequestRefresh()
	waitCacheState(t, c, 2)
}

func TestExcludedSamplesAreFilteredBeforeCandidateLimitAndScoring(t *testing.T) {
	ctx := context.Background()
	_, db := serviceDB(t)
	_, err := db.Writer.Exec(`WITH RECURSIVE ids(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM ids WHERE n<120)
	INSERT INTO clients(id,github_id,server_id,display_name,avatar_id,ranking_hidden,created_at_ms) SELECT n,n,'550e8400-e29b-41d4-a716-446655440000','candidate-'||n,1,n>2,0 FROM ids;
	INSERT INTO daily_reports SELECT '2026-11-18',id,0 FROM clients;
	INSERT INTO daily_stats SELECT '2026-11-18',id,0,id*1000,id*1000,0,100,0,0,100,90,id*100,100,10000,100 FROM clients`)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := statistics.NewReader(db.Reader, 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	data, err := reader.Read(ctx, statistics.Selection{Kind: periods.Day, Day: "2026-11-18", Public: true})
	if err != nil || len(data.Rows) != 2 || len(data.Profiles) != 2 {
		t.Fatal("hidden candidates consumed public capacity or samples", err)
	}
	baseline, err := leaderboard.Build(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	if baseline[leaderboard.Usage][0].ClientID != "2" {
		t.Fatal("wrong candidates")
	}
	// Changing excluded values cannot affect the two eligible users' scores.
	if _, err := db.Writer.Exec(`UPDATE daily_stats SET total_tokens=900000000000,ttft_sum_ms=1 WHERE client_id>2`); err != nil {
		t.Fatal(err)
	}
	after, err := reader.Read(ctx, statistics.Selection{Kind: periods.Day, Day: "2026-11-18", Public: true})
	if err != nil {
		t.Fatal(err)
	}
	boards, err := leaderboard.Build(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	for i, entry := range baseline[leaderboard.Overall] {
		if boards[leaderboard.Overall][i].ClientID != entry.ClientID || boards[leaderboard.Overall][i].Metric != entry.Metric {
			t.Fatal("excluded samples changed scores")
		}
	}
}
