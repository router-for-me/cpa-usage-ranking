package test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/leaderboard"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/readiness"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
	"cpa-usage-ranking/internal/storage"
)

const (
	lifecycleFirst      = `{"schema_version":1,"revision":"1","items":[{"model":"alpha","total_tokens":"80","input_tokens":"60","output_tokens":"20","cache_read_tokens":"20","cache_write_tokens":"0","reasoning_tokens":"0","request_count":"2","success_count":"2","ttft_avg_ms":100,"ttft_samples":"2","latency_avg_ms":null,"latency_samples":"0"},{"model":"beta","total_tokens":"40","input_tokens":"30","output_tokens":"10","cache_read_tokens":"10","cache_write_tokens":"0","reasoning_tokens":"0","request_count":"1","success_count":"0","ttft_avg_ms":null,"ttft_samples":"0","latency_avg_ms":null,"latency_samples":"0"}]}`
	lifecycleCorrection = `{"schema_version":1,"revision":"2","items":[{"model":"alpha","total_tokens":"60","input_tokens":"40","output_tokens":"20","cache_read_tokens":"10","cache_write_tokens":"0","reasoning_tokens":"0","request_count":"2","success_count":"1","ttft_avg_ms":100,"ttft_samples":"2","latency_avg_ms":null,"latency_samples":"0"}]}`
	lifecycleNext       = `{"schema_version":1,"revision":"1","items":[{"model":"alpha","total_tokens":"40","input_tokens":"30","output_tokens":"10","cache_read_tokens":"15","cache_write_tokens":"0","reasoning_tokens":"0","request_count":"2","success_count":"2","ttft_avg_ms":50,"ttft_samples":"2","latency_avg_ms":null,"latency_samples":"0"}]}`
	lifecycleOtherFirst = `{"schema_version":1,"revision":"1","items":[{"model":"beta","total_tokens":"25","input_tokens":"20","output_tokens":"5","cache_read_tokens":"0","cache_write_tokens":"0","reasoning_tokens":"0","request_count":"1","success_count":"1","ttft_avg_ms":null,"ttft_samples":"0","latency_avg_ms":null,"latency_samples":"0"}]}`
	lifecycleOtherNext  = `{"schema_version":1,"revision":"1","items":[{"model":"beta","total_tokens":"10","input_tokens":"10","output_tokens":"0","cache_read_tokens":"0","cache_write_tokens":"0","reasoning_tokens":"0","request_count":"1","success_count":"1","ttft_avg_ms":null,"ttft_samples":"0","latency_avg_ms":null,"latency_samples":"0"}]}`
)

type lifecycleRuntime struct {
	*reportFixture
	cache       *cache.Cache
	cacheCancel context.CancelFunc
	cacheDone   <-chan error
	closed      bool
}

