package test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"cpa-usage-ranking/internal/capacity"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

func fixture(t *testing.T, clients, backlog int) (capacity.Fixture, string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "load.sqlite")
	catalog := filepath.Join(dir, "models.json")
	items := []map[string]any{}
	for i := 0; i < 8; i++ {
		items = append(items, map[string]any{"model_name": fmt.Sprintf("model-%d", i), "alias": []string{}, "prices": map[string]any{}})
	}
	data, _ := json.Marshal(map[string]any{"currency": "USD", "price_unit": "per_million_tokens", "models": items})
	if err := os.WriteFile(catalog, data, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := capacity.Seed(context.Background(), path, catalog, clients, 90, backlog, "2026-11-12")
	if err != nil {
		t.Fatal(err)
	}
	return f, path, catalog
}

func TestSyntheticFixtureIsExclusiveAndSettlementConservesTotals(t *testing.T) {
	ctx := context.Background()
	f, path, catalog := fixture(t, 3, 2)
	before, err := capacity.Inspect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if before["clients"] != int64(3) || before["daily_reports"] != int64(8) || before["daily_rows"] != int64(72) || before["closed_backlog"] != int64(2) || before["pending_tokens"] != int64(64000) || before["settled_tokens"] != int64((3*90-2)*8000) {
		t.Fatalf("unexpected fixture: %v", before)
	}
	if _, err = capacity.Seed(ctx, path, catalog, 3, 90, 0, f.Day); err == nil {
		t.Fatal("seed overwrote existing database")
	}
	db, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hash []byte
	if err = db.Reader.QueryRow("SELECT token_hash FROM client_writers WHERE id=1").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(f.Secret(1))
	expected := sha256.Sum256(raw)
	if !bytes.Equal(hash, expected[:]) {
		t.Fatal("synthetic credential mismatch")
	}
	for id := int64(1); id <= 2; id++ {
		ok, e := reports.Finalize(ctx, db.Writer, "2026-11-10", id)
		if e != nil || !ok {
			t.Fatalf("real settlement: %v %v", ok, e)
		}
	}
	after, err := capacity.Inspect(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if after["closed_backlog"] != int64(0) || after["pending_tokens"] != int64(48000) || after["settled_tokens"] != int64(3*90*8000) {
		t.Fatalf("settlement loss/duplication: %v", after)
	}
	probes, err := capacity.Probe(ctx, path, f.Day)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 5 {
		t.Fatal("missing periods")
	}
	for _, p := range probes {
		if p["candidates"] != 3 {
			t.Fatalf("wrong candidates %v", p)
		}
		metrics, ok := p["metrics"].(reports.Metrics)
		if !ok {
			t.Fatalf("missing metrics: %v", p)
		}
		if p["kind"] == "all" && metrics.TotalTokens != 3*92*8000 {
			t.Fatalf("incorrect all-period total: %v", p)
		}
		if p["kind"] == "day" && metrics.TotalTokens != 3*8000 {
			t.Fatalf("incorrect day total: %v", p)
		}
		if p["kind"] == "week" && metrics.TotalTokens != 168000 {
			t.Fatalf("incorrect previous week total: %v", p)
		}
		if p["kind"] == "month" && metrics.TotalTokens != 744000 {
			t.Fatalf("incorrect previous month total: %v", p)
		}
	}
}

func TestLoadWritesReceiptsAndVerifiesRealReplacements(t *testing.T) {
	ctx := context.Background()
	f, path, catalogPath := fixture(t, 10, 0)
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
	now, _ := time.Parse(time.DateOnly, f.Day)
	store := reports.NewStore(db.Writer, registry, func() time.Time { return now.Add(12 * time.Hour) })
	ids := map[string]int64{}
	for i := 1; i <= 10; i++ {
		ids["Bearer "+f.Secret(i)] = int64(i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ids[r.Header.Get("Authorization")]
		if id == 0 {
			w.WriteHeader(401)
			return
		}
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
		revision, e := store.Apply(r.Context(), id, f.Day, prepared)
		if e != nil {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"revision": strconv.FormatInt(revision, 10)})
	}))
	defer server.Close()
	ledger := filepath.Join(t.TempDir(), "receipts.jsonl")
	summary, err := capacity.Load(ctx, f, capacity.LoadOptions{URL: server.URL, RPS: 20, Duration: time.Second, Workers: 8, Revision: 2, Changed: 2, Ledger: ledger})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Confirmed != 20 || summary.Sent != 20 || summary.GeneratorDropped != 0 || summary.BadACK != 0 {
		t.Fatalf("load incomplete: %+v", summary)
	}
	checked, err := capacity.Verify(ctx, f, path, []string{ledger})
	if err != nil {
		t.Fatal(err)
	}
	if checked["checked"] != 10 {
		t.Fatalf("ledger verification: %v", checked)
	}
	if _, err = capacity.Load(ctx, f, capacity.LoadOptions{URL: server.URL, RPS: 1, Duration: time.Second, Workers: 1, Revision: 2, Changed: 2, Ledger: ledger}); err == nil {
		t.Fatal("receipt history overwritten")
	}
	if _, err = db.Writer.Exec("UPDATE daily_stats SET total_tokens=total_tokens+1 WHERE model_id=0 AND day=? AND client_id=1", f.Day); err != nil {
		t.Fatal(err)
	}
	if _, err = capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("ledger failed to detect corrupted totals")
	}
}

