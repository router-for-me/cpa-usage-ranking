package test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/registration"
)

func TestGitHubReturnToWithoutReferer(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	returns := []string{
		returnURL, "http://192.168.1.2:9000/path//keep?one=%2f&one=2#anchor",
		"https://[2001:db8::8]:8443/path/../keep?empty=#", "https://203.0.113.7/?",
		"https://public.example:9999/%E4%B8%AD?tab=x%20y#status", "http://internal/?one=2",
	}
	for i, dest := range returns {
		secret := testSecret(byte(i + 1))
		id := f.start(t, secret, serverID, dest)
		if i == 0 {
			// 超长 GitHub name 应安全截断至 32 个码点，而不是整段丢弃。
			f.approve(t, id, int64(i+1), strings.Repeat("x", 33), "fallback", dest)
			if got := identity(t, f.complete(t, secret, id)); got.Name != strings.Repeat("x", 32) {
				t.Fatalf("long GitHub name was not truncated: got %q", got.Name)
			}
			continue
		}
		// 空白或无可见字符的 GitHub name 仍应回退到 login。
		f.approve(t, id, int64(i+1), "   ", "fallback", dest)
		if got := identity(t, f.complete(t, secret, id)); got.Name != "fallback" {
			t.Fatalf("empty GitHub name did not fall back to login: got %q", got.Name)
		}
	}
	if calls, _ := f.github.counts(); calls != 2*len(returns) {
		t.Fatal("unexpected upstream calls")
	}
}

