package test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/reports"
)

func TestWithdrawUnknownContentLength(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	httpserver.MountWithdraw(f.router, f.repo, nil)
	secret := testSecret(1)
	f.register(t, secret, 42, "Seat")
	for _, body := range []string{"x", ""} {
		req, err := http.NewRequest(http.MethodPost, f.server.URL+withdrawPath, io.NopCloser(strings.NewReader(body)))
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if body != "" {
			requireHTTPError(t, registrationResponse{status: resp.StatusCode, body: data}, http.StatusBadRequest, "invalid_request")
		} else if resp.StatusCode != http.StatusNoContent || len(data) != 0 {
			t.Fatalf("empty chunked withdrawal: %d %s", resp.StatusCode, data)
		}
	}
}

func TestMultiWriterReportAndWholeSeatWithdrawal(t *testing.T) {
	f := reportSetup(t, defaultRegistrationSettings(), generousReportLimits(), nil)
	var refreshes atomic.Int32
	httpserver.MountWithdraw(f.router, f.repo, func() { refreshes.Add(1) })
	a, b, other := testSecret(1), testSecret(2), testSecret(3)
	seata := f.register(t, a, 42, "Shared")
	seatb := f.register(t, b, 42, "Ignored")
	otherID := f.start(t, other, "550e8400-e29b-41d4-a716-446655440001", returnURL)
	f.approve(t, otherID, 42, "Other CPA", "test", returnURL)
	otherSeat := identity(t, f.complete(t, other, otherID))
	if seata != seatb || seata.ID == otherSeat.ID {
		t.Fatal("test did not allocate a shared seat and a distinct seat")
	}
	assertTotal := func(id int64, expected int) {
		t.Helper()
		var total int
		if err := f.db.Reader.QueryRow(`SELECT total_tokens FROM daily_stats WHERE client_id=? AND model_id=0`, id).Scan(&total); err != nil || total != expected {
			t.Fatalf("total seat %d = %d, want %d, err %v", id, total, expected, err)
		}
	}
	day := "2026-11-01"
	a1 := strings.Replace(reportJSON, `"revision":"1"`, `"revision":"18"`, 1)
	b1 := strings.Replace(strings.Replace(reportJSON, `"total_tokens":"10"`, `"total_tokens":"7"`, 1), `"input_tokens":"8"`, `"input_tokens":"5"`, 1)
	expectRevision(t, f.report(t, day, a, a1, nil), 200, "18")
	expectRevision(t, f.report(t, day, b, b1, nil), 200, "1")
	expectRevision(t, f.report(t, day, a, a1, nil), 200, "18")
	expectRevision(t, f.report(t, day, b, reportJSON, nil), 409, "1")
	expectRevision(t, f.report(t, day, a, reportJSON, nil), 409, "18")
	assertTotal(seata.ID, 7)
	a2 := strings.Replace(a1, `"revision":"18"`, `"revision":"19"`, 1)
	expectRevision(t, f.report(t, day, a, a2, nil), 200, "19")
	expectRevision(t, f.report(t, day, b, b1, nil), 200, "1")
	assertTotal(seata.ID, 10)
	requireHTTPError(t, f.api(t, "POST", withdrawPath, "", testSecret(99), nil), 401, "invalid_credential")
	requireHTTPError(t, f.api(t, "POST", withdrawPath, `{"client_id":"1"}`, a, nil), 400, "invalid_request")
	requireHTTPError(t, f.api(t, "POST", withdrawPath+"?client_id=2", "", a, nil), 400, "invalid_request")
	for _, secret := range []string{a, b} {
		if r := f.api(t, "POST", withdrawPath, "", secret, nil); r.status != 204 || len(r.body) != 0 {
			t.Fatalf("withdrawal is not idempotent: %d %s", r.status, r.body)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("withdrawal did not notify cache: %d", refreshes.Load())
	}
	requireHTTPError(t, f.report(t, day, a, a2, nil), 403, "client_withdrawn")
	requireHTTPError(t, f.report(t, day, b, b1, nil), 403, "client_withdrawn")
	requireHTTPError(t, f.report(t, day, b, strings.Replace(b1, `"revision":"1"`, `"revision":"2"`, 1), nil), 403, "client_withdrawn")
	expectRevision(t, f.report(t, day, other, reportJSON, nil), 200, "1")
	assertTotal(seata.ID, 10)
	assertTotal(otherSeat.ID, 10)
	var versions int
	if err := f.db.Reader.QueryRow("SELECT sum(revision) FROM daily_writer_versions WHERE client_id=?", seata.ID).Scan(&versions); err != nil || versions != 20 {
		t.Fatalf("withdrawal or rejected report mutated revision: %d %v", versions, err)
	}
}

func TestReportRateLimitIsSharedByWriters(t *testing.T) {
	limits := generousReportLimits()
	limits.ClientBurst = 1
	f := reportSetup(t, defaultRegistrationSettings(), limits, nil)
	a, b := testSecret(1), testSecret(2)
	f.register(t, a, 42, "Shared")
	f.register(t, b, 42, "Shared")
	expectRevision(t, f.report(t, "2026-11-01", a, reportJSON, nil), 200, "1")
	requireHTTPError(t, f.report(t, "2026-11-01", b, reportJSON, nil), 429, "report_rate_limited")
}

func TestWithdrawWinsAgainstAlreadyAuthenticatedReport(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	f := reportSetup(t, defaultRegistrationSettings(), generousReportLimits(), func(s *reports.Store) reports.SnapshotProcessor {
		return httpProcessor(func(ctx context.Context, writerID int64, day string, prepared reports.Prepared) (int64, error) {
			close(started)
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-release:
				return s.Apply(ctx, writerID, day, prepared)
			}
		})
	})
	httpserver.MountWithdraw(f.router, f.repo, nil)
	a, b := testSecret(1), testSecret(2)
	f.register(t, a, 42, "Shared")
	f.register(t, b, 42, "Shared")
	done := make(chan registrationResponse, 1)
	go func() { done <- f.report(t, "2026-11-01", b, reportJSON, nil) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("report never passed middleware authentication")
	}
	withdrawn := f.api(t, "POST", withdrawPath, "", a, nil)
	close(release)
	if withdrawn.status != 204 {
		t.Fatalf("withdraw failed: %d", withdrawn.status)
	}
	select {
	case r := <-done:
		requireHTTPError(t, r, 403, "client_withdrawn")
	case <-time.After(3 * time.Second):
		t.Fatal("no report result")
	}
	var rows int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM daily_writer_versions").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("late report advanced writer version: %d %v", rows, err)
	}
}

