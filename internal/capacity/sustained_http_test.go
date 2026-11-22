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