func TestGitHubDenialStateBrowserAndConcurrentReplay(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secret := testSecret(1)
	id := f.start(t, secret, serverID, returnURL)
	// 浏览器或聊天软件预取请求直接返回 204，不消耗也不轮换授权会话。
	for _, headers := range []map[string]string{
		{"Purpose": "prefetch"},
		{"Sec-Purpose": "Prefetch;anonymous-client-ip"},
	} {
		prefetch := f.api(t, "GET", "/auth/github/authorize?request_id="+id, "", "", headers)
		if prefetch.status != http.StatusNoContent || len(prefetch.body) != 0 || prefetch.header.Get("Cache-Control") != "no-store" || prefetch.header.Get("Set-Cookie") != "" {
			t.Fatalf("prefetch response: %d %q %+v", prefetch.status, prefetch.body, prefetch.header)
		}
	}
	firstBrowser := f.open(t, id)
	// 未进入回调核销前允许重复打开授权页，覆盖生成新的 state/cookie 并使旧会话失效。
	browser := f.open(t, id)
	if browser.state == firstBrowser.state || browser.cookie == firstBrowser.cookie {
		t.Fatal("reopened authorize did not rotate state and cookie")
	}
	requireHTTPError(t, f.callback(t, firstBrowser.state, firstBrowser.cookie, "error=access_denied"), 400, "invalid_request")
	for _, r := range []registrationResponse{
		f.callback(t, testSecret(7), browser.cookie, "error=access_denied"),
		f.callback(t, browser.state, "", "error=access_denied"),
		f.callback(t, browser.state, strings.SplitN(browser.cookie, "=", 2)[0]+"="+testSecret(4), "error=access_denied"),
		f.callback(t, browser.state, browser.cookie, "code=secret&error=access_denied"),
		f.callback(t, browser.state, browser.cookie, "code=secret&state="+browser.state),
	} {
		requireHTTPError(t, r, 400, "invalid_request")
		if r.header.Get("Location") != "" || r.header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("invalid callback redirected or exposed referrer")
		}
	}
	if calls, _ := f.github.counts(); calls != 0 || f.complete(t, secret, id).status != 202 {
		t.Fatal("invalid callback advanced authorization")
	}
	r := f.callback(t, browser.state, browser.cookie, "error=access_denied&error_description=ignored")
	if r.status != 302 || r.header.Get("Location") != returnURL || !strings.Contains(r.header.Get("Set-Cookie"), "Max-Age=0") {
		t.Fatal("valid denial did not record and redirect")
	}
	// 进入 denied 等终态后严格拒绝再次打开授权页。
	requireHTTPError(t, f.api(t, "GET", "/auth/github/authorize?request_id="+id, "", "", nil), 400, "invalid_request")
	requireHTTPError(t, f.complete(t, secret, id), 403, "github_authorization_denied")
	requireHTTPError(t, f.callback(t, browser.state, browser.cookie, "code=not-a-grant"), 400, "invalid_request")

	// authorize 允许无关附加查询参数（但忽略恶意 return_to），callback 对任意非空 error 按取消处理并重定向。
	extraID := f.start(t, secret, serverID, returnURL)
	requireHTTPError(t, f.api(t, "GET", "/auth/github/authorize?request_id="+extraID+"&request_id="+extraID, "", "", nil), 400, "invalid_request")
	extraResp := f.api(t, "GET", "/auth/github/authorize?request_id="+extraID+"&utm_source=test&return_to=https://evil.example", "", "", nil)
	if extraResp.status != 302 {
		t.Fatalf("authorize with extra query params failed: %d %s", extraResp.status, extraResp.body)
	}
	extraCookies := (&http.Response{Header: extraResp.header}).Cookies()
	extraLoc, _ := url.Parse(extraResp.header.Get("Location"))
	extraState := extraLoc.Query().Get("state")
	otherErrResp := f.callback(t, extraState, extraCookies[0].Name+"="+extraCookies[0].Value, "error=application_suspended&utm_campaign=gh")
	if otherErrResp.status != 302 || otherErrResp.header.Get("Location") != returnURL {
		t.Fatalf("non-access_denied OAuth error did not redirect back to saved return_to: %d %q %s", otherErrResp.status, otherErrResp.header.Get("Location"), otherErrResp.body)
	}
	requireHTTPError(t, f.complete(t, secret, extraID), 403, "github_authorization_denied")

	next := f.start(t, secret, serverID, returnURL)
	browser = f.open(t, next)
	f.github.setGrant("concurrent-code", fakeGrant{challenge: browser.challenge, userJSON: `{"id":42,"login":"test"}`})
	status := make(chan int, 8)
	var wg sync.WaitGroup
	for range cap(status) {
		wg.Go(func() { status <- f.callback(t, browser.state, browser.cookie, "code=concurrent-code").status })
	}
	wg.Wait()
	close(status)
	success := 0
	for s := range status {
		if s == 302 {
			success++
		} else if s != 400 {
			t.Fatalf("duplicate callback returned %d", s)
		}
	}
	if calls, tokens := f.github.counts(); success != 1 || calls != 2 || tokens != 1 {
		t.Fatal("callback replay caused repeated GitHub exchange")
	}
	identity(t, f.complete(t, secret, next))
}

func TestRegistrationCannotRebindWriterAndDifferentServerIDIsDistinct(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	httpserver.MountWithdraw(f.router, f.repo, nil)
	const otherServerID = "550e8400-e29b-41d4-a716-446655440001"
	secret := testSecret(1)
	id := f.start(t, secret, serverID, returnURL)
	f.approve(t, id, 10, "Name", "login", returnURL)
	one := identity(t, f.complete(t, secret, id))
	newPair := f.start(t, secret, otherServerID, returnURL)
	f.approve(t, newPair, 10, "Name", "login", returnURL)
	requireHTTPError(t, f.complete(t, secret, newPair), 401, "invalid_credential")
	var n int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM clients").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rebind created orphan seat: %d %v", n, err)
	}
	other := testSecret(2)
	twoID := f.start(t, other, otherServerID, returnURL)
	f.approve(t, twoID, 10, "Name", "login", returnURL)
	two := identity(t, f.complete(t, other, twoID))
	if two.ID == one.ID {
		t.Fatal("different server_id did not allocate another seat")
	}
	intoExisting := f.start(t, secret, otherServerID, returnURL)
	f.approve(t, intoExisting, 10, "Name", "login", returnURL)
	requireHTTPError(t, f.complete(t, secret, intoExisting), 401, "invalid_credential")

	if r := f.api(t, "POST", withdrawPath, "", other, nil); r.status != 204 {
		t.Fatalf("withdraw target seat: %d %s", r.status, r.body)
	}
	intoWithdrawn := f.start(t, secret, otherServerID, returnURL)
	f.approve(t, intoWithdrawn, 10, "Name", "login", returnURL)
	requireHTTPError(t, f.complete(t, secret, intoWithdrawn), 401, "invalid_credential")
	requireSeatWriterState(t, f, two.ID, true, 1)
	if got := identity(t, f.complete(t, secret, id)); got.ID != one.ID {
		t.Fatal("rebind replaced original writer")
	}
}

