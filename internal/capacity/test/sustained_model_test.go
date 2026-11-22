package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"cpa-usage-ranking/internal/capacity"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

func sustainedCatalog(t *testing.T) (string, []string) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "config", "models.json")
	catalog, err := models.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, model := range catalog.List() {
		names = append(names, model.Name)
	}
	return path, names
}

type independentRow struct {
	total, input, output, read, write, reasoning, requests, successes, ttft, latency int64
	ttftAverage, latencyAverage                                                      int64
}

type independentDay struct {
	revision int64
	rows     map[string]independentRow
}

func decodeIndependent(t *testing.T, body []byte) independentDay {
	t.Helper()
	r, err := reports.DecodeLimited(body, 128)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reports.Prepare(r); err != nil {
		t.Fatal(err)
	}
	out := independentDay{revision: r.Revision, rows: make(map[string]independentRow)}
	for _, x := range r.Items {
		row := independentRow{total: x.TotalTokens, input: x.InputTokens, output: x.OutputTokens, read: x.CacheReadTokens, write: x.CacheWriteTokens, reasoning: x.ReasoningTokens, requests: x.RequestCount, successes: x.SuccessCount, ttft: x.TTFTSamples, latency: x.LatencySamples}
		if x.TTFTAvgMS != nil {
			row.ttftAverage = int64(*x.TTFTAvgMS)
		}
		if x.LatencyAvgMS != nil {
			row.latencyAverage = int64(*x.LatencyAvgMS)
		}
		out.rows[x.Model] = row
	}
	return out
}

func TestSustainedDeterministicIncrementLedger(t *testing.T) {
	_, names := sustainedCatalog(t)
	const stock, seed = 400, 1234567
	a, err := capacity.NewSustainedModel("2026-11-13", names, stock, seed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := capacity.NewSustainedModel("2026-11-13", names, stock, seed)
	if err != nil {
		t.Fatal(err)
	}
	days := map[string]independentDay{}
	yesterday := 0
	var firstBody []byte
	for i := 0; i < 2500; i++ {
		task, err := a.Next()
		if err != nil {
			t.Fatal(err)
		}
		other, err := b.Next()
		if err != nil {
			t.Fatal(err)
		}
		one, _ := json.Marshal(task)
		two, _ := json.Marshal(other)
		if !bytes.Equal(one, two) {
			t.Fatalf("seed replay mismatch at %d", i)
		}
		if i == 0 {
			firstBody = append([]byte(nil), task.Body...)
		}
		sum := sha256.Sum256(task.Body)
		if task.BodyHash != hex.EncodeToString(sum[:]) || task.Sequence != int64(i+1) || len(task.Deltas) < 1 || len(task.Deltas) > 4 {
			t.Fatalf("invalid task: %+v", task)
		}
		if task.Client < 1 || task.Client > stock {
			t.Fatal("unseeded synthetic seat")
		}
		if task.Day == "2026-11-12" {
			yesterday++
		} else if task.Day != "2026-11-13" {
			t.Fatalf("invalid day %s", task.Day)
		}
		key := fmt.Sprintf("%d/%s", task.Client, task.Day)
		prior, ok := days[key]
		if !ok {
			initial, err := a.Initial(task.Client, task.Day)
			if err != nil {
				t.Fatal(err)
			}
			prior = decodeIndependent(t, initial)
		}
		if task.Revision != prior.revision+1 {
			t.Fatalf("version did not advance by one for %s", key)
		}
		prior.revision++
		changed := make(map[string]bool)
		for _, delta := range task.Deltas {
			k := delta.Model
			if changed[k] {
				t.Fatalf("duplicate delta for %s", k)
			}
			changed[k] = true
			v := prior.rows[k]
			if v.ttft > 0 && v.ttftAverage != delta.TTFTAvgMS || v.latency > 0 && v.latencyAverage != delta.LatencyAvgMS {
				t.Fatalf("average changed for %s", k)
			}
			v.total += delta.TotalTokens
			v.input += delta.InputTokens
			v.output += delta.OutputTokens
			v.read += delta.CacheReadTokens
			v.write += delta.CacheWriteTokens
			v.reasoning += delta.ReasoningTokens
			v.requests += delta.RequestCount
			v.successes += delta.SuccessCount
			v.ttft += delta.TTFTSamples
			v.latency += delta.LatencySamples
			if v.ttft > 0 {
				v.ttftAverage = delta.TTFTAvgMS
			}
			if v.latency > 0 {
				v.latencyAverage = delta.LatencyAvgMS
			}
			prior.rows[k] = v
		}
		actual := decodeIndependent(t, task.Body)
		if len(actual.rows) > task.Limit || !reflect.DeepEqual(prior, actual) {
			t.Fatalf("delta reconstruction mismatch for %s revision %d", key, task.Revision)
		}
		days[key] = prior
	}
	if yesterday < 180 || yesterday > 320 {
		t.Fatalf("unexpected basic proportions: yesterday=%d", yesterday)
	}
	if len(firstBody) == 0 {
		t.Fatal("first body was lost")
	}
}

// Compare every stored metric independently from the JSON body; the total is
// reconstructed by arithmetic rather than by the production Aggregate helper.
func assertSustainedRows(t *testing.T, db *sql.DB, registry *models.Registry, id int, day string, body []byte) {
	t.Helper()
	r, err := reports.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[reports.Key]reports.Metrics)
	var total reports.Metrics
	add := func(a *reports.Metrics, v reports.Metrics) {
		a.TotalTokens += v.TotalTokens
		a.InputTokens += v.InputTokens
		a.OutputTokens += v.OutputTokens
		a.CacheReadTokens += v.CacheReadTokens
		a.CacheWriteTokens += v.CacheWriteTokens
		a.ReasoningTokens += v.ReasoningTokens
		a.RequestCount += v.RequestCount
		a.SuccessCount += v.SuccessCount
		a.TTFTSumMS += v.TTFTSumMS
		a.TTFTSamples += v.TTFTSamples
		a.LatencySumMS += v.LatencySumMS
		a.LatencySamples += v.LatencySamples
	}
	for _, item := range r.Items {
		resolved, err := registry.Resolve(item.Model)
		if err != nil || !resolved.Matched {
			t.Fatalf("catalog resolution: %v", err)
		}
		v := reports.Metrics{TotalTokens: item.TotalTokens, InputTokens: item.InputTokens, OutputTokens: item.OutputTokens, CacheReadTokens: item.CacheReadTokens, CacheWriteTokens: item.CacheWriteTokens, ReasoningTokens: item.ReasoningTokens, RequestCount: item.RequestCount, SuccessCount: item.SuccessCount, TTFTSamples: item.TTFTSamples, LatencySamples: item.LatencySamples}
		if item.TTFTAvgMS != nil {
			v.TTFTSumMS = *item.TTFTAvgMS * float64(item.TTFTSamples)
		}
		if item.LatencyAvgMS != nil {
			v.LatencySumMS = *item.LatencyAvgMS * float64(item.LatencySamples)
		}
		k := reports.Key{ModelID: resolved.ID}
		current := want[k]
		add(&current, v)
		want[k] = current
		add(&total, v)
	}
	if len(want) > 0 {
		want[reports.Key{}] = total
	}
	rows, err := db.Query(`SELECT model_id,total_tokens,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,request_count,success_count,ttft_sum_ms,ttft_samples,latency_sum_ms,latency_samples FROM daily_stats WHERE client_id=? AND day=?`, id, day)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := make(map[reports.Key]reports.Metrics)
	for rows.Next() {
		var k reports.Key
		var v reports.Metrics
		if err = rows.Scan(&k.ModelID, &v.TotalTokens, &v.InputTokens, &v.OutputTokens, &v.CacheReadTokens, &v.CacheWriteTokens, &v.ReasoningTokens, &v.RequestCount, &v.SuccessCount, &v.TTFTSumMS, &v.TTFTSamples, &v.LatencySumMS, &v.LatencySamples); err != nil {
			t.Fatal(err)
		}
		got[k] = v
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored 12-metric rows mismatch for %d/%s: got=%v want=%v", id, day, got, want)
	}
}

