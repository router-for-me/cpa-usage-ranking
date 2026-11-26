package capacity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSustainedRetryClassificationForAmbiguousServerErrors(t *testing.T) {
	var status atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		fmt.Fprint(w, `{"error":{"code":"report_overloaded"}}`)
	}))
	defer server.Close()
	f := Fixture{Seed: strings.Repeat("ab", 32)}
	task := SustainedTask{Sequence: 1, Client: 1, Day: "2026-11-13", Revision: 2, Body: []byte(`{}`)}
	for _, tt := range []struct {
		status    int
		retryable bool
	}{{408, true}, {429, true}, {500, true}, {501, true}, {502, true}, {503, true}, {504, true}, {599, true}, {400, false}, {422, false}} {
		status.Store(int32(tt.status))
		result := sustainedRequest(context.Background(), server.Client(), server.URL, f, task, 1, time.Second, &sustainedTraceCounts{})
		if result.retryable != tt.retryable || result.record.Status != tt.status || result.record.ErrorCode != "report_overloaded" {
			t.Fatalf("status %d: %+v", tt.status, result)
		}
	}
}

func TestSustainedOptionsAcceptLocalhostAndSeedSupportsSingleModelCatalog(t *testing.T) {
	now := time.Now().UTC()
	today := now.Format(time.DateOnly)
	opts := SustainedOptions{
		URL:             "http://localhost:8080",
		Journal:         "journal.jsonl",
		MaxPending:      10,
		MaxInFlight:     4,
		MaxAttempts:     3,
		MaxJournalBytes: 2 << 20,
		RequestTimeout:  time.Second,
		MaxArrivalLag:   time.Second,
		MaxActive:       time.Minute,
	}
	todayFixture := Fixture{Clients: 2, Day: today, HistoryDays: 2, Backlog: 0, Models: []string{"model-a"}, Seed: strings.Repeat("ab", 32)}
	if err := validateSustainedOptions(todayFixture, opts); err != nil {
		t.Fatalf("today localhost fixture rejected: %v", err)
	}
	yesterdayFixture := todayFixture
	yesterdayFixture.Day = now.AddDate(0, 0, -1).Format(time.DateOnly)
	if err := validateSustainedOptions(yesterdayFixture, opts); err == nil {
		t.Fatal("yesterday fixture unexpectedly accepted for sustained mode")
	}
	nonLoopback := opts
	nonLoopback.URL = "http://ranking.example:8080"
	if err := validateSustainedOptions(todayFixture, nonLoopback); err == nil {
		t.Fatal("non-loopback host unexpectedly accepted")
	}
	emptyModels := todayFixture
	emptyModels.Models = nil
	if err := emptyModels.Validate(); err == nil {
		t.Fatal("empty models fixture unexpectedly accepted")
	}

	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "models.json")
	if err := os.WriteFile(catalogPath, []byte(`{"models":[{"model_name":"only-model"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "seeded.db")
	seeded, err := Seed(context.Background(), dbPath, catalogPath, 2, 2, 0, today)
	if err != nil {
		t.Fatalf("single-model Seed failed: %v", err)
	}
	if len(seeded.Models) != 1 || seeded.Models[0] != "only-model" {
		t.Fatalf("unexpected seeded models: %+v", seeded.Models)
	}
	stats, err := Inspect(context.Background(), dbPath)
	if err != nil || stats["clients"] != int64(2) {
		t.Fatalf("Inspect seeded db: stats=%+v err=%v", stats, err)
	}
}
