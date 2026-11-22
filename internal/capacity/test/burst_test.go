package test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/capacity"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

func burstOptions(t *testing.T, server string, n int) capacity.BurstOptions {
	t.Helper()
	d := t.TempDir()
	return capacity.BurstOptions{URL: server, Ledger: filepath.Join(d, "attempts.jsonl"), Events: filepath.Join(d, "events.jsonl"), Samples: filepath.Join(d, "samples.jsonl"), Duration: 600 * time.Millisecond, Cycle: 5 * time.Second, RequestTimeout: 2 * time.Second, RetrySpread: 0, MaxRetryDelay: 50 * time.Millisecond, MaxAttempts: 5, QueuePerClient: 16, MaxPending: 4 * n, Changed: 2}
}

func jsonLines[T any](t *testing.T, path string) []T {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var rows []T
	s := bufio.NewScanner(file)
	for s.Scan() {
		var row T
		if err := json.Unmarshal(s.Bytes(), &row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestBurstBarrierRetrySerialDatesAndActualInFlight(t *testing.T) {
	f, _, _ := fixture(t, 20, 0)
	ids := map[string]int{}
	for i := 1; i <= f.Clients; i++ {
		ids["Bearer "+f.Secret(i)] = i
	}
	var mu sync.Mutex
	active := map[int]int{}
	todayOK := map[int]bool{}
	versions := map[string]int64{}
	bodies := map[string]string{}
	var failures []string
	var initial atomic.Int64
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ids[r.Header.Get("Authorization")]
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Revision string `json:"revision"`
		}
		_ = json.Unmarshal(body, &payload)
		rev, _ := strconv.ParseInt(payload.Revision, 10, 64)
		day := strings.TrimPrefix(r.URL.Path, "/v1/clients/report/")
		key := fmt.Sprintf("%d/%s", id, day)
		mu.Lock()
		active[id]++
		if active[id] != 1 {
			failures = append(failures, fmt.Sprintf("identity %d overlapping", id))
		}
		if day != f.Day && !todayOK[id] {
			failures = append(failures, fmt.Sprintf("yesterday before today ACK for %d", id))
		}
		if rev < versions[key] {
			failures = append(failures, "revision decreased")
		}
		if rev > versions[key] {
			versions[key] = rev
		}
		task := fmt.Sprintf("%s/%d", key, rev)
		if b, ok := bodies[task]; ok && b != string(body) {
			failures = append(failures, "retry body changed")
		}
		bodies[task] = string(body)
		mu.Unlock()
		defer func() { mu.Lock(); active[id]--; mu.Unlock() }()
		if day == f.Day && rev == 2 {
			if initial.Add(1) == int64(f.Clients) {
				close(gate)
			}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if id == 1 && day == f.Day && rev == 2 { // only the first attempt is rejected
			mu.Lock()
			first := !todayOK[-1]
			todayOK[-1] = true
			mu.Unlock()
			if first {
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(429)
				return
			}
		}
		if day == f.Day && rev == 2 {
			mu.Lock()
			todayOK[id] = true
			mu.Unlock()
		}
		fmt.Fprintf(w, `{"revision":%q}`, payload.Revision)
	}))
	defer server.Close()
	o := burstOptions(t, server.URL, 20)
	s, err := capacity.Burst(context.Background(), f, o)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(failures) > 0 {
		t.Fatal(failures)
	}
	if s.InitialFirstAttemptsWritten != 20 || s.InitialWriteSpanMS == nil || s.WrittenUnfinishedPeak < 20 || s.InitialTodayConfirmed != 20 {
		t.Fatalf("not a measured barrier/in-flight wave: %+v", s)
	}
	if s.Status[429] != 1 || s.Retries < 1 || s.BadACK != 0 || s.LateTraceCallbacks != 0 {
		t.Fatalf("retry/trace: %+v", s)
	}
	attempts := jsonLines[capacity.Receipt](t, o.Ledger)
	var first, second *capacity.Receipt
	for i := range attempts {
		r := &attempts[i]
		if r.ID == 1 && r.Day == f.Day && r.Revision == 2 {
			if r.Attempt == 1 {
				first = r
			}
			if r.Attempt == 2 {
				second = r
			}
		}
	}
	if first == nil || second == nil || first.Status != 429 || !second.Confirmed || first.TaskID != second.TaskID || first.Changed != 2 || first.WroteUnixNS == 0 || second.WroteUnixNS == 0 {
		t.Fatalf("retry lost its logical identity or timestamps: %v %v", first, second)
	}
	yest := 0
	for k := range bodies {
		if strings.Contains(k, "2026-11-11/2") {
			yest++
		}
	}
	if yest != 2 {
		t.Fatalf("expected two initial yesterday reports, got %d", yest)
	}
}