func TestExpiredGitHubCallbackDoesNotReadOrRedirect(t *testing.T) {
	settings := defaultRegistrationSettings()
	var now atomic.Int64
	now.Store(time.Now().UnixMilli())
	settings.service.TTL = time.Minute
	settings.service.Now = func() time.Time { return time.UnixMilli(now.Load()) }
	f := newRegistrationFixture(t, settings)
	secret := testSecret(1)
	id := f.start(t, secret, serverID, returnURL)
	browser := f.open(t, id)
	now.Add(settings.service.TTL.Milliseconds())
	r := f.callback(t, browser.state, browser.cookie, "code=late-code")
	requireHTTPError(t, r, 400, "invalid_request")
	if calls, _ := f.github.counts(); r.header.Get("Location") != "" || calls != 0 {
		t.Fatal("expired callback used GitHub or redirected")
	}
	requireHTTPError(t, f.complete(t, secret, id), 410, "registration_request_expired")
}

func TestGitHubFailureClosedAndNotRetried(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secret := testSecret(1)
	id := f.start(t, secret, serverID, returnURL)
	browser := f.open(t, id)
	f.github.setGrant("redirect-code", fakeGrant{challenge: browser.challenge, tokenStatus: 307})
	r := f.callback(t, browser.state, browser.cookie, "code=redirect-code")
	requireHTTPError(t, r, 500, "internal_error")
	if calls, _ := f.github.counts(); r.header.Get("Location") != "" || calls != 1 {
		t.Fatal("upstream redirect was followed or callback redirected on failure")
	}
	requireHTTPError(t, f.complete(t, secret, id), 500, "internal_error")
	requireHTTPError(t, f.callback(t, browser.state, browser.cookie, "code=redirect-code"), 400, "invalid_request")
	var row int
	if err := f.db.Reader.QueryRowContext(context.Background(), "SELECT id FROM clients").Scan(&row); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("upstream failure created a seat")
	}
}

func TestGitHubOptionalReturnToRendersStaticCompletionPage(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secret := testSecret(1)
	for _, body := range []string{
		`{"server_id":"` + serverID + `"}`,
		`{"server_id":"` + serverID + `","return_to":null}`,
		`{"server_id":"` + serverID + `","return_to":"   "}`,
	} {
		id := f.startWithBody(t, secret, body)
		browser := f.open(t, id)
		f.github.setGrant("ok-code", fakeGrant{challenge: browser.challenge, userJSON: `{"id":50,"login":"headless"}`})
		cb := f.callback(t, browser.state, browser.cookie, "code=ok-code")
		if cb.status != http.StatusOK ||
			!strings.HasPrefix(cb.header.Get("Content-Type"), "text/html; charset=utf-8") ||
			cb.header.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'" ||
			cb.header.Get("Location") != "" ||
			!strings.Contains(string(cb.body), "Authorization Complete") ||
			!strings.Contains(string(cb.body), "授权已完成") {
			t.Fatalf("approved static page: status=%d header=%+v body=%q", cb.status, cb.header, cb.body)
		}
		if got := identity(t, f.complete(t, secret, id)); got.Name != "headless" {
			t.Fatalf("complete headless: %+v", got)
		}
	}
	// 无 return_to 时取消授权也返回 200 静态提示页，并标记为 denied。
	deniedID := f.startWithBody(t, secret, `{"server_id":"`+serverID+`"}`)
	browser := f.open(t, deniedID)
	cb := f.callback(t, browser.state, browser.cookie, "error=access_denied")
	if cb.status != http.StatusOK ||
		!strings.HasPrefix(cb.header.Get("Content-Type"), "text/html; charset=utf-8") ||
		cb.header.Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'" ||
		!strings.Contains(string(cb.body), "Authorization Cancelled") ||
		!strings.Contains(string(cb.body), "授权已取消") {
		t.Fatalf("denied static page: status=%d header=%+v body=%q", cb.status, cb.header, cb.body)
	}
	requireHTTPError(t, f.complete(t, secret, deniedID), 403, "github_authorization_denied")
}

