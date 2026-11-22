package test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cpa-usage-ranking/internal/reports"
)

type cancellingReportBody struct {
	io.Reader
	cancel context.CancelFunc
}

func (b cancellingReportBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.cancel()
	return n, err
}

func TestCancelledReportNeverFallsThroughToEmptySuccess(t *testing.T) {
	for _, queued := range []bool{false, true} {
		name := "before-enqueue"
		if queued {
			name = "waiting-for-result"
		}
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			f := reportSetup(t, defaultRegistrationSettings(), generousReportLimits(), func(_ *reports.Store) reports.SnapshotProcessor {
				return httpProcessor(func(ctx context.Context, _ int64, _ string, _ reports.Prepared) (int64, error) {
					close(started)
					<-ctx.Done()
					<-release
					return 0, ctx.Err()
				})
			})
			f.seed(t, testSecret(1))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var body io.Reader = strings.NewReader(reportJSON)
			if !queued {
				body = cancellingReportBody{body, cancel}
			}
			req := httptest.NewRequest("PUT", "/v1/clients/report/2026-11-01", body).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+testSecret(1))
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); f.router.ServeHTTP(recorder, req) }()
			if queued {
				select {
				case <-started:
					cancel()
				case <-time.After(2 * time.Second):
					t.Fatal("report did not reach writer")
				}
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled HTTP handler did not return")
			}
			requireHTTPError(t, registrationResponse{status: recorder.Code, header: recorder.Header(), body: recorder.Body.Bytes()}, 503, "report_overloaded")
		})
	}
}