func TestBurstIndependentInputCancellationAndBacklog(t *testing.T) {
	f, _, _ := fixture(t, 10, 0)
	block := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+f.Secret(1) {
			select {
			case <-block:
			case <-r.Context().Done():
				return
			}
		}
		var x struct {
			Revision string `json:"revision"`
		}
		_ = json.NewDecoder(r.Body).Decode(&x)
		fmt.Fprintf(w, `{"revision":%q}`, x.Revision)
	}))
	defer server.Close()
	defer close(block)
	o := burstOptions(t, server.URL, 10)
	o.Duration = 800 * time.Millisecond
	o.Cycle = 200 * time.Millisecond
	o.QueuePerClient = 32
	o.MaxPending = 100
	ctx, cancel := context.WithTimeout(context.Background(), 650*time.Millisecond)
	defer cancel()
	s, err := capacity.Burst(ctx, f, o)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancellation: %v", err)
	}
	if s.BackgroundProduced < 20 || s.Pending < 3 || s.WrittenUnfinishedPeak < 1 || s.WrittenUnfinished != 0 || s.ConnectionWaiting != 0 {
		t.Fatalf("independent input/pending/cancellation: %+v", s)
	}
	type event struct {
		Type string `json:"type"`
		Task *struct {
			ID     int    `json:"id"`
			Source string `json:"source"`
		} `json:"task"`
	}
	events := jsonLines[event](t, o.Events)
	created, blockedCreated := 0, 0
	for _, e := range events {
		if e.Type == "created" {
			created++
			if e.Task.ID == 1 && e.Task.Source == "background" {
				blockedCreated++
			}
		}
	}
	if int64(created) != s.Produced || blockedCreated < 2 {
		t.Fatalf("created=%d blocked-created=%d summary=%+v", created, blockedCreated, s)
	}
	attempts := jsonLines[capacity.Receipt](t, o.Ledger)
	var confirmed int64
	for _, r := range attempts {
		if r.Confirmed {
			confirmed++
		}
	}
	if confirmed != s.Confirmed || s.Pending != s.Produced-confirmed {
		t.Fatalf("backlog does not reconcile: %+v confirmed=%d", s, confirmed)
	}
}

