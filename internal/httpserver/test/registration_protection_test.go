package test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/httpserver"
)

func TestRegistrationHTTPConcurrentSafeRetries(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secrets := []string{testSecret(1), testSecret(2)}
	var ids []string
	for _, secret := range secrets {
		id := f.start(t, secret, serverID, returnURL)
		f.approve(t, id, 42, "Concurrent", "concurrent", returnURL)
		ids = append(ids, id)
	}
	const n = 24
	results := make(chan registrationResponse, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { results <- f.complete(t, secrets[i%2], ids[i%2]) })
	}
	wg.Wait()
	close(results)
	var clientID int64
	created := 0
	for r := range results {
		value := identity(t, r)
		if r.status == 201 {
			created++
		}
		if clientID == 0 {
			clientID = value.ID
		}
		if value.ID != clientID {
			t.Fatal("concurrent completion allocated another seat")
		}
	}
	var seats, writers int
	if err := f.db.Reader.QueryRow("SELECT (SELECT count(*) FROM clients),(SELECT count(*) FROM client_writers)").Scan(&seats, &writers); err != nil || seats != 1 || writers != 2 || created != 1 {
		t.Fatalf("created=%d seats=%d writers=%d err=%v", created, seats, writers, err)
	}
}

func TestRegistrationHTTPConcurrentStartsAndForgedProxies(t *testing.T) {
	settings := defaultRegistrationSettings()
	settings.requests.SourceRequests = 3
	f := newRegistrationFixture(t, settings)
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := byte(1); i <= 16; i++ {
		wg.Go(func() {
			r := f.send(t, validStartBody, testSecret(i), map[string]string{"X-Forwarded-For": fmt.Sprintf("192.0.2.%d", i), "X-Real-IP": fmt.Sprintf("198.51.100.%d", i)})
			if r.status == 200 {
				accepted.Add(1)
				return
			}
			requireHTTPError(t, r, 429, "registration_rate_limited")
		})
	}
	wg.Wait()
	if accepted.Load() != 3 {
		t.Fatalf("forged headers bypassed source rate: %d", accepted.Load())
	}
}

func TestRegistrationHTTPRequestRateAndCapacity(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		global, source, capacity int
		trusted                  bool
		secondCode               string
	}{
		{"source including failed attempts", 100, 1, 10, false, "registration_rate_limited"},
		{"global across sources", 1, 100, 10, true, "registration_rate_limited"},
		{"capacity cannot evict", 100, 1, 1, true, "registration_capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := defaultRegistrationSettings()
			settings.requests.SourceRequests = tc.source
			settings.requests.GlobalRequests = tc.global
			settings.requests.MaxSources = tc.capacity
			var now atomic.Int64
			now.Store(time.Now().UnixNano())
			settings.service.Now = func() time.Time { return time.Unix(0, now.Load()) }
			if tc.trusted {
				settings.proxies = []string{"127.0.0.1"}
			}
			f := newRegistrationFixture(t, settings)
			requireHTTPError(t, f.send(t, `{}`, "", map[string]string{"X-Forwarded-For": "192.0.2.1"}), 401, "invalid_credential")
			r := f.send(t, validStartBody, testSecret(1), map[string]string{"X-Forwarded-For": "192.0.2.2"})
			requireHTTPError(t, r, 429, tc.secondCode)
			if r.header.Get("Retry-After") != "60" {
				t.Fatal("wrong rate/capacity Retry-After")
			}
			if tc.secondCode == "registration_capacity" {
				requireHTTPError(t, f.send(t, `{}`, "", map[string]string{"X-Forwarded-For": "192.0.2.1"}), 429, "registration_rate_limited")
			}
			for _, path := range []string{"/healthz", "/avatars/catalog.json", "/avatars/images/001.webp"} {
				response, err := f.server.Client().Get(f.server.URL + path)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if response.StatusCode != 200 {
					t.Fatalf("registration affected %s", path)
				}
			}
			now.Add(int64(time.Minute))
			if r := f.send(t, validStartBody, testSecret(1), map[string]string{"X-Forwarded-For": "192.0.2.2"}); r.status != 200 {
				t.Fatalf("expired request rate state: %d %s", r.status, r.body)
			}
		})
	}
}