func TestGitHubLoopbackHTTPDowngradesCookieNameAndSecureFlag(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	gh, err := registration.NewGitHub(registration.GitHubOptions{
		PublicOrigin: "http://127.0.0.1:8080",
		ClientID:     "loopback-id",
		ClientSecret: "loopback-secret",
	}, f.github)
	if err != nil {
		t.Fatal(err)
	}
	router := httpserver.NewRouter(quietLogger())
	limiter, err := registration.NewRequestLimiter(defaultRegistrationSettings().requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpserver.MountRegistration(router, f.reg, gh, limiter, httpserver.RegistrationHTTPOptions{MaxBodyBytes: 4096}); err != nil {
		t.Fatal(err)
	}
	id := f.start(t, testSecret(9), serverID, "")
	req, _ := http.NewRequest(http.MethodGet, "/auth/github/authorize?request_id="+id, nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("loopback authorize: %d %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0].Name, "ranking-github-") || cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("loopback cookie: %+v", cookies)
	}
}

func TestGitHubAuthorizeReopenRateLimitedPerRequestID(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	id := f.start(t, testSecret(1), serverID, returnURL)
	otherID := f.start(t, testSecret(2), serverID, returnURL)

	// Prefetch requests must not consume authorize slots.
	for range 5 {
		resp := f.api(t, "GET", "/auth/github/authorize?request_id="+id, "", "", map[string]string{"Purpose": "prefetch"})
		if resp.status != http.StatusNoContent {
			t.Fatalf("prefetch status: %d", resp.status)
		}
	}
	last := f.open(t, id)
	for i := 1; i < 6; i++ {
		last = f.open(t, id)
		if last.state == "" {
			t.Fatalf("open %d produced empty state", i)
		}
	}
	limited := f.api(t, "GET", "/auth/github/authorize?request_id="+id, "", "", nil)
	requireHTTPError(t, limited, http.StatusTooManyRequests, "registration_rate_limited")
	if limited.header.Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header on rate-limited authorize")
	}
	// Even if one source exhausts its full per-minute start budget (up to 18 more requests here) and hits per-ID reopen limits, another user's first open still succeeds.
	for i := range 18 {
		batchID := f.start(t, testSecret(byte(10+i)), serverID, returnURL)
		for range 6 {
			_ = f.open(t, batchID)
		}
		requireHTTPError(t, f.api(t, "GET", "/auth/github/authorize?request_id="+batchID, "", "", nil), http.StatusTooManyRequests, "registration_rate_limited")
	}
	_ = f.open(t, otherID)
	f.github.setGrant("after-limit", fakeGrant{challenge: last.challenge, userJSON: `{"id":99,"login":"ok"}`})
	if cb := f.callback(t, last.state, last.cookie, "code=after-limit"); cb.status != http.StatusFound {
		t.Fatalf("callback after rate-limited reopen: %d %s", cb.status, cb.body)
	}
}
