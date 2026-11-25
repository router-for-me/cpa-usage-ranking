package capacity

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestSustainedOptionsAcceptLocalhostYesterdayAndVariableModelCount(t *testing.T) {
	now := time.Now().UTC()
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
	for _, day := range []string{now.Format(time.DateOnly), now.AddDate(0, 0, -1).Format(time.DateOnly)} {
		f := Fixture{Clients: 2, Day: day, HistoryDays: 2, Backlog: 0, Models: []string{"model-a", "model-b"}, Seed: strings.Repeat("ab", 32)}
		if err := validateSustainedOptions(f, opts); err != nil {
			t.Fatalf("day %s rejected: %v", day, err)
		}
	}
	oldFixture := Fixture{Clients: 2, Day: now.AddDate(0, 0, -2).Format(time.DateOnly), HistoryDays: 2, Backlog: 0, Models: []string{"model-a"}, Seed: strings.Repeat("ab", 32)}
	if err := validateSustainedOptions(oldFixture, opts); err == nil {
		t.Fatal("two-day-old fixture unexpectedly accepted")
	}
	nonLoopback := opts
	nonLoopback.URL = "http://ranking.example:8080"
	todayFixture := Fixture{Clients: 2, Day: now.Format(time.DateOnly), HistoryDays: 2, Backlog: 0, Models: []string{"model-a"}, Seed: strings.Repeat("ab", 32)}
	if err := validateSustainedOptions(todayFixture, nonLoopback); err == nil {
		t.Fatal("non-loopback host unexpectedly accepted")
	}
	emptyModels := todayFixture
	emptyModels.Models = nil
	if err := emptyModels.Validate(); err == nil {
		t.Fatal("empty models fixture unexpectedly accepted")
	}
}