func TestRegistrationHTTPCapacityReclaimsCompletedAndCapsActive(t *testing.T) {
	settings := defaultRegistrationSettings()
	settings.requests.SourceRequests = 1
	settings.service.Capacity = 1
	settings.service.TTL = 90 * time.Second
	var now atomic.Int64
	now.Store(time.Now().Truncate(time.Second).UnixNano())
	settings.service.Now = func() time.Time { return time.Unix(0, now.Load()) }
	f := newRegistrationFixture(t, settings)
	secret := testSecret(1)
	id := f.start(t, secret, serverID, returnURL)
	const login = "this-is-a-github-login-longer-than-32"
	f.approve(t, id, 7, "", login, returnURL)
	first := identity(t, f.complete(t, secret, id))
	if first.Name != login[:32] {
		t.Fatalf("invalid login fallback: %q", first.Name)
	}
	// 未被淘汰前，已完成请求仍可幂等取回结果。
	if r := f.complete(t, secret, id); r.status != 200 || identity(t, r).ID != first.ID {
		t.Fatal("exhausted start budget blocked an unexpired recovery")
	}
	now.Add(int64(time.Minute))
	// 表满且仅有已完成行时，新注册应淘汰最早的已完成行并正常签发。
	activeID := f.start(t, testSecret(2), serverID, returnURL)
	requireHTTPError(t, f.complete(t, secret, id), 410, "registration_request_expired")
	// 活跃未完成请求占满容量时，后续请求必须返回 429 registration_capacity 且不驱逐活跃行。
	now.Add(int64(time.Minute))
	r := f.send(t, validStartBody, testSecret(3), nil)
	requireHTTPError(t, r, 429, "registration_capacity")
	if r.header.Get("Retry-After") != "30" {
		t.Fatalf("capacity retry time: %v", r.header)
	}
	if f.complete(t, testSecret(2), activeID).status != 202 {
		t.Fatal("active request was disrupted by capacity check")
	}
	now.Add(int64(30 * time.Second))
	requireHTTPError(t, f.complete(t, testSecret(2), activeID), 410, "registration_request_expired")
	if err := f.reg.PruneExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	now.Add(int64(30 * time.Second))
	fresh := f.start(t, secret, serverID, returnURL)
	f.approve(t, fresh, 7, "Changed", "different-login", returnURL)
	if r := f.complete(t, secret, fresh); r.status != 200 || identity(t, r) != first {
		t.Fatal("same seat recovery after expiry failed")
	}
	var writers int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM client_writers").Scan(&writers); err != nil || writers != 1 {
		t.Fatalf("lost response added another writer: %d %v", writers, err)
	}
}

func TestRegistrationHTTPClosedDisabledWithdrawnAndStorageFailure(t *testing.T) {
	settings := defaultRegistrationSettings()
	settings.service.Enabled = false
	closed := newRegistrationFixture(t, settings)
	requireHTTPError(t, closed.send(t, validStartBody, testSecret(1), nil), 503, "registration_closed")
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	httpserver.MountWithdraw(f.router, f.repo, nil)
	secret := testSecret(1)
	id := f.start(t, secret, serverID, returnURL)
	f.approve(t, id, 9, "Name", "test", returnURL)
	first := identity(t, f.complete(t, secret, id))
	if err := f.repo.SetDisabled(context.Background(), first.ID, true); err != nil {
		t.Fatal(err)
	}
	requireHTTPError(t, f.complete(t, secret, id), 403, "client_disabled")
	other := f.start(t, testSecret(2), serverID, returnURL)
	f.approve(t, other, 9, "Other", "test", returnURL)
	requireHTTPError(t, f.complete(t, testSecret(2), other), 403, "client_disabled")
	if r := f.api(t, "POST", withdrawPath, "", secret, nil); r.status != 204 {
		t.Fatalf("disabled holder couldn't withdraw: %d %s", r.status, r.body)
	}
	requireHTTPError(t, f.complete(t, secret, id), 403, "client_withdrawn")
	requireHTTPError(t, f.complete(t, testSecret(2), other), 403, "client_disabled")
	third := f.start(t, secret, serverID, returnURL)
	f.approve(t, third, 9, "Other", "test", returnURL)
	requireHTTPError(t, f.complete(t, secret, third), 403, "client_disabled")
	seat, err := f.repo.ByID(context.Background(), first.ID)
	if err != nil || !seat.Withdrawn || !seat.Disabled {
		t.Fatal("withdraw or retry changed the management flag")
	}
	var writers int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM client_writers").Scan(&writers); err != nil || writers != 1 {
		t.Fatalf("banned seat: writers=%d err=%v", writers, err)
	}
	if err := f.db.Writer.Close(); err != nil {
		t.Fatal(err)
	}
	requireHTTPError(t, f.complete(t, secret, third), 500, "internal_error")
}