func TestBurstRetainsAndRetries503WithSameSnapshot(t *testing.T) {
	f, _, _ := fixture(t, 1, 0)
	var count atomic.Int64
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		if count.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"revision":"2"}`)
	}))
	defer server.Close()
	o := burstOptions(t, server.URL, 1)
	o.Cycle = time.Hour
	o.Duration = 400 * time.Millisecond
	s, err := capacity.Burst(context.Background(), f, o)
	if err != nil || s.Confirmed != 1 || s.Pending != 0 || s.Status[503] != 1 || s.Retries != 1 {
		t.Fatalf("503 task was not retained and confirmed: %+v %v", s, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 || bodies[0] != bodies[1] {
		t.Fatalf("503 retry changed snapshot: %d bodies", len(bodies))
	}
	attempts := jsonLines[capacity.Receipt](t, o.Ledger)
	if len(attempts) != 2 || attempts[0].TaskID != attempts[1].TaskID || attempts[0].Revision != attempts[1].Revision || attempts[1].Attempt != 2 {
		t.Fatalf("503 retry changed logical task/version: %+v", attempts)
	}
}

func TestBurstBoundedRetryAndQueueOverflowAreExplicit(t *testing.T) {
	f, _, _ := fixture(t, 1, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "0"); w.WriteHeader(429) }))
	defer server.Close()
	o := burstOptions(t, server.URL, 1)
	o.Duration = time.Second
	o.MaxAttempts = 2
	o.Cycle = time.Hour
	s, err := capacity.Burst(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "retry limit") || s.Pending != 1 || s.Confirmed != 0 || s.Status[429] != 2 {
		t.Fatalf("retry bound: %+v %v", s, err)
	}
	attempts := jsonLines[capacity.Receipt](t, o.Ledger)
	if len(attempts) != 2 || attempts[0].TaskID != attempts[1].TaskID || attempts[1].Attempt != 2 {
		t.Fatalf("lost retry: %+v", attempts)
	}

	release := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)
	o = burstOptions(t, blocked.URL, 1)
	o.Cycle = 100 * time.Millisecond
	o.Duration = time.Second
	o.QueuePerClient = 2
	o.MaxPending = 10
	s, err = capacity.Burst(context.Background(), f, o)
	if err == nil || !strings.Contains(err.Error(), "queue limit") || s.Pending < 4 || s.BackgroundProduced < 3 {
		t.Fatalf("queue bound: %+v %v", s, err)
	}
	type event struct {
		Type string `json:"type"`
	}
	events := jsonLines[event](t, o.Events)
	n := 0
	for _, e := range events {
		if e.Type == "created" {
			n++
		}
	}
	if int64(n) != s.Produced {
		t.Fatalf("overflow task silently lost: events=%d summary=%+v", n, s)
	}
}

func TestBurstTransportErrorIsCategorizedWithoutCredentials(t *testing.T) {
	f, _, _ := fixture(t, 1, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer server.Close()
	o := burstOptions(t, server.URL, 1)
	o.Cycle = time.Hour
	o.Duration = 120 * time.Millisecond
	o.MaxAttempts = 2
	_, _ = capacity.Burst(context.Background(), f, o)
	attempts := jsonLines[capacity.Receipt](t, o.Ledger)
	if len(attempts) == 0 || attempts[0].Status != 0 || attempts[0].Error != "transport" || attempts[0].ErrorDetail == "" {
		t.Fatalf("missing transport category: %+v", attempts)
	}
	data, err := os.ReadFile(o.Ledger)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), f.Secret(1)) || strings.Contains(string(data), f.Seed) {
		t.Fatal("private fixture material leaked into ledger")
	}
}

func TestVerifySameRevisionRetriesDoNotMaskImproperCommit(t *testing.T) {
	ctx := context.Background()
	f, path, catalogPath := fixture(t, 1, 0)
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
	body, _ := f.Snapshot(2, 2)
	report, err := reports.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := reports.Prepare(report)
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.DateOnly, f.Day)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return now.Add(12 * time.Hour) })
	if _, err = store.Apply(ctx, 1, f.Day, prepared); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(t.TempDir(), "retry.jsonl")
	write := func(rs ...capacity.Receipt) {
		t.Helper()
		file, e := os.Create(ledger)
		if e != nil {
			t.Fatal(e)
		}
		enc := json.NewEncoder(file)
		for _, r := range rs {
			if e = enc.Encode(r); e != nil {
				t.Fatal(e)
			}
		}
		if e = file.Close(); e != nil {
			t.Fatal(e)
		}
	}
	first := capacity.Receipt{ID: 1, Day: f.Day, Revision: 2, Changed: 2, Status: 429, TaskID: "1/today/2", Attempt: 1}
	second := first
	second.Attempt = 2
	second.Status = 200
	second.Confirmed = true
	write(first, second)
	got, err := capacity.Verify(ctx, f, path, []string{ledger})
	if err != nil || got["rejections_mixed"] != 1 || got["rejections_checked"] != 0 || got["unconfirmed_commits"] != 0 {
		t.Fatalf("legal rejection then confirmation: %v %v", got, err)
	}
	second.Status = 503
	second.Confirmed = false
	write(first, second)
	got, err = capacity.Verify(ctx, f, path, []string{ledger})
	if err != nil || got["rejections_mixed"] != 1 || got["unconfirmed_commits"] != 1 {
		t.Fatalf("ambiguous retry: %v %v", got, err)
	}
	write(first)
	if _, err = capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("only rejected attempts improperly committed")
	}
	second.Status = 200
	second.Confirmed = true
	second.Changed = 8
	write(first, second)
	if _, err = capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("different retry body accepted")
	}
	second.Changed = 2
	second.TaskID = "different"
	write(first, second)
	if _, err = capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("different retry task accepted")
	}
	second.TaskID = first.TaskID
	second.Attempt = 3
	write(first, second)
	if _, err = capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("missing retry attempt accepted")
	}
	second.Attempt = 1
	write(first, second)
	if _, err = capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("duplicate retry attempt number accepted")
	}
}
