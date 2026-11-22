package test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/leaderboard"
	"cpa-usage-ranking/internal/statistics"
	"cpa-usage-ranking/internal/storage"
)

func TestLeaderboardAPICacheOnlyAndPublicWhitelist(t *testing.T) {
	ctx := context.Background()
	db, err := storage.Open(ctx, filepath.Join(t.TempDir(), "boards.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := storage.InitSchema(ctx, db.Writer); err != nil {
		t.Fatal(err)
	}
	_, err = db.Writer.Exec(`INSERT INTO clients(github_id,cpa_uuid,display_name,avatar_id,created_at_ms) VALUES(1,'550e8400-e29b-41d4-a716-446655440000','<script>plain</script>',1,0);
INSERT INTO daily_reports(day,client_id,received_at_ms) VALUES('2026-12-31',1,0);
INSERT INTO daily_stats VALUES('2026-12-31',1,0,9007199254740993,10,0,0,0,0,1,1,0,0,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := statistics.NewReader(db.Reader, 10, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	opts := cache.DefaultOptions()
	opts.Interval = time.Hour
	opts.Now = func() time.Time { return time.Date(2027, 1, 1, 0, 0, 1, 0, time.UTC) }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := cache.New(reader, leaderboard.Items(), opts, logger)
	if err != nil {
		t.Fatal(err)
	}
	router := httpserver.NewRouter(logger)
	httpserver.MountLeaderboards(router, service)
	request := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if !strings.Contains(w.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("not JSON: %s", w.Body)
		}
		return w
	}
	w := request(http.MethodGet, "/v1/leaderboards/overall/today")
	if w.Code != 503 || !strings.Contains(w.Body.String(), "leaderboard_unavailable") {
		t.Fatal(w.Code, w.Body)
	}
	for _, path := range []string{"/v1/leaderboards/other/today", "/v1/leaderboards/usage/2026-12-31", "/v1/leaderboards/usage/today?model=alpha", "/v1/leaderboards/usage/today?x=1;y=2"} {
		w = request(http.MethodGet, path)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_parameter") {
			t.Fatal(path, w.Code, w.Body)
		}
	}
	if w = request(http.MethodPost, "/v1/leaderboards/usage/today"); w.Code != 405 {
		t.Fatal(w.Code, w.Body)
	}
	worker, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- service.Run(worker) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := service.Current(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache did not initialize")
		}
		time.Sleep(time.Millisecond)
	}
	// 关闭整个隔离数据库后仍可读取，不允许 HTTP 回源。
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, period := range []string{"today", "yesterday", "week", "month", "year", "all"} {
		for _, board := range []string{"overall", "usage", "cache", "success", "ttft"} {
			w = request(http.MethodGet, "/v1/leaderboards/"+board+"/"+period)
			if w.Code != 200 {
				t.Fatal(period, board, w.Code, w.Body)
			}
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &obj); err != nil {
				t.Fatal(err)
			}
			if len(obj) != 6 || obj["board"] == nil || obj["period"] == nil || obj["start"] == nil || obj["end"] == nil || obj["generated_at"] == nil || obj["entries"] == nil {
				t.Fatal(obj)
			}
			var rows []map[string]any
			if err := json.Unmarshal(obj["entries"], &rows); err != nil || rows == nil {
				t.Fatal(err, string(obj["entries"]))
			}
			for _, row := range rows {
				fields := 5
				if board == "overall" {
					fields = 6
					details, ok := row["details"].(map[string]any)
					if !ok || len(details) != 6 || details["total_tokens"] != "9007199254740993" {
						t.Fatalf("overall cached details: %+v", row)
					}
				}
				if len(row) != fields || row["rank"] == nil || row["client_id"] == nil || row["display_name"] != "<script>plain</script>" || row["avatar_id"] != "1" || row["metric"] == nil {
					t.Fatal(row)
				}
			}
			if period == "yesterday" && board == "usage" {
				if len(rows) != 1 || rows[0]["metric"] != "9007199254740993" || string(obj["start"]) != `"2026-12-31"` || string(obj["end"]) != `"2027-01-01"` || string(obj["generated_at"]) != `"2027-01-01T00:00:01Z"` {
					t.Fatal(obj, rows)
				}
			}
			if period == "today" && len(rows) != 0 {
				t.Fatal(rows)
			}
			if period == "all" && string(obj["end"]) != "null" {
				t.Fatal(string(obj["end"]))
			}
		}
	}
}