func TestWithdrawalRefreshExcludesSeatFromBoardsAndScoreReferences(t *testing.T) {
	var clock atomic.Int64
	enter := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	clock.Store(enter.UnixNano())
	dir := t.TempDir()
	catalogPath := filepath.Join(dir, "models.json")
	if err := os.WriteFile(catalogPath, []byte(`{"currency":"USD","price_unit":"per_million_tokens","models":[{"model_name":"alpha","alias":[],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	f := startLifecycle(t, filepath.Join(dir, "withdraw.db"), catalogPath, &clock, true)
	a, other := testSecret(1), testSecret(2)
	f.register(t, a, 42, "Withdrawn Seat")
	f.register(t, other, 43, "Remaining")
	id := f.start(t, testSecret(3), serverID, returnURL)
	browser := f.open(t, id)
	f.github.setGrant("test-outage", fakeGrant{challenge: browser.challenge, tokenStatus: 503})
	requireHTTPError(t, f.callback(t, browser.state, browser.cookie, "code=test-outage"), 500, "internal_error")
	// 已注册端继续写入，公开读榜继续读取已发布的完整缓存。
	before := f.readBoard(t, "overall", "all")
	expectRevision(t, f.report(t, "2026-11-02", other, lifecycleFirst, nil), 200, "1")
	round := enter.Add(time.Second)
	clock.Store(round.UnixNano())
	f.waitRound(t, round)
	solo := f.readBoard(t, "overall", "all")
	if len(solo.Entries) != 1 || len(before.Entries) != 0 {
		t.Fatal("failed to seed scoring reference control")
	}
	expectRevision(t, f.report(t, "2026-11-02", a, lifecycleOtherFirst, nil), 200, "1")
	round = enter.Add(2 * time.Second)
	clock.Store(round.UnixNano())
	f.waitRound(t, round)
	together := f.readBoard(t, "overall", "all")
	if len(together.Entries) != 2 || together.Entries[0].Metric == solo.Entries[0].Metric {
		t.Fatal("failed to affect scoring reference set")
	}
	if r := f.api(t, "POST", withdrawPath, "", a, nil); r.status != 204 {
		t.Fatalf("withdraw: %d", r.status)
	}
	round = enter.Add(3 * time.Second)
	clock.Store(round.UnixNano())
	f.waitRound(t, round)
	for _, board := range []string{"overall", "usage", "cache", "success", "ttft"} {
		for _, period := range []string{"today", "yesterday", "week", "month", "year", "all"} {
			data := f.readBoard(t, board, period)
			for _, row := range data.Entries {
				if row.ClientID != solo.Entries[0].ClientID {
					t.Fatalf("withdrawn seat remains on %s/%s", board, period)
				}
			}
		}
	}
	after := f.readBoard(t, "overall", "all")
	if len(after.Entries) != 1 || after.Entries[0].Metric != solo.Entries[0].Metric {
		t.Fatal("withdrawn seat still affected reference set")
	}
}
