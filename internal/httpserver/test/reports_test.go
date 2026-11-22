package test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
)

const reportJSON = `{"schema_version":1,"revision":"1","items":[{"model":"alpha","total_tokens":"10","input_tokens":"8","output_tokens":"2","cache_read_tokens":"3","cache_write_tokens":"1","reasoning_tokens":"1","request_count":"2","success_count":"1","ttft_avg_ms":null,"ttft_samples":"0","latency_avg_ms":2,"latency_samples":"2"}]}`

type reportFixture struct {
	*registrationFixture
	clock  atomic.Int64
	writer *reports.Writer
	store  *reports.Store
}

func reportSetup(t *testing.T, registration registrationSettings, limits reports.IngestionLimits, processor func(*reports.Store) reports.SnapshotProcessor) *reportFixture {
	t.Helper()
	f := &reportFixture{registrationFixture: newRegistrationFixture(t, registration)}
	f.clock.Store(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC).UnixNano())
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"currency":"USD","price_unit":"per_million_tokens","models":[{"model_name":"alpha","alias":["alias"],"prices":{}}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := models.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := models.Register(context.Background(), f.db.Writer, catalog)
	if err != nil {
		t.Fatal(err)
	}
	f.store = reports.NewStore(f.db.Writer, registry, func() time.Time { return time.Unix(0, f.clock.Load()).UTC() })
	var apply reports.SnapshotProcessor = f.store
	if processor != nil {
		apply = processor(f.store)
	}
	f.writer, err = reports.NewWriter(apply, reports.WriterOptions{QueueCapacity: 1, TransactionTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.writer.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	admission, err := reports.NewAdmission(limits, func() time.Time { return time.Unix(0, f.clock.Load()).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	if err := httpserver.MountReports(f.router, auth.NewAuthenticator(f.repo), admission, f.writer, int64(limits.MaxBodyBytes)); err != nil {
		t.Fatal(err)
	}
	return f
}

func generousReportLimits() reports.IngestionLimits {
	l := reports.DefaultIngestionLimits()
	l.ClientBurst, l.GlobalBurst = 100, 1000
	return l
}

func (f *reportFixture) report(t *testing.T, day, secret, body string, headers map[string]string) registrationResponse {
	t.Helper()
	r, err := http.NewRequest(http.MethodPut, f.server.URL+"/v1/clients/report/"+day, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	res, err := f.server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return registrationResponse{status: res.StatusCode, header: res.Header, body: data}
}

func expectRevision(t *testing.T, response registrationResponse, status int, revision string) {
	t.Helper()
	var body struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(response.body, &body); err != nil || response.status != status || body.Revision != revision {
		t.Fatalf("response %d %s; wanted %d revision %s", response.status, response.body, status, revision)
	}
	if response.header.Get("Cache-Control") != "no-store" {
		t.Fatal("report response cached")
	}
}

func TestReportHTTPIdentityRevisionsWindowsAndRegistrationSwitch(t *testing.T) {
	settings := defaultRegistrationSettings()
	settings.service.Enabled = false
	f := reportSetup(t, settings, generousReportLimits(), nil)
	first, second := f.seed(t, testSecret(1)), f.seed(t, testSecret(2))
	a, b := testSecret(1), testSecret(2)
	day := "2026-11-01"
	requireHTTPError(t, f.api(t, "PUT", "/v1/reports/"+day, reportJSON, a, nil), 404, "not_found")
	expectRevision(t, f.report(t, day, a, reportJSON, nil), 200, "1")
	expectRevision(t, f.report(t, day, b, reportJSON, nil), 200, "1")
	expectRevision(t, f.report(t, day, a, reportJSON, nil), 200, "1")
	changed := strings.Replace(reportJSON, `"total_tokens":"10"`, `"total_tokens":"11"`, 1)
	expectRevision(t, f.report(t, day, a, changed, nil), 409, "1")
	jump := strings.Replace(changed, `"revision":"1"`, `"revision":"9"`, 1)
	expectRevision(t, f.report(t, day, a, jump, nil), 200, "9")
	expectRevision(t, f.report(t, day, a, reportJSON, nil), 409, "9")
	var aTotal, bTotal int64
	if err := f.db.Reader.QueryRow(`SELECT total_tokens FROM daily_stats WHERE client_id=? AND day=? AND model_id=0`, first.ID, day).Scan(&aTotal); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Reader.QueryRow(`SELECT total_tokens FROM daily_stats WHERE client_id=? AND day=? AND model_id=0`, second.ID, day).Scan(&bTotal); err != nil {
		t.Fatal(err)
	}
	if aTotal != 11 || bTotal != 10 {
		t.Fatalf("client isolation: %d %d", aTotal, bTotal)
	}
	claim := strings.Replace(reportJSON, `"revision":"1"`, `"client_id":`+strconv.FormatInt(second.ID, 10)+`,"revision":"10"`, 1)
	requireHTTPError(t, f.report(t, day, a, claim, nil), 422, "invalid_report")
	requireHTTPError(t, f.report(t, "2026-11-02", a, reportJSON, nil), 422, "invalid_day")
	requireHTTPError(t, f.report(t, "2026-02-29", a, reportJSON, nil), 422, "invalid_day")
	requireHTTPError(t, f.report(t, "2026-10-30", a, reportJSON, nil), 410, "expired_day")
	requireHTTPError(t, f.report(t, day, "", reportJSON, nil), 401, "invalid_credential")
	if err := f.repo.SetDisabled(context.Background(), second.ID, true); err != nil {
		t.Fatal(err)
	}
	requireHTTPError(t, f.report(t, day, b, reportJSON, nil), 403, "client_disabled")
	requireHTTPError(t, f.send(t, validStartBody, testSecret(3), nil), 503, "registration_closed")
	if strings.Contains(f.logs.String(), a) || strings.Contains(f.logs.String(), reportJSON) {
		t.Fatal("sensitive report was logged")
	}
}

func TestReportHTTPRejectsLegacyTierAndDuplicateModels(t *testing.T) {
	f := reportSetup(t, defaultRegistrationSettings(), generousReportLimits(), nil)
	secret := testSecret(1)
	f.seed(t, secret)
	for _, value := range []string{`"default"`, `null`} {
		body := strings.Replace(reportJSON, `"model":"alpha"`, `"model":"alpha","service_tier":`+value, 1)
		requireHTTPError(t, f.report(t, "2026-11-01", secret, body, nil), 422, "invalid_report")
	}
	item := strings.TrimSuffix(strings.SplitN(reportJSON, `"items":[`, 2)[1], `]}`)
	for _, name := range []string{"alias", "route/alpha"} {
		duplicate := strings.Replace(item, `"model":"alpha"`, `"model":"`+name+`"`, 1)
		body := strings.TrimSuffix(reportJSON, `]}`) + `,` + duplicate + `]}`
		requireHTTPError(t, f.report(t, "2026-11-01", secret, body, nil), 422, "invalid_report")
	}
	var count int
	if err := f.db.Reader.QueryRow(`SELECT count(*) FROM daily_reports`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid report persisted: %d %v", count, err)
	}
}

func TestReportHTTPBadInputLimitsAndIndependentHealth(t *testing.T) {
	limits := generousReportLimits()
	limits.ClientBurst = 20
	limits.MaxBodyBytes = len(reportJSON) + 16
	limits.MaxItems = 1
	f := reportSetup(t, defaultRegistrationSettings(), limits, nil)
	secret := testSecret(1)
	f.seed(t, secret)
	day := "2026-11-01"
	for _, tc := range []struct {
		body    string
		headers map[string]string
		status  int
		code    string
	}{
		{`{`, nil, 422, "invalid_report"},
		{strings.Replace(reportJSON, `"input_tokens":"8"`, `"input_tokens":null`, 1), nil, 422, "invalid_report"},
		{reportJSON + strings.Repeat(" ", 17), nil, 413, "request_too_large"},
		{`{"schema_version":1,"revision":"1","items":[{},{}]}`, nil, 422, "too_many_items"},
		{reportJSON, map[string]string{"Content-Type": "text/plain"}, 415, "unsupported_media_type"},
		{reportJSON, map[string]string{"Content-Encoding": "gzip"}, 415, "unsupported_media_type"},
	} {
		requireHTTPError(t, f.report(t, day, secret, tc.body, tc.headers), tc.status, tc.code)
	}
	requireHTTPError(t, f.report(t, day, "", strings.Repeat("x", limits.MaxBodyBytes+100), nil), 401, "invalid_credential")
	chunked, err := http.NewRequest(http.MethodPut, f.server.URL+"/v1/clients/report/"+day, strings.NewReader(reportJSON+strings.Repeat(" ", 17)))
	if err != nil {
		t.Fatal(err)
	}
	chunked.ContentLength = -1
	chunked.TransferEncoding = []string{"chunked"}
	chunked.Header.Set("Content-Type", "application/json")
	chunked.Header.Set("Authorization", "Bearer "+secret)
	chunkedResponse, err := f.server.Client().Do(chunked)
	if err != nil {
		t.Fatal(err)
	}
	chunkedBody, err := io.ReadAll(chunkedResponse.Body)
	chunkedResponse.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	requireHTTPError(t, registrationResponse{status: chunkedResponse.StatusCode, header: chunkedResponse.Header, body: chunkedBody}, 413, "request_too_large")

	limited := reports.DefaultIngestionLimits()
	limited.GlobalBurst = 100
	g := reportSetup(t, defaultRegistrationSettings(), limited, nil)
	g.seed(t, secret)
	expectRevision(t, g.report(t, day, secret, reportJSON, nil), 200, "1")
	expectRevision(t, g.report(t, day, secret, reportJSON, nil), 200, "1")
	response := g.report(t, day, secret, reportJSON, nil)
	requireHTTPError(t, response, 429, "report_rate_limited")
	if response.header.Get("Retry-After") != "60" {
		t.Fatalf("retry-after: %s", response.header.Get("Retry-After"))
	}
	health, err := g.server.Client().Get(g.server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != 200 {
		t.Fatal("report limit affected health")
	}
	if got := g.send(t, validStartBody, testSecret(2), nil); got.status != 200 {
		t.Fatalf("report limit affected registration: %d %s", got.status, got.body)
	}
}

func TestReportHTTPDatabaseFailureNeverConfirms(t *testing.T) {
	f := reportSetup(t, defaultRegistrationSettings(), generousReportLimits(), nil)
	client := f.seed(t, testSecret(1))
	day := "2026-11-01"
	expectRevision(t, f.report(t, day, testSecret(1), reportJSON, nil), 200, "1")
	if _, err := f.db.Writer.Exec(`CREATE TRIGGER fail_report BEFORE UPDATE ON daily_stats WHEN NEW.model_id>0 BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(strings.Replace(reportJSON, `"revision":"1"`, `"revision":"2"`, 1), `"total_tokens":"10"`, `"total_tokens":"11"`, 1)
	r := f.report(t, day, testSecret(1), changed, nil)
	requireHTTPError(t, r, 500, "internal_error")
	if strings.Contains(string(r.body), "injected") || strings.Contains(string(r.body), testSecret(1)) || strings.Contains(string(r.body), changed) {
		t.Fatal("database error leaked data")
	}
	var revision, sum int64
	if err := f.db.Reader.QueryRow(`SELECT revision FROM daily_writer_versions WHERE client_id=?`, client.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if err := f.db.Reader.QueryRow(`SELECT sum(total_tokens) FROM daily_stats WHERE client_id=?`, client.ID).Scan(&sum); err != nil {
		t.Fatal(err)
	}
	if revision != 1 || sum != 20 {
		t.Fatal("database failure left a partial report")
	}
}

type httpProcessor func(context.Context, int64, string, reports.Prepared) (int64, error)

func (p httpProcessor) Apply(ctx context.Context, id int64, day string, input reports.Prepared) (int64, error) {
	return p(ctx, id, day, input)
}

func TestReportHTTPQueueFullAndShutdown(t *testing.T) {
	started := make(chan struct{})
	f := reportSetup(t, defaultRegistrationSettings(), generousReportLimits(), func(_ *reports.Store) reports.SnapshotProcessor {
		return httpProcessor(func(ctx context.Context, _ int64, _ string, _ reports.Prepared) (int64, error) {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		})
	})
	f.seed(t, testSecret(1))
	first := make(chan registrationResponse, 1)
	go func() { first <- f.report(t, "2026-11-01", testSecret(1), reportJSON, nil) }()
	<-started
	second, err := f.writer.Enqueue(context.Background(), 1, "2026-11-01", reports.Prepared{})
	if err != nil {
		t.Fatal(err)
	}
	r := f.report(t, "2026-11-01", testSecret(1), reportJSON, nil)
	requireHTTPError(t, r, 503, "report_overloaded")
	if r.header.Get("Retry-After") != "1" {
		t.Fatal("missing overload retry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.writer.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown: %v", err)
	}
	requireHTTPError(t, <-first, 503, "report_overloaded")
	if result := <-second; !errors.Is(result.Err, context.Canceled) {
		t.Fatal(result.Err)
	}
	requireHTTPError(t, f.report(t, "2026-11-01", testSecret(1), reportJSON, nil), 503, "report_overloaded")
}