func TestSustainedSeedAndRealSQLiteApply(t *testing.T) {
	ctx := context.Background()
	catalogPath, names := sustainedCatalog(t)
	const stock, seed, day = 40, 42, "2026-11-13"
	path := filepath.Join(t.TempDir(), "synthetic.sqlite")
	f, err := capacity.SeedSustained(ctx, path, catalogPath, stock, 90, stock, day, seed)
	if err != nil {
		t.Fatal(err)
	}
	model, err := capacity.NewSustainedModel(day, names, stock, seed)
	if err != nil {
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
	for id := 1; id <= stock; id++ {
		for _, date := range []string{day, "2026-11-12"} {
			body, err := model.Initial(id, date)
			if err != nil {
				t.Fatal(err)
			}
			r, err := reports.Decode(body)
			if err != nil {
				t.Fatal(err)
			}
			p, err := reports.Prepare(r)
			if err != nil {
				t.Fatal(err)
			}
			var rev int64
			var digest []byte
			if err = db.Reader.QueryRowContext(ctx, "SELECT revision,digest FROM daily_writer_versions WHERE writer_id=? AND day=?", id, date).Scan(&rev, &digest); err != nil {
				t.Fatal(err)
			}
			want := p.Digest()
			if rev != 1 || !bytes.Equal(digest, want[:]) {
				t.Fatalf("seed version mismatch for %d/%s", id, date)
			}
			assertSustainedRows(t, db.Reader, registry, id, date, body)
		}
	}
	inspection, err := capacity.Inspect(ctx, path)
	if err != nil || inspection["closed_backlog"] != int64(stock) || inspection["daily_reports"] != int64(3*stock) {
		t.Fatalf("seed shape: %v %v", inspection, err)
	}
	if f.Clients != stock {
		t.Fatal("fixture client count mismatch")
	}
	at, _ := time.Parse(time.DateOnly, day)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return at.Add(12 * time.Hour) })
	applied := 0
	for applied < 100 {
		task, err := model.Next()
		if err != nil {
			t.Fatal(err)
		}
		r, err := reports.Decode(task.Body)
		if err != nil {
			t.Fatal(err)
		}
		p, err := reports.Prepare(r)
		if err != nil {
			t.Fatal(err)
		}
		revision, err := store.Apply(ctx, int64(task.Client), task.Day, p)
		if err != nil || revision != task.Revision {
			t.Fatalf("apply %d/%s revision %d: %v %v", task.Client, task.Day, task.Revision, revision, err)
		}
		// An identical retry is an idempotent confirmation.
		if got, err := store.Apply(ctx, int64(task.Client), task.Day, p); err != nil || got != revision {
			t.Fatalf("retry: %v %v", got, err)
		}
		var rev int64
		var digest []byte
		if err = db.Reader.QueryRowContext(ctx, "SELECT revision,digest FROM daily_writer_versions WHERE writer_id=? AND day=?", task.Client, task.Day).Scan(&rev, &digest); err != nil {
			t.Fatal(err)
		}
		want := p.Digest()
		if rev != task.Revision || !bytes.Equal(digest, want[:]) {
			t.Fatalf("stored revision/digest mismatch for %s", strconv.Itoa(task.Client))
		}
		assertSustainedRows(t, db.Reader, registry, task.Client, task.Day, task.Body)
		applied++
	}
}