func TestRegistrationHTTPRejoinWithdrawnSeat(t *testing.T) {
	original := testSecret(1)
	for _, tc := range []struct{ name, secret string }{{"original bearer", original}, {"new bearer", testSecret(2)}} {
		t.Run(tc.name, func(t *testing.T) {
			var refreshes atomic.Int32
			settings := defaultRegistrationSettings()
			settings.service.RequestRefresh = func() { refreshes.Add(1) }
			f := newRegistrationFixture(t, settings)
			httpserver.MountWithdraw(f.router, f.repo, nil)
			id := f.startWithBody(t, original, startBodyWithAvatar(serverID, returnURL, "66"))
			f.approve(t, id, 9, "Original", "test", returnURL)
			first := identity(t, f.complete(t, original, id))
			if _, err := f.db.Writer.Exec("UPDATE clients SET ranking_hidden=1 WHERE id=?", first.ID); err != nil {
				t.Fatal(err)
			}
			if r := f.api(t, "POST", withdrawPath, "", original, nil); r.status != 204 {
				t.Fatalf("withdraw: %d %s", r.status, r.body)
			}
			requireHTTPError(t, f.complete(t, original, id), 403, "client_withdrawn")
			if got := refreshes.Load(); got != 0 {
				t.Fatalf("registration refreshes before rejoin: %d", got)
			}

			fresh := f.start(t, tc.secret, serverID, returnURL)
			f.approve(t, fresh, 9, "Changed", "test", returnURL)
			r := f.complete(t, tc.secret, fresh)
			if r.status != 200 || identity(t, r) != first || refreshes.Load() != 1 {
				t.Fatalf("rejoin: %d %s, refreshes=%d", r.status, r.body, refreshes.Load())
			}
			authenticator := auth.NewAuthenticator(f.repo)
			for _, secret := range []string{original, tc.secret} {
				writer, err := authenticator.Authenticate(t.Context(), http.Header{"Authorization": {"Bearer " + secret}})
				if err != nil || writer.ID != first.ID || !writer.RankingHidden {
					t.Fatalf("writer after rejoin: %+v, err=%v", writer, err)
				}
			}
			if identity(t, f.complete(t, tc.secret, fresh)) != first || f.register(t, tc.secret, 9, "Changed") != first || refreshes.Load() != 1 {
				t.Fatalf("retry or active seat reuse changed identity or requested another refresh (count=%d)", refreshes.Load())
			}
		})
	}
}

func TestRegistrationHTTPBodyBoundForKnownAndChunkedLength(t *testing.T) {
	settings := defaultRegistrationSettings()
	settings.bodyBytes = int64(len(validStartBody))
	f := newRegistrationFixture(t, settings)
	large := validStartBody + strings.Repeat(" ", 128)
	requireHTTPError(t, f.send(t, large, testSecret(1), nil), 413, "request_too_large")
	request, err := http.NewRequest("POST", f.server.URL+startPath, io.NopCloser(strings.NewReader(large)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testSecret(1))
	response, err := f.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	requireHTTPError(t, registrationResponse{status: response.StatusCode, body: body, header: response.Header}, 413, "request_too_large")
	f.start(t, testSecret(1), serverID, returnURL)
}
