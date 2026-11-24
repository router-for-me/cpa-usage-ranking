package test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	browser := f.open(t, id)
	requireHTTPError(t, f.api(t, "GET", "/auth/github/authorize?request_id="+id+"&return_to=https://evil.example", "", "", nil), 400, "invalid_request")
	requireHTTPError(t, f.api(t, "GET", "/auth/github/authorize?request_id="+id, "", "", nil), 400, "invalid_request")
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
