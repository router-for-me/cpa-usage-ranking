package test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
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
)

func sustainedFixture(t *testing.T, clients int) (capacity.Fixture, []string) {
	t.Helper()
	_, names := sustainedCatalog(t)
	return capacity.Fixture{Clients: clients, Day: time.Now().UTC().Format(time.DateOnly), HistoryDays: 90, Models: names[:8], Seed: strings.Repeat("ab", 32)}, names
}

func TestSustainedOpenInputCancelAndResume(t *testing.T) {
	f, names := sustainedFixture(t, 20)
	// Fix a seed that actually exercises new identities within the first debt.
	const seed uint64 = 1
	var release atomic.Bool
	var violation atomic.Int32
	var mu sync.Mutex
	active := map[string]int{}
	bodyHashes := map[string][32]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret := r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			violation.Add(1)
			return
		}
		mu.Lock()
		active[secret]++
		if active[secret] > 1 {
			violation.Add(1)
		}
		mu.Unlock()
		defer func() { mu.Lock(); active[secret]--; mu.Unlock() }()
		if r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/clients/report/") {
			var report struct {
				Revision string `json:"revision"`
			}
			if json.Unmarshal(body, &report) != nil {
				violation.Add(1)
				w.WriteHeader(400)
				return
			}
			key := secret + "/" + r.URL.Path + "/" + report.Revision
			hash := sha256.Sum256(body)
			mu.Lock()
			if old, ok := bodyHashes[key]; ok && old != hash {
				violation.Add(1)
			}
			bodyHashes[key] = hash
			mu.Unlock()
			if !release.Load() {
				<-r.Context().Done()
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"revision": report.Revision})
			return
		}
		violation.Add(1)
		w.WriteHeader(400)
	}))
	defer server.Close()
	journal := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := capacity.SustainedOptions{URL: server.URL, Journal: journal, Models: names, ModelSeed: seed, ArrivalSeed: 23, MaxPending: 1000, MaxInFlight: 4, MaxAttempts: 30, MaxJournalBytes: 32 << 20, RequestTimeout: 10 * time.Second, MaxArrivalLag: 5 * time.Second, MaxActive: time.Minute}
	type ended struct {
		result capacity.SustainedResult
		err    error
	}
	run := func(ctx context.Context, opts capacity.SustainedOptions) (chan capacity.SustainedCommand, chan capacity.SustainedSample, chan ended) {
		commands := make(chan capacity.SustainedCommand, 4)
		samples := make(chan capacity.SustainedSample, 20)
		finished := make(chan ended, 1)
		go func() { r, e := capacity.RunSustained(ctx, f, opts, commands, samples); finished <- ended{r, e} }()
		return commands, samples, finished
	}
	ctx, cancel := context.WithCancel(context.Background())
	commands, samples, finished := run(ctx, opts)
	commands <- capacity.SustainedCommand{Operation: "stage", Stage: "slow", Rate: 20}
	var first capacity.SustainedSample
	deadline := time.After(5 * time.Second)
	for first.Produced < 100 || first.DebtRemaining != 0 {
		select {
		case first = <-samples:
		case done := <-finished:
			t.Fatalf("driver exited early: %+v %v", done.result, done.err)
		case <-deadline:
			t.Fatalf("no open-input evidence: %+v", first)
		}
	}
	if first.Confirmed != 0 || first.Pending != first.Produced || first.InFlight != 4 || first.ArrivalProduced == 0 {
		t.Fatalf("slow-response debt not accounted: %+v", first)
	}
	cancel()
	var stopped ended
	select {
	case stopped = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop")
	}
	if stopped.err != context.Canceled || stopped.result.Sample.Pending != stopped.result.Sample.Produced || stopped.result.Sample.InFlight != 0 {
		t.Fatalf("cancel lost debt: %+v %v", stopped.result, stopped.err)
	}
	before := stopped.result.Sample
	release.Store(true)
	opts.Resume = true
	commands, samples, finished = run(context.Background(), opts)
	commands <- capacity.SustainedCommand{Operation: "stage", Stage: "resume", Rate: 0.2}
	var after capacity.SustainedSample
	deadline = time.After(12 * time.Second)
	for {
		select {
		case after = <-samples:
			if after.Pending == 0 && after.Confirmed >= before.Produced {
				goto drained
			}
		case done := <-finished:
			t.Fatalf("resume exited early: %+v %v", done.result, done.err)
		case <-deadline:
			t.Fatalf("resumed debt not resolved: before=%+v after=%+v", before, after)
		}
	}