// 每次启动重新打开同一文件并组装真实组件；清理顺序与服务关停一致。
func startLifecycle(t *testing.T, dbPath, catalogPath string, clock *atomic.Int64, registrationEnabled bool) *lifecycleRuntime {
	t.Helper()
	f := &lifecycleRuntime{reportFixture: &reportFixture{registrationFixture: &registrationFixture{}}}
	t.Cleanup(func() { f.close(t) })
	ctx := context.Background()
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	var err error
	f.db, err = storage.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.InitSchema(ctx, f.db.Writer); err != nil {
		t.Fatal(err)
	}
	if err := reports.AdvanceWindow(ctx, f.db.Writer, now()); err != nil {
		t.Fatal(err)
	}
	catalog, err := models.Load(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := models.Register(ctx, f.db.Writer, catalog)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = clients.NewRepository(f.db)
	avatarFiles := avatars.BuiltinFS()
	avatarCatalog, err := avatars.Load(avatarFiles)
	if err != nil {
		t.Fatal(err)
	}
	settings := defaultRegistrationSettings()
	settings.service.Enabled = registrationEnabled
	settings.service.Now = now
	f.store = reports.NewStore(f.db.Writer, registry, now)
	f.writer, err = reports.NewWriter(f.store, reports.DefaultWriterOptions())
	if err != nil {
		t.Fatal(err)
	}
	limits := generousReportLimits()
	admission, err := reports.NewAdmission(limits, now)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := statistics.NewReader(f.db.Reader, statistics.DefaultMaxCandidates, statistics.DefaultReadTimeout)
	if err != nil {
		t.Fatal(err)
	}
	options := cache.DefaultOptions()
	options.Interval, options.Now = 25*time.Millisecond, now
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.cache, err = cache.New(reader, leaderboard.Items(), options, logger)
	if err != nil {
		t.Fatal(err)
	}
	ready := readiness.New(f.db, registry, avatarCatalog, f.cache)
	f.router = httpserver.NewRouterWithReadiness(logger, ready.Ready)
	if err := f.router.SetTrustedProxies(nil); err != nil {
		t.Fatal(err)
	}
	f.mountRegistration(t, settings, avatarCatalog)
	httpserver.MountWithdraw(f.router, f.repo, f.cache.RequestRefresh)
	if err := httpserver.MountReports(f.router, auth.NewAuthenticator(f.repo), admission, f.writer, int64(limits.MaxBodyBytes)); err != nil {
		t.Fatal(err)
	}
	httpserver.MountLeaderboards(f.router, f.cache)
	f.server = httptest.NewServer(f.router)
	f.server.Client().Timeout = 5 * time.Second
	if status := f.status(t, "/readyz"); status != http.StatusServiceUnavailable {
		t.Fatalf("readiness before cache: %d", status)
	}
	if status := f.status(t, "/v1/leaderboards/usage/all"); status != http.StatusServiceUnavailable {
		t.Fatalf("leaderboard before cache: %d", status)
	}
	worker, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	f.cacheCancel, f.cacheDone = cancel, done
	go func() { done <- f.cache.Run(worker) }()
	f.waitRound(t, now())
	if status := f.status(t, "/readyz"); status != http.StatusOK {
		t.Fatalf("readiness after cache: %d", status)
	}
	return f
}

func (f *lifecycleRuntime) close(t *testing.T) {
	t.Helper()
	if f.closed {
		return
	}
	f.closed = true
	if f.server != nil {
		f.server.Close()
	}
	if f.cacheCancel != nil {
		f.cacheCancel()
		select {
		case err := <-f.cacheDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("cache shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("cache shutdown timed out")
		}
	}
	if f.writer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := f.writer.Close(ctx); err != nil {
			t.Errorf("writer shutdown: %v", err)
		}
		cancel()
	}
	if f.db != nil {
		if err := f.db.Close(); err != nil {
			t.Errorf("database shutdown: %v", err)
		}
		if f.db.Reader.Ping() == nil || f.db.Writer.Ping() == nil {
			t.Error("database connections remained open")
		}
	}
}

func (f *lifecycleRuntime) status(t *testing.T, path string) int {
	t.Helper()
	response, err := f.server.Client().Get(f.server.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	return response.StatusCode
}

// 等待计时器正常发布指定时钟的一整轮，而不是以睡眠时间推测构建已完成。
func (f *lifecycleRuntime) waitRound(t *testing.T, when time.Time) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for {
		snapshot, err := f.cache.Current()
		complete := err == nil && snapshot.Len() == 6
		for _, id := range []string{"today", "yesterday", "week", "month", "year", "all"} {
			item, ok := snapshot.Get(id)
			complete = complete && ok && item.GeneratedAt == when.Format(time.RFC3339Nano)
		}
		if complete {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("cache did not publish round at %s", when)
		case <-ticker.C:
		}
	}
}

type lifecycleVersion struct {
	revision, received int64
	digest             []byte
}

func (f *lifecycleRuntime) version(t *testing.T, day string, id int64) lifecycleVersion {
	t.Helper()
	var v lifecycleVersion
	if err := f.db.Reader.QueryRow(`SELECT revision,received_at_ms,digest FROM daily_writer_versions JOIN daily_reports USING(day,client_id) WHERE day=? AND client_id=?`, day, id).Scan(&v.revision, &v.received, &v.digest); err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *lifecycleRuntime) scalar(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := f.db.Reader.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *lifecycleRuntime) finalize(t *testing.T, day string, id int64, want bool) {
	t.Helper()
	var finalized bool
	result, err := f.writer.EnqueueMaintenance(context.Background(), func(ctx context.Context) error {
		var err error
		finalized, err = reports.Finalize(ctx, f.db.Writer, day, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil || finalized != want {
			t.Fatalf("finalize %s client %d: %v, %v; want %v", day, id, finalized, err, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("settlement timed out")
	}
}

type lifecycleBoard struct {
	Board       string              `json:"board"`
	Period      string              `json:"period"`
	Start       string              `json:"start"`
	End         *string             `json:"end"`
	GeneratedAt string              `json:"generated_at"`
	Entries     []leaderboard.Entry `json:"entries"`
}

func (f *lifecycleRuntime) readBoard(t *testing.T, board, period string) lifecycleBoard {
	t.Helper()
	response, err := f.server.Client().Get(f.server.URL + "/v1/leaderboards/" + board + "/" + period)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var got lifecycleBoard
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || got.Board != board || got.Period != period {
		t.Fatalf("%s/%s: status=%d %+v", board, period, response.StatusCode, got)
	}
	return got
}

func (f *lifecycleRuntime) assertBoard(t *testing.T, board, period, start, end string, when time.Time, entries []leaderboard.Entry) {
	t.Helper()
	got := f.readBoard(t, board, period)
	if got.Start != start || got.GeneratedAt != when.Format(time.RFC3339Nano) || (end == "" && got.End != nil) || (end != "" && (got.End == nil || *got.End != end)) || !reflect.DeepEqual(got.Entries, entries) {
		t.Fatalf("%s/%s: got=%+v want start=%s end=%s generated=%s entries=%+v", board, period, got, start, end, when, entries)
	}
}

func lifecycleEntry(rank int, identity publicIdentity, metric string) leaderboard.Entry {
	return leaderboard.Entry{Rank: rank, ClientID: strconv.FormatInt(identity.ID, 10), DisplayName: identity.Name, AvatarID: strconv.FormatInt(identity.AvatarID, 10), Metric: metric}
}

func (f *lifecycleRuntime) assertCombined(t *testing.T, when time.Time, alice, bob publicIdentity) {
	t.Helper()
	// 两日来源分别为 Alice 60+40、Bob 25+10；总计行和模型明细不相加。
	usage := []leaderboard.Entry{lifecycleEntry(1, alice, "100"), lifecycleEntry(2, bob, "35")}
	for _, period := range []struct{ id, start, end string }{
		{"year", "2026-01-01", "2027-01-01"}, {"all", "0001-01-01", ""},
	} {
		f.assertBoard(t, "usage", period.id, period.start, period.end, when, usage)
	}
	f.assertBoard(t, "usage", "week", "2026-10-26", "2026-11-02", when, []leaderboard.Entry{})
	f.assertBoard(t, "usage", "month", "2026-10-01", "2026-11-01", when, []leaderboard.Entry{})
	f.assertBoard(t, "usage", "today", "2026-11-04", "2026-11-05", when, []leaderboard.Entry{})
	f.assertBoard(t, "usage", "yesterday", "2026-11-03", "2026-11-04", when, []leaderboard.Entry{lifecycleEntry(1, alice, "40"), lifecycleEntry(2, bob, "10")})
	// Alice 的缓存命中为 25/70，TTFT 为 (2*100+2*50)/4；Bob 无 TTFT 样本。
	f.assertBoard(t, "cache", "all", "0001-01-01", "", when, []leaderboard.Entry{lifecycleEntry(1, alice, "35.71"), lifecycleEntry(2, bob, "0.00")})
	f.assertBoard(t, "ttft", "all", "0001-01-01", "", when, []leaderboard.Entry{lifecycleEntry(1, alice, "75.00")})
	// 中间百分位 30/10 分，Wilson 贡献 9.02/10.27，缓存 7.14/0，TTFT 5/0。
	a, b := lifecycleEntry(1, alice, "51.16"), lifecycleEntry(2, bob, "20.27")
	cacheAlice, cacheBob, ttftAlice := "35.71", "0.00", "75.00"
	a.Details = &leaderboard.OverallDetails{TotalTokens: "100", RequestCount: "4", SuccessRate: "75.00", CacheReadRate: &cacheAlice, TTFTMeanMS: &ttftAlice}
	b.Details = &leaderboard.OverallDetails{TotalTokens: "35", RequestCount: "2", SuccessRate: "100.00", CacheReadRate: &cacheBob}
	f.assertBoard(t, "overall", "all", "0001-01-01", "", when, []leaderboard.Entry{a, b})
}

func TestCompleteIngestionLifecycle(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath, catalogPath := filepath.Join(dir, "lifecycle.db"), filepath.Join(dir, "models.json")
	if err := os.WriteFile(catalogPath, []byte(`{"currency":"USD","price_unit":"per_million_tokens","models":[{"model_name":"alpha","alias":[],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	setTime := func(when time.Time) { clock.Store(when.UnixNano()) }
	firstDay := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	setTime(firstDay)
	f := startLifecycle(t, dbPath, catalogPath, &clock, true)
	f.assertBoard(t, "usage", "today", "2026-11-02", "2026-11-03", firstDay, []leaderboard.Entry{})
	aSecret, bSecret := testSecret(1), testSecret(2)
	alice := f.register(t, aSecret, 1, "Alice")
	bob := f.register(t, bSecret, 2, "Bob")
	if err := f.repo.UpdateProfile(ctx, bob.ID, "Bob", 2); err != nil {
		t.Fatal(err)
	}
	bob.AvatarID = 2
	if alice.ID == bob.ID || alice.Name != "Alice" || alice.AvatarID != 1 || bob.Name != "Bob" {
		t.Fatalf("registration: %+v %+v", alice, bob)
	}
	expectRevision(t, f.report(t, "2026-11-02", aSecret, lifecycleFirst, nil), 200, "1")
	expectRevision(t, f.report(t, "2026-11-02", bSecret, lifecycleOtherFirst, nil), 200, "1")
	original := f.version(t, "2026-11-02", alice.ID)
	if got := f.scalar(t, `SELECT count(*) FROM daily_stats WHERE day=? AND client_id=?`, "2026-11-02", alice.ID); got != 3 {
		t.Fatalf("initial total and two details: %d", got)
	}
	firstRound := firstDay.Add(time.Second)
	setTime(firstRound)
	f.waitRound(t, firstRound)
	f.assertBoard(t, "usage", "today", "2026-11-02", "2026-11-03", firstRound, []leaderboard.Entry{lifecycleEntry(1, alice, "120"), lifecycleEntry(2, bob, "25")})
	expectRevision(t, f.report(t, "2026-11-02", aSecret, lifecycleFirst, nil), 200, "1")
	if got := f.version(t, "2026-11-02", alice.ID); !reflect.DeepEqual(got, original) {
		t.Fatalf("identical retry changed version: %+v != %+v", got, original)
	}
	expectRevision(t, f.report(t, "2026-11-02", aSecret, lifecycleCorrection, nil), 200, "2")
	expectRevision(t, f.report(t, "2026-11-02", aSecret, lifecycleFirst, nil), 409, "2")
	conflicting := strings.Replace(lifecycleCorrection, `"total_tokens":"60"`, `"total_tokens":"61"`, 1)
	expectRevision(t, f.report(t, "2026-11-02", aSecret, conflicting, nil), 409, "2")
	corrected := f.version(t, "2026-11-02", alice.ID)
	if corrected.revision != 2 || corrected.received == original.received || f.scalar(t, `SELECT count(*) FROM daily_stats WHERE day=? AND client_id=?`, "2026-11-02", alice.ID) != 2 || f.scalar(t, `SELECT total_tokens FROM daily_stats WHERE day=? AND client_id=? AND model_id=0`, "2026-11-02", alice.ID) != 60 || f.scalar(t, `SELECT total_tokens FROM daily_stats WHERE day=? AND client_id=? AND model_id<>0`, "2026-11-02", alice.ID) != 60 {
		t.Fatal("correction did not replace total and remove the omitted model")
	}
	correctedRound := firstDay.Add(2 * time.Second)
	setTime(correctedRound)
	expectRevision(t, f.report(t, "2026-11-02", aSecret, lifecycleCorrection, nil), 200, "2")
	if got := f.version(t, "2026-11-02", alice.ID); !reflect.DeepEqual(got, corrected) {
		t.Fatal("correction retry changed committed metadata")
	}
	f.waitRound(t, correctedRound)
	f.assertBoard(t, "usage", "today", "2026-11-02", "2026-11-03", correctedRound, []leaderboard.Entry{lifecycleEntry(1, alice, "60"), lifecycleEntry(2, bob, "25")})

	secondDay := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	setTime(secondDay)
	expectRevision(t, f.report(t, "2026-11-03", aSecret, lifecycleNext, nil), 200, "1")
	expectRevision(t, f.report(t, "2026-11-03", bSecret, lifecycleOtherNext, nil), 200, "1")
	nextVersion := f.version(t, "2026-11-03", alice.ID)
	f.finalize(t, "2026-11-02", alice.ID, false)
	if got := f.scalar(t, `SELECT count(*) FROM daily_reports`); got != 4 {
		t.Fatalf("pending report count: %d", got)
	}
	f.close(t)

	// 不重新注册，直接用原凭据重试未结数据；新身份注册已关闭。
	f = startLifecycle(t, dbPath, catalogPath, &clock, false)
	f.assertBoard(t, "usage", "today", "2026-11-03", "2026-11-04", secondDay, []leaderboard.Entry{lifecycleEntry(1, alice, "40"), lifecycleEntry(2, bob, "10")})
	f.assertBoard(t, "usage", "yesterday", "2026-11-02", "2026-11-03", secondDay, []leaderboard.Entry{lifecycleEntry(1, alice, "60"), lifecycleEntry(2, bob, "25")})
	expectRevision(t, f.report(t, "2026-11-02", aSecret, lifecycleCorrection, nil), 200, "2")
	expectRevision(t, f.report(t, "2026-11-03", bSecret, lifecycleOtherNext, nil), 200, "1")
	requireHTTPError(t, f.report(t, "2026-11-03", testSecret(3), lifecycleNext, nil), 401, "invalid_credential")
	if got := f.version(t, "2026-11-02", alice.ID); !reflect.DeepEqual(got, corrected) || f.scalar(t, `SELECT count(*) FROM clients`) != 2 {
		t.Fatal("restart or retry changed identity or pending version")
	}

	settlementTime := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	setTime(settlementTime)
	if err := reports.AdvanceWindow(ctx, f.db.Writer, settlementTime); err != nil {
		t.Fatal(err)
	}
	f.waitRound(t, settlementTime)
	f.assertCombined(t, settlementTime, alice, bob)
	f.finalize(t, "2026-11-02", alice.ID, true)
	f.finalize(t, "2026-11-02", bob.ID, true)
	f.finalize(t, "2026-11-02", alice.ID, false)
	if f.scalar(t, `SELECT count(*) FROM daily_reports`) != 2 || f.scalar(t, `SELECT count(*) FROM daily_stats WHERE day='2026-11-02'`) != 0 {
		t.Fatal("settlement did not remove only the closed source")
	}
	for _, participant := range []struct {
		id           int64
		closed, open int64
	}{{alice.ID, 60, 40}, {bob.ID, 25, 10}} {
		if f.scalar(t, `SELECT total_tokens FROM daily_stats WHERE day='2026-11-03' AND client_id=? AND model_id=0`, participant.id) != participant.open {
			t.Fatal("pending source changed during settlement")
		}
		for _, period := range []struct{ kind, start string }{{"day", "2026-11-02"}, {"week", "2026-11-02"}, {"month", "2026-11-01"}, {"all", "0001-01-01"}} {
			if got := f.scalar(t, `SELECT total_tokens FROM period_stats WHERE kind=? AND start_day=? AND client_id=? AND model_id=0`, period.kind, period.start, participant.id); got != participant.closed {
				t.Fatalf("settled %s client %d: %d", period.kind, participant.id, got)
			}
		}
	}
	settledRound := settlementTime.Add(time.Second)
	setTime(settledRound)
	f.waitRound(t, settledRound)
	f.assertCombined(t, settledRound, alice, bob)
	f.close(t)

	// 已结和未结同时跨重启，仍用原凭据确认未结快照。
	f = startLifecycle(t, dbPath, catalogPath, &clock, false)
	finalRound := settlementTime.Add(2 * time.Second)
	setTime(finalRound)
	expectRevision(t, f.report(t, "2026-11-03", aSecret, lifecycleNext, nil), 200, "1")
	requireHTTPError(t, f.report(t, "2026-11-02", aSecret, lifecycleCorrection, nil), 410, "expired_day")
	if got := f.version(t, "2026-11-03", alice.ID); !reflect.DeepEqual(got, nextVersion) || f.scalar(t, `SELECT count(*) FROM clients`) != 2 {
		t.Fatal("settled restart changed pending version or identity")
	}
	f.waitRound(t, finalRound)
	f.assertCombined(t, finalRound, alice, bob)
}

func TestCompleteWeekAndMonthReadSettledAndPendingReports(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "models.json")
	if err := os.WriteFile(catalogPath, []byte(`{"currency":"USD","price_unit":"per_million_tokens","models":[{"model_name":"alpha","alias":[],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	setTime := func(text string) time.Time {
		t.Helper()
		when, err := time.Parse(time.RFC3339, text)
		if err != nil {
			t.Fatal(err)
		}
		clock.Store(when.UnixNano())
		return when
	}
	setTime("2026-12-30T12:00:00Z")
	f := startLifecycle(t, filepath.Join(dir, "periods.db"), catalogPath, &clock, true)
	aSecret, bSecret := testSecret(11), testSecret(12)
	alice := f.register(t, aSecret, 1, "Alice")
	bob := f.register(t, bSecret, 2, "Bob")
	expectRevision(t, f.report(t, "2026-12-30", aSecret, lifecycleNext, nil), 200, "1")
	setTime("2026-12-31T12:00:00Z")
	expectRevision(t, f.report(t, "2026-12-31", bSecret, lifecycleOtherFirst, nil), 200, "1")
	setTime("2027-01-01T00:00:00Z")
	monthRevision := strings.Replace(lifecycleOtherFirst, `"revision":"1"`, `"revision":"2"`, 1)
	monthRevision = strings.Replace(monthRevision, `"total_tokens":"25"`, `"total_tokens":"30"`, 1)
	expectRevision(t, f.report(t, "2026-12-31", bSecret, monthRevision, nil), 200, "2")
	monthRound := setTime("2027-01-01T00:00:01Z")
	f.waitRound(t, monthRound)
	f.assertBoard(t, "usage", "month", "2026-12-01", "2027-01-01", monthRound,
		[]leaderboard.Entry{lifecycleEntry(1, alice, "40"), lifecycleEntry(2, bob, "30")})
	setTime("2027-01-04T00:00:00Z")
	expectRevision(t, f.report(t, "2027-01-03", aSecret, lifecycleCorrection, nil), 200, "2")
	expectRevision(t, f.report(t, "2027-01-04", bSecret, lifecycleFirst, nil), 200, "1")
	f.finalize(t, "2026-12-30", alice.ID, true)
	assertPeriods := func(when time.Time, weekAlice, ttft string) {
		t.Helper()
		f.assertBoard(t, "usage", "week", "2026-12-28", "2027-01-04", when,
			[]leaderboard.Entry{lifecycleEntry(1, alice, weekAlice), lifecycleEntry(2, bob, "30")})
		f.assertBoard(t, "usage", "month", "2026-12-01", "2027-01-01", when,
			[]leaderboard.Entry{lifecycleEntry(1, alice, "40"), lifecycleEntry(2, bob, "30")})
		f.assertBoard(t, "ttft", "week", "2026-12-28", "2027-01-04", when,
			[]leaderboard.Entry{lifecycleEntry(1, alice, ttft)})
	}
	round := setTime("2027-01-04T00:00:01Z")
	f.waitRound(t, round)
	assertPeriods(round, "100", "75.00")
	f.assertBoard(t, "usage", "today", "2027-01-04", "2027-01-05", round, []leaderboard.Entry{lifecycleEntry(1, bob, "120")})
	f.assertBoard(t, "usage", "yesterday", "2027-01-03", "2027-01-04", round, []leaderboard.Entry{lifecycleEntry(1, alice, "60")})

	// Monday's yesterday is still editable even though it belongs to the complete week.
	revised := strings.ReplaceAll(lifecycleCorrection, `"revision":"2"`, `"revision":"3"`)
	revised = strings.Replace(revised, `"total_tokens":"60"`, `"total_tokens":"70"`, 1)
	revised = strings.Replace(revised, `"ttft_avg_ms":100`, `"ttft_avg_ms":120`, 1)
	expectRevision(t, f.report(t, "2027-01-03", aSecret, revised, nil), 200, "3")
	round = setTime("2027-01-04T00:00:02Z")
	f.waitRound(t, round)
	assertPeriods(round, "110", "85.00")
	next := setTime("2027-01-05T00:00:00Z")
	if err := reports.AdvanceWindow(ctx, f.db.Writer, next); err != nil {
		t.Fatal(err)
	}
	f.finalize(t, "2026-12-31", bob.ID, true)
	f.finalize(t, "2027-01-03", alice.ID, true)
	round = setTime("2027-01-05T00:00:01Z")
	f.waitRound(t, round)
	assertPeriods(round, "110", "85.00")
}
