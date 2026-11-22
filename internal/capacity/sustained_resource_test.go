package capacity

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	"cpa-usage-ranking/internal/models"
)

// This is an opt-in local sizing sample, NOT a capacity benchmark. It uses
// the fixed 10k model and journal schema without network or service work.
// Run with CAPACITY_SIZE_SAMPLE=1 go test -run TestSustainedSizingSample -v.
func TestSustainedSizingSample(t *testing.T) {
	if os.Getenv("CAPACITY_SIZE_SAMPLE") != "1" {
		t.Skip("opt-in sizing experiment")
	}
	catalog, err := models.Load("../../config/models.json")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range catalog.List() {
		names = append(names, m.Name)
	}
	var m0, m1, m2, m3 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	began := time.Now()
	model, err := NewSustainedModel(time.Now().UTC().Format(time.DateOnly), names, 10000, 8675309)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	zcount := &sustainedByteCounter{out: io.Discard}
	z, err := gzip.NewWriterLevel(zcount, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	var raw, body, deltas, firstRaw, firstCompressed int64
	count := 200000
	if value := os.Getenv("CAPACITY_SIZE_TASKS"); value != "" {
		count, err = strconv.Atoi(value)
		if err != nil || count < 10000 || count > 2000000 {
			t.Fatal("CAPACITY_SIZE_TASKS must be 10000..2000000")
		}
	}
	// Bounded per-byte histogram: body limit is at most 512 KiB.
	histogram := make([]int64, 512<<10+1)
	for i := 1; i <= count; i++ {
		task, e := model.Next()
		if e != nil {
			t.Fatal(e)
		}
		body += int64(len(task.Body))
		if len(task.Body) >= len(histogram) {
			t.Fatal("sample body exceeds bounded histogram")
		}
		histogram[len(task.Body)]++
		deltas += int64(len(task.Deltas))
		now := int64(1794541534000000000) + int64(i)*10000000
		recs := []sustainedRecord{
			{Kind: "created", TimeUnixNS: now, Task: &task, Source: "arrival", PlannedUnixNS: now, ArrivalState: uint64(i)},
			{Kind: "start", TimeUnixNS: now + 100000, Sequence: task.Sequence, Client: task.Client, Action: "report", Attempt: 1},
			{Kind: "finish", TimeUnixNS: now + 200000, Sequence: task.Sequence, Client: task.Client, Action: "report", Attempt: 1, Status: 200, Confirmed: true, AckRevision: fmt.Sprint(task.Revision), GotConnUnixNS: now + 110000, WroteUnixNS: now + 120000},
		}
		for _, rec := range recs {
			data, e := json.Marshal(rec)
			if e != nil {
				t.Fatal(e)
			}
			data = append(data, '\n')
			raw += int64(len(data))
			if _, e = z.Write(data); e != nil {
				t.Fatal(e)
			}
		}
		if i%100 == 0 {
			if e = z.Flush(); e != nil {
				t.Fatal(e)
			}
		}
		if i == count/2 {
			firstRaw, firstCompressed = raw, zcount.n
		}
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&m2)
	quantile := func(numerator int64) int {
		threshold := (int64(count)*numerator + 99) / 100
		var total int64
		for n, count := range histogram {
			total += count
			if total >= threshold {
				return n
			}
		}
		return -1
	}
	// Keep 10k real queued jobs and their frozen bodies/actors alive to
	// estimate pending-memory cost. This does not simulate network workers.
	pending := newSustainedState(model, 20261113)
	for i := 0; i < 10000; i++ {
		task, e := model.Next()
		if e != nil {
			t.Fatal(e)
		}
		pending.addTask(task, "arrival", int64(i+1), int64(i+1))
	}
	runtime.GC()
	runtime.ReadMemStats(&m3)
	runtime.KeepAlive(pending)
	result := map[string]any{
		"label": "local synthetic sizing only, not performance or remote capacity",
		"stock": 10000, "tasks": count,
		"raw_bytes": raw, "gzip_bytes": zcount.n, "raw_first_half": firstRaw,
		"gzip_first_half": firstCompressed, "body_bytes": body, "delta_rows": deltas,
		"heap_baseline": m0.HeapAlloc, "heap_after_stock": m1.HeapAlloc, "heap_after_tasks": m2.HeapAlloc,
		"heap_after_10000_pending": m3.HeapAlloc, "pending_sample": 10000,
		"body_p50": quantile(50), "body_p95": quantile(95), "body_p99": quantile(99),
		"elapsed_seconds": time.Since(began).Seconds(),
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(string(encoded))
}

// Optional bounded verifier-memory experiment using a newly created 10k seed.
// It feeds independently reconstructed events one at a time; it is not a
// substitute for the full stopped-database verification integration test.
func TestSustainedVerifierSizingSample(t *testing.T) {
	if os.Getenv("CAPACITY_VERIFY_SIZE_SAMPLE") != "1" {
		t.Skip("opt-in verifier sizing experiment")
	}
	fixture, err := ReadFixture(os.Getenv("CAPACITY_SIZE_FIXTURE"))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := readDB(os.Getenv("CAPACITY_SIZE_SEED_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer seed.Close()
	catalog, err := models.Load("../../config/models.json")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, item := range catalog.List() {
		names = append(names, item.Name)
	}
	const count = 200000
	began := time.Now()
	runtime.GC()
	var m0, m1, m2 runtime.MemStats
	runtime.ReadMemStats(&m0)
	ctx := context.Background()
	state, err := newVerificationState(ctx, fixture, seed)
	if err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.ReadMemStats(&m1)
	model, err := NewSustainedModel(fixture.Day, names, fixture.Clients, 8675309)
	if err != nil {
		t.Fatal(err)
	}
	if err = state.stageRecord(sustainedRecord{TimeUnixNS: 1, Command: &SustainedCommand{Operation: "stage", Stage: "sizing-only", Rate: 100}}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= count; i++ {
		task, e := model.Next()
		if e != nil {
			t.Fatal(e)
		}
		now := int64(i*10 + 2)
		if e = state.createdTask(sustainedRecord{Task: &task, Source: "arrival", PlannedUnixNS: now, TimeUnixNS: now}); e != nil {
			t.Fatal(e)
		}
		start := sustainedRecord{Client: task.Client, Sequence: task.Sequence, Action: "report", Attempt: 1, TimeUnixNS: now + 3}
		if e = state.start(start); e != nil {
			t.Fatal(e)
		}
		start.TimeUnixNS++
		start.Confirmed, start.Status, start.AckRevision = true, 200, strconv.FormatInt(task.Revision, 10)
		if e = state.finish(start); e != nil {
			t.Fatal(e)
		}
	}
	// Measure verifier alone, without retaining the generator model.
	model = nil
	runtime.GC()
	runtime.ReadMemStats(&m2)
	runtime.KeepAlive(state)
	result := map[string]any{"label": "local verifier sizing only; no service or throughput result", "tasks": count,
		"created": state.created, "confirmed": state.confirmed, "identity_day_states": len(state.days),
		"latest_confirmed_snapshots": len(state.acks), "actors": len(state.actors),
		"heap_baseline": m0.HeapAlloc, "heap_seed_state": m1.HeapAlloc, "heap_after_tasks": m2.HeapAlloc,
		"elapsed_seconds": time.Since(began).Seconds()}
	encoded, _ := json.Marshal(result)
	t.Log(string(encoded))
}