drained:
	commands <- capacity.SustainedCommand{Operation: "stop"}
	select {
	case stopped = <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not finish")
	}
	if stopped.err != nil || stopped.result.Sample.Produced-stopped.result.Sample.Confirmed != stopped.result.Sample.Pending || violation.Load() != 0 {
		t.Fatalf("resume mismatch: %+v %v violations=%d", stopped.result, stopped.err, violation.Load())
	}
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), f.Secret(1)) {
		t.Fatal("journal contains a credential")
	}
	var creates, confirms int64
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var record struct {
			Kind      string `json:"kind"`
			Action    string `json:"action"`
			Confirmed bool   `json:"confirmed"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record.Kind == "created" {
			creates++
		}
		if record.Kind == "finish" && record.Action == "report" && record.Confirmed {
			confirms++
		}
	}
	if err = scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if creates != stopped.result.Sample.Produced || confirms != stopped.result.Sample.Confirmed {
		t.Fatalf("journal counts %d/%d != sample %+v", creates, confirms, stopped.result.Sample)
	}
}

func TestSustainedRetriesKeepVersionBodyAndRetryAfter(t *testing.T) {
	f, names := sustainedFixture(t, 1)
	var mu sync.Mutex
	var times []time.Time
	var firstHash [32]byte
	var mismatch bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || !strings.HasPrefix(r.URL.Path, "/v1/clients/report/") {
			w.WriteHeader(405)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var value struct {
			Revision string `json:"revision"`
		}
		if json.Unmarshal(body, &value) != nil {
			w.WriteHeader(422)
			return
		}
		hash := sha256.Sum256(body)
		mu.Lock()
		times = append(times, time.Now())
		attempt := len(times)
		if attempt == 1 {
			firstHash = hash
		} else if attempt == 2 && hash != firstHash {
			mismatch = true
		}
		mu.Unlock()
		if attempt == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			fmt.Fprint(w, `{"error":{"code":"report_rate_limited"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"revision": value.Revision})
	}))
	defer server.Close()
	opts := capacity.SustainedOptions{URL: server.URL, Journal: filepath.Join(t.TempDir(), "journal.jsonl"), Models: names, ModelSeed: 9, ArrivalSeed: 99, MaxPending: 100, MaxInFlight: 4, MaxAttempts: 30, MaxJournalBytes: 4 << 20, RequestTimeout: 5 * time.Second, MaxArrivalLag: 5 * time.Second, MaxActive: time.Minute}
	commands := make(chan capacity.SustainedCommand, 4)
	samples := make(chan capacity.SustainedSample, 10)
	done := make(chan error, 1)
	go func() { _, err := capacity.RunSustained(context.Background(), f, opts, commands, samples); done <- err }()
	commands <- capacity.SustainedCommand{Operation: "stage", Stage: "retry", Rate: 0.2}
	deadline := time.After(8 * time.Second)
	for {
		select {
		case sample := <-samples:
			if sample.ReportRetries > 0 && sample.Confirmed > 0 {
				if sample.ReportStatuses["429"] != 1 || sample.ReportErrorCodes["report_rate_limited"] != 1 {
					t.Fatalf("policy limit not visible in sample: %+v", sample)
				}
				goto observed
			}
		case err := <-done:
			t.Fatalf("driver exited: %v", err)
		case <-deadline:
			t.Fatal("retry not observed")
		}
	}
observed:
	commands <- capacity.SustainedCommand{Operation: "stop"}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) < 2 || times[1].Sub(times[0]) < time.Second || mismatch {
		t.Fatalf("retry violated identity/body/delay: count=%s mismatch=%v", strconv.Itoa(len(times)), mismatch)
	}
}
