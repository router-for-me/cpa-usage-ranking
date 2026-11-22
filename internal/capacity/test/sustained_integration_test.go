package test

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/capacity"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

func totalSustainedMetrics(t *testing.T, db *sql.DB) reports.Metrics {
	t.Helper()
	const cols = `total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples`
	q := `SELECT COALESCE(SUM(total_tokens),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),COALESCE(SUM(cache_read_tokens),0),COALESCE(SUM(cache_write_tokens),0),COALESCE(SUM(reasoning_tokens),0),COALESCE(SUM(request_count),0),COALESCE(SUM(success_count),0),COALESCE(SUM(ttft_sum_ms),0),COALESCE(SUM(ttft_samples),0),COALESCE(SUM(latency_sum_ms),0),COALESCE(SUM(latency_samples),0) FROM (SELECT ` + cols + ` FROM daily_stats WHERE model_id=0 UNION ALL SELECT ` + cols + ` FROM period_stats WHERE model_id=0 AND kind='all')`
	var v reports.Metrics
	if err := db.QueryRow(q).Scan(&v.TotalTokens, &v.InputTokens, &v.OutputTokens, &v.CacheReadTokens, &v.CacheWriteTokens, &v.ReasoningTokens, &v.RequestCount, &v.SuccessCount, &v.TTFTSumMS, &v.TTFTSamples, &v.LatencySumMS, &v.LatencySamples); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSustainedActualHTTPAndSQLiteConservation(t *testing.T) {
	for _, suffix := range []string{".jsonl", ".jsonl.gz"} {
		t.Run(suffix, func(t *testing.T) { sustainedActualHTTPAndSQLiteConservation(t, suffix) })
	}
}

func sustainedActualHTTPAndSQLiteConservation(t *testing.T, suffix string) {
	ctx := context.Background()
	catalogPath, names := sustainedCatalog(t)
	day := time.Now().UTC().Format(time.DateOnly)
	const stock = 20
	const seed uint64 = 1
	path := filepath.Join(t.TempDir(), "actual.sqlite")
	f, err := capacity.SeedSustained(ctx, path, catalogPath, stock, 90, stock, day, seed)
	if err != nil {
		t.Fatal(err)
	}
	seedCopy := filepath.Join(t.TempDir(), "seed.sqlite")
	original, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	copyFile, err := os.OpenFile(seedCopy, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(copyFile, original); err != nil {
		t.Fatal(err)
	}
	if err = copyFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err = original.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	catalog, err := models.Load(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := models.Register(ctx, db.Writer, catalog)
	if err != nil {
		t.Fatal(err)
	}
	before := totalSustainedMetrics(t, db.Reader)
	repo := clients.NewRepository(db)
	limits := reports.DefaultIngestionLimits()
	limits.GlobalInterval, limits.GlobalBurst = 10*time.Microsecond, 100000
	limits.ClientInterval, limits.ClientBurst = 100*time.Microsecond, 1000
	admission, err := reports.NewAdmission(limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := reports.NewStore(db.Writer, registry, nil)
	writer, err := reports.NewWriter(store, reports.DefaultWriterOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close(ctx)
	router := httpserver.NewRouter(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err = httpserver.MountReports(router, auth.NewAuthenticator(repo), admission, writer, int64(limits.MaxBodyBytes)); err != nil {
		t.Fatal(err)
	}
	// Hold real report requests on the first process so cancellation retains
	// a nonempty logical debt.
	var blockReports atomic.Bool
	blockReports.Store(true)
	blocked := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && blockReports.Load() {
			select {
			case <-r.Context().Done():
			case <-blocked:
			}
			return
		}
		router.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer release()
	journal := filepath.Join(t.TempDir(), "actual"+suffix)
	opts := capacity.SustainedOptions{URL: server.URL, Journal: journal, Models: names, ModelSeed: seed, ArrivalSeed: 41, MaxPending: 1000, MaxInFlight: 20, MaxAttempts: 30, MaxJournalBytes: 16 << 20, RequestTimeout: 5 * time.Second, MaxArrivalLag: 5 * time.Second, MaxActive: time.Minute}
	commands := make(chan capacity.SustainedCommand, 4)
	samples := make(chan capacity.SustainedSample, 16)
	type end struct {
		result capacity.SustainedResult
		err    error
	}
	done := make(chan end, 1)
	loadCtx, cancelLoad := context.WithCancel(ctx)
	go func() { r, e := capacity.RunSustained(loadCtx, f, opts, commands, samples); done <- end{r, e} }()
	commands <- capacity.SustainedCommand{Operation: "stage", Stage: "local", Rate: 20}
	deadline := time.After(15 * time.Second)
	var last capacity.SustainedSample
	for last.DebtRemaining != 0 || last.Produced < 100 || last.Pending < 100 {
		select {
		case last = <-samples:
		case e := <-done:
			t.Fatalf("first local driver exited: %+v %v", e.result, e.err)
		case <-deadline:
			t.Fatalf("first local run did not retain debt: %+v", last)
		}
	}
	cancelLoad()
	first := <-done
	if first.err != context.Canceled || first.result.Sample.Pending < 100 || first.result.Sample.InFlight != 0 {
		t.Fatalf("first local stop lost debt: %+v %v", first.result, first.err)
	}
	blockReports.Store(false)
	release()
	opts.Resume = true
	commands = make(chan capacity.SustainedCommand, 4)
	samples = make(chan capacity.SustainedSample, 16)
	done = make(chan end, 1)
	go func() { r, e := capacity.RunSustained(ctx, f, opts, commands, samples); done <- end{r, e} }()
	commands <- capacity.SustainedCommand{Operation: "resume"}
	deadline = time.After(15 * time.Second)
	paused := false
	for {
		select {
		case last = <-samples:
			if !paused && last.DebtRemaining == 0 && last.Produced >= 100 && last.Pending == 0 {
				commands <- capacity.SustainedCommand{Operation: "pause"}
				paused = true
				continue
			}
			if paused && last.TargetRate == 0 {
				if last.Pending == 0 {
					goto complete
				}
				commands <- capacity.SustainedCommand{Operation: "resume"}
				paused = false
			}
		case e := <-done:
			t.Fatalf("local real HTTP run failed: %+v %v", e.result, e.err)
		case <-deadline:
			t.Fatalf("local run did not settle: %+v", last)
		}
	}
complete:
	commands <- capacity.SustainedCommand{Operation: "stop"}
	finished := <-done
	if finished.err != nil || finished.result.Sample.Pending != 0 {
		t.Fatalf("invalid local result: %+v %v", finished.result, finished.err)
	}

	initial, err := capacity.NewSustainedModel(day, names, stock, seed)
	if err != nil {
		t.Fatal(err)
	}
	today, _ := time.Parse(time.DateOnly, day)
	yesterday := today.AddDate(0, 0, -1).Format(time.DateOnly)
	type key struct {
		id  int
		day string
	}
	want := map[key]json.RawMessage{}
	for id := 1; id <= stock; id++ {
		for _, d := range []string{day, yesterday} {
			body, e := initial.Initial(id, d)
			if e != nil {
				t.Fatal(e)
			}
			want[key{id, d}] = body
		}
	}
	produced := map[int64]capacity.SustainedTask{}
	confirmed := map[int64]bool{}
	expected := before
	file, err := os.Open(journal)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var input io.Reader = file
	if suffix == ".jsonl.gz" {
		z, e := gzip.NewReader(file)
		if e != nil {
			t.Fatal(e)
		}
		defer z.Close()
		input = z
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var rec struct {
			Kind, Action string
			Task         *capacity.SustainedTask
			Sequence     int64
			Client       int
			Confirmed    bool
		}
		if err = json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		if rec.Kind == "created" {
			produced[rec.Task.Sequence] = *rec.Task
			for _, d := range rec.Task.Deltas {
				expected.TotalTokens += d.TotalTokens
				expected.InputTokens += d.InputTokens
				expected.OutputTokens += d.OutputTokens
				expected.CacheReadTokens += d.CacheReadTokens
				expected.CacheWriteTokens += d.CacheWriteTokens
				expected.ReasoningTokens += d.ReasoningTokens
				expected.RequestCount += d.RequestCount
				expected.SuccessCount += d.SuccessCount
				expected.TTFTSamples += d.TTFTSamples
				expected.TTFTSumMS += float64(d.TTFTAvgMS * d.TTFTSamples)
				expected.LatencySamples += d.LatencySamples
				expected.LatencySumMS += float64(d.LatencyAvgMS * d.LatencySamples)
			}
		}
		if rec.Kind == "finish" && rec.Confirmed {
			if rec.Action != "report" {
				t.Fatal("unexpected non-report HTTP attempt")
			}
			if rec.Action == "report" {
				task, ok := produced[rec.Sequence]
				if !ok || confirmed[rec.Sequence] {
					t.Fatal("missing or duplicate confirmed task")
				}
				if task.Client < 1 || task.Client > stock {
					t.Fatal("report used an unseeded synthetic seat")
				}
				confirmed[rec.Sequence] = true
				want[key{task.Client, task.Day}] = task.Body
			}
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(produced) != len(confirmed) || len(produced) != int(finished.result.Sample.Produced) {
		t.Fatalf("not all logical updates confirmed: %d/%d", len(confirmed), len(produced))
	}
	// Real closed-day settlement must only move metrics, not alter their sum.
	closed := today.AddDate(0, 0, -2).Format(time.DateOnly)
	for id := 1; id <= stock; id++ {
		ok, e := reports.Finalize(ctx, db.Writer, closed, int64(id))
		if e != nil || !ok {
			t.Fatalf("finalize %d: %v %v", id, ok, e)
		}
	}
	if actual := totalSustainedMetrics(t, db.Reader); !reflect.DeepEqual(actual, expected) {
		t.Fatalf("12-metric conservation: got=%+v want=%+v", actual, expected)
	}
	var count int
	if err = db.Reader.QueryRow("SELECT count(*) FROM daily_reports").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(want) {
		t.Fatalf("unexpected open-day version count: %d != %d", count, len(want))
	}
	for k, body := range want {
		id := k.id
		r, e := reports.Decode(body)
		if e != nil {
			t.Fatal(e)
		}
		p, e := reports.Prepare(r)
		if e != nil {
			t.Fatal(e)
		}
		var revision int64
		var digest []byte
		if e = db.Reader.QueryRow("SELECT revision,digest FROM daily_writer_versions WHERE writer_id=? AND day=?", id, k.day).Scan(&revision, &digest); e != nil {
			t.Fatal(e)
		}
		wantDigest := p.Digest()
		if revision != r.Revision || !reflect.DeepEqual(digest, wantDigest[:]) {
			t.Fatalf("version/digest mismatch for %v", k)
		}
		assertSustainedRows(t, db.Reader, registry, int(id), k.day, body)
	}
	if _, err = db.Writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	verifyOptions := capacity.SustainedVerifyOptions{SeedDB: seedCopy, FinalDB: path, Journal: journal, ExpectedDB: filepath.Join(t.TempDir(), "expected.sqlite"), Models: names, ModelSeed: seed}
	verified, err := capacity.VerifySustained(ctx, f, verifyOptions)
	if err != nil {
		t.Fatalf("independent full verification: %v", err)
	}
	if verified.Produced != int64(len(produced)) || verified.Confirmed != verified.Produced || verified.Pending != 0 || verified.CheckedDays != int64(len(want)) || verified.CheckedPeriodRows == 0 || verified.UnconfirmedCommits != 0 {
		t.Fatalf("verification result mismatch: %+v", verified)
	}
	stats, e := capacity.SummarizeSustained(journal, names, 1000)
	if e != nil || stats.Produced != verified.Produced || stats.Confirmed != verified.Confirmed || stats.Pending != 0 || len(stats.Stages) != 1 {
		t.Fatalf("streaming statistics mismatch: %+v %v", stats, e)
	}
	stage := stats.Stages[0]
	if stage.Stage != "local" || stage.DebtTasks != 100 || stage.TouchedClients == 0 || stage.TodayTasks+stage.YesterdayTasks != stats.Produced || stage.BodyBytes.Count != stats.Produced || stage.BodyBytes.P99 < stage.BodyBytes.P50 || stage.ConfirmDelayMS.Count != stats.Confirmed || stage.ReportAttempts < stats.Confirmed {
		t.Fatalf("streaming stage statistics mismatch: %+v", stage)
	}
	if _, e = capacity.SummarizeSustained(journal, names, 1); e == nil {
		t.Fatal("summary ignored bounded pending memory")
	}
	// A missing gzip trailer or a missing final JSONL newline is never a clean
	// ledger, even if the last complete JSON object is an end marker.
	raw, e := os.ReadFile(journal)
	if e != nil || len(raw) < 2 {
		t.Fatalf("read completed journal: %v", e)
	}
	truncated := filepath.Join(t.TempDir(), "truncated"+suffix)
	if e = os.WriteFile(truncated, raw[:len(raw)-1], 0600); e != nil {
		t.Fatal(e)
	}
	verifyOptions.Journal = truncated
	if _, e = capacity.SummarizeSustained(truncated, names, 1000); e == nil {
		t.Fatal("summary accepted a truncated journal")
	}
	verifyOptions.ExpectedDB = filepath.Join(t.TempDir(), "truncated-expected.sqlite")
	if _, e = capacity.VerifySustained(ctx, f, verifyOptions); e == nil {
		t.Fatal("verification accepted a truncated journal")
	}
	badOpts := opts
	badOpts.Journal = truncated
	badOpts.Resume = true
	if _, e = capacity.RunSustained(ctx, f, badOpts, make(chan capacity.SustainedCommand), nil); e == nil {
		t.Fatal("resume accepted a truncated journal")
	}
	verifyOptions.Journal = journal
	if _, err = db.Writer.ExecContext(ctx, "UPDATE daily_stats SET total_tokens=total_tokens+1 WHERE client_id=1 AND day=? AND model_id=0", day); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	verifyOptions.ExpectedDB = filepath.Join(t.TempDir(), "tampered-expected.sqlite")
	if _, err = capacity.VerifySustained(ctx, f, verifyOptions); err == nil {
		t.Fatal("full verification failed to detect corrupted daily total")
	}
}