func TestLoadDoesNotCountIncorrectSuccessBodies(t *testing.T) {
	f, _, _ := fixture(t, 2, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"revision":"999"}`) }))
	defer server.Close()
	summary, err := capacity.Load(context.Background(), f, capacity.LoadOptions{URL: server.URL, RPS: 10, Duration: 200 * time.Millisecond, Workers: 2, Revision: 2, Changed: 8, Ledger: filepath.Join(t.TempDir(), "bad.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sent != 2 || summary.Confirmed != 0 || summary.BadACK != 2 || summary.P95MS != nil || summary.Seconds < 0.2 {
		t.Fatalf("invalid success counted: %+v", summary)
	}
}

func TestLoadOffsetsTwoDayRotation(t *testing.T) {
	f, _, _ := fixture(t, 4, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"revision":"2"}`) }))
	defer server.Close()
	ledger := filepath.Join(t.TempDir(), "offset.jsonl")
	summary, err := capacity.Load(context.Background(), f, capacity.LoadOptions{URL: server.URL, RPS: 20, Duration: 200 * time.Millisecond, Workers: 4, Offset: 2, Revision: 2, Changed: 2, BothDays: true, Ledger: ledger})
	if err != nil || summary.Confirmed != 4 || summary.GeneratorDropped != 0 {
		t.Fatalf("offset load: %+v %v", summary, err)
	}
	file, err := os.Open(ledger)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got := map[string]bool{}
	scan := bufio.NewScanner(file)
	for scan.Scan() {
		var r capacity.Receipt
		if err := json.Unmarshal(scan.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		got[fmt.Sprintf("%d/%s", r.ID, r.Day)] = true
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"3/2026-11-12", "3/2026-11-11", "4/2026-11-12", "4/2026-11-11"} {
		if !got[key] {
			t.Fatalf("missing %s in %v", key, got)
		}
	}
	if _, err := capacity.Load(context.Background(), f, capacity.LoadOptions{URL: server.URL, RPS: 1, Duration: time.Second, Workers: 1, Offset: 4, Revision: 2, Changed: 2, Ledger: filepath.Join(t.TempDir(), "invalid.jsonl")}); err == nil {
		t.Fatal("out-of-range identity offset accepted")
	}
}

func TestVerifyDistinguishesRejectionFromAmbiguousCommit(t *testing.T) {
	ctx := context.Background()
	f, path, catalogPath := fixture(t, 1, 0)
	ledger := filepath.Join(t.TempDir(), "receipts.jsonl")
	writeReceipts := func(rs ...capacity.Receipt) {
		t.Helper()
		var b bytes.Buffer
		for _, r := range rs {
			if err := json.NewEncoder(&b).Encode(r); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(ledger, b.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	receipt := capacity.Receipt{ID: 1, Day: f.Day, Revision: 2, Changed: 8, Status: 429}
	writeReceipts(receipt)
	got, err := capacity.Verify(ctx, f, path, []string{ledger})
	if err != nil || got["unchanged_at_seed"] != 1 || got["unconfirmed_commits"] != 0 || got["rejections_checked"] != 1 {
		t.Fatalf("rejected unchanged snapshot: %v %v", got, err)
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
	body, err := f.Snapshot(2, 8)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := store.Apply(ctx, 1, f.Day, prepared); err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("explicit 429 incorrectly allowed to commit")
	}
	for _, status := range []int{0, 200, 503} {
		receipt.Status = status
		writeReceipts(receipt)
		got, err = capacity.Verify(ctx, f, path, []string{ledger})
		if err != nil || got["unconfirmed_commits"] != 1 {
			t.Fatalf("ambiguous status %d: %v %v", status, got, err)
		}
	}
	receipt.Status, receipt.Confirmed = 200, true
	writeReceipts(receipt)
	got, err = capacity.Verify(ctx, f, path, []string{ledger})
	if err != nil || got["checked"] != 1 || got["unconfirmed_commits"] != 0 {
		t.Fatalf("confirmed commit: %v %v", got, err)
	}
	rejected := receipt
	rejected.Revision, rejected.Status, rejected.Confirmed = 3, 429, false
	writeReceipts(receipt, rejected)
	got, err = capacity.Verify(ctx, f, path, []string{ledger})
	if err != nil || got["checked"] != 1 || got["unconfirmed_commits"] != 0 || got["rejections_checked"] != 1 {
		t.Fatalf("rejected newer revision changed prior confirmation: %v %v", got, err)
	}
	rejected.Status, rejected.Confirmed = 200, true
	writeReceipts(receipt, rejected)
	if _, err := capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("lost newer confirmed revision was not detected")
	}
	writeReceipts(receipt, receipt)
	if _, err := capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("reused identity/day revision was accepted")
	}
	body, err = f.Snapshot(4, 8)
	if err != nil {
		t.Fatal(err)
	}
	report, err = reports.Decode(body)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err = reports.Prepare(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(ctx, 1, f.Day, prepared); err != nil {
		t.Fatal(err)
	}
	rejected.Status, rejected.Confirmed = 429, false
	latest := receipt
	latest.Revision = 4
	writeReceipts(receipt, rejected, latest)
	got, err = capacity.Verify(ctx, f, path, []string{ledger})
	if err != nil || got["rejections_checked"] != 0 || got["rejections_superseded"] != 1 {
		t.Fatalf("superseded rejection was mistaken for verified rejection: %v %v", got, err)
	}
	writeReceipts(latest)
	if _, err := db.Writer.Exec(`UPDATE daily_stats SET input_tokens=input_tokens+1 WHERE day=? AND client_id=1 AND model_id=(SELECT id FROM models WHERE model_name=?)`, f.Day, f.Models[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := capacity.Verify(ctx, f, path, []string{ledger}); err == nil {
		t.Fatal("per-model metric corruption was not detected")
	}
}
