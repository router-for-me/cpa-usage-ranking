package test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpa-usage-ranking/internal/capacity"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

func TestSustainedVerifierRecognizesAmbiguousCommit(t *testing.T) {
	ctx := context.Background()
	catalogPath, names := sustainedCatalog(t)
	day := time.Now().UTC().Format(time.DateOnly)
	const seed uint64 = 1
	path := filepath.Join(t.TempDir(), "service.sqlite")
	f, err := capacity.SeedSustained(ctx, path, catalogPath, 1, 2, 0, day, seed)
	if err != nil {
		t.Fatal(err)
	}
	seedPath := filepath.Join(t.TempDir(), "seed.sqlite")
	src, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(seedPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err = dst.Close(); err != nil {
		t.Fatal(err)
	}
	if err = src.Close(); err != nil {
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
	store := reports.NewStore(db.Writer, registry, nil)
	committed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e != nil {
			w.WriteHeader(400)
			return
		}
		report, e := reports.Decode(body)
		if e != nil {
			w.WriteHeader(422)
			return
		}
		prepared, e := reports.Prepare(report)
		if e != nil {
			w.WriteHeader(422)
			return
		}
		if _, e = store.Apply(r.Context(), 1, r.URL.Path[len("/v1/clients/report/"):], prepared); e != nil {
			w.WriteHeader(500)
			return
		}
		h, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(500)
			return
		}
		conn, _, e := h.Hijack()
		if e != nil {
			return
		}
		_ = conn.Close()
		select {
		case committed <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	journal := filepath.Join(t.TempDir(), "journal.jsonl")
	opts := capacity.SustainedOptions{URL: server.URL, Journal: journal, Models: names, ModelSeed: seed, ArrivalSeed: 99, MaxPending: 100, MaxInFlight: 1, MaxAttempts: 30, MaxJournalBytes: 4 << 20, RequestTimeout: 5 * time.Second, MaxArrivalLag: 5 * time.Second, MaxActive: time.Minute}
	commands := make(chan capacity.SustainedCommand, 2)
	type result struct {
		out capacity.SustainedResult
		err error
	}
	done := make(chan result, 1)
	go func() { out, e := capacity.RunSustained(ctx, f, opts, commands, nil); done <- result{out, e} }()
	commands <- capacity.SustainedCommand{Operation: "stage", Stage: "ambiguous", Rate: 0.2}
	select {
	case <-committed:
		commands <- capacity.SustainedCommand{Operation: "stop"}
	case <-time.After(5 * time.Second):
		t.Fatal("commit not observed")
	}
	var stopped result
	select {
	case stopped = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("driver failed to stop")
	}
	if stopped.err != nil || stopped.out.Sample.Pending < 1 || stopped.out.Sample.Confirmed != 0 {
		t.Fatalf("not an unconfirmed task: %+v %v", stopped.out, stopped.err)
	}
	if _, err = db.Writer.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	v, err := capacity.VerifySustained(ctx, f, capacity.SustainedVerifyOptions{SeedDB: seedPath, FinalDB: path, Journal: journal, ExpectedDB: filepath.Join(t.TempDir(), "expected.sqlite"), Models: names, ModelSeed: seed})
	if err != nil {
		t.Fatal(err)
	}
	if v.UnconfirmedCommits != 1 || v.Pending < 1 || v.Confirmed != 0 {
		t.Fatalf("ambiguous commit not recognized: %+v", v)
	}
}
