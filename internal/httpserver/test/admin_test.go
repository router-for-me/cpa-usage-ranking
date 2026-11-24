package test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/admin"
	"cpa-usage-ranking/internal/httpserver"
	"github.com/gin-gonic/gin"
)

func adminRouter(t *testing.T, enabled bool, options httpserver.AdminHTTPOptions) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpserver.NewRouter(logger)
	var service *admin.Service
	if enabled {
		var err error
		service, err = admin.New(admin.Owner{Username: "owner", Password: "isolated-test-password"}, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := httpserver.MountAdmin(router, service, nil, logger, options); err != nil {
		t.Fatal(err)
	}
	return router
}

func adminCall(router http.Handler, method, path, body, cookie, csrf, origin string, secure bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://ranking.test"+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Set("Content-Type", "application/json")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if secure {
		request.TLS = &tls.ConnectionState{}
	} else {
		request.TLS = nil
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestAdminDisabledAndAuthentication(t *testing.T) {
	disabled := adminRouter(t, false, httpserver.AdminHTTPOptions{})
	status := adminCall(disabled, "GET", "/v1/admin/session", "", "", "", "", false)
	if status.Code != 200 || strings.Contains(status.Body.String(), "user") || !strings.Contains(status.Body.String(), `"enabled":false`) {
		t.Fatal(status.Body.String())
	}
	for _, path := range []string{"/admin/login", "/admin/leaderboards", "/v1/admin/users"} {
		if got := adminCall(disabled, "GET", path, "", "", "", "", false).Code; got != 404 {
			t.Fatalf("disabled %s: %d", path, got)
		}
	}
	router := adminRouter(t, true, httpserver.AdminHTTPOptions{})
	body := `{"username":"owner","password":"isolated-test-password"}`
	if got := adminCall(router, "POST", "/v1/admin/session", body, "", "", "https://evil.test", true).Code; got != 403 {
		t.Fatal(got)
	}
	if got := adminCall(router, "POST", "/v1/admin/session", body, "", "", "", true).Code; got != 403 {
		t.Fatal(got)
	}
	if got := adminCall(router, "POST", "/v1/admin/session", body, "", "", "https://ranking.test", false).Code; got != 403 {
		t.Fatal("plaintext accepted")
	}
	wrong := adminCall(router, "POST", "/v1/admin/session", `{"username":"missing","password":"wrong"}`, "", "", "https://ranking.test", true)
	if wrong.Code != 401 || strings.Contains(wrong.Body.String(), "missing") {
		t.Fatal(wrong.Body.String())
	}
	logged := adminCall(router, "POST", "/v1/admin/session", body, "", "", "https://ranking.test", true)
	if logged.Code != 200 {
		t.Fatal(logged.Body.String())
	}
	cookies := logged.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].MaxAge != 28800 || cookies[0].Path != "/" {
		t.Fatal("unsafe cookie")
	}
	cookie := cookies[0].Name + "=" + cookies[0].Value
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(logged.Body.Bytes(), &session); err != nil || session.CSRF == "" {
		t.Fatal(err)
	}
	if got := adminCall(router, "DELETE", "/v1/admin/session", "{}", cookie, "", "https://ranking.test", true).Code; got != 403 {
		t.Fatal("missing csrf accepted")
	}
	if got := adminCall(router, "DELETE", "/v1/admin/session", "{}", cookie, session.CSRF, "https://ranking.test", true).Code; got != 204 {
		t.Fatal(got)
	}
	state := adminCall(router, "GET", "/v1/admin/session", "", cookie, "", "", true)
	if strings.Contains(state.Body.String(), `"authenticated":true`) || strings.Contains(state.Body.String(), "csrf_token") {
		t.Fatal("logout retained identity")
	}
	request := httptest.NewRequest("DELETE", "https://ranking.test/v1/admin/session", bytes.NewBufferString("{}"))
	request.Header.Set("Origin", "https://ranking.test")
	request.Header.Set("Authorization", "Bearer client-secret-not-a-session")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("bearer authorized administration", response.Code)
	}
}

func TestAdminLoginLimitsAndStrictJSON(t *testing.T) {
	router := adminRouter(t, true, httpserver.AdminHTTPOptions{})
	for range 5 {
		if got := adminCall(router, "POST", "/v1/admin/session", `{"username":"owner","password":"wrong","owner":true}`, "", "", "https://ranking.test", true).Code; got != 400 {
			t.Fatal(got)
		}
	}
	limited := adminCall(router, "POST", "/v1/admin/session", `{}`, "", "", "https://ranking.test", true)
	if limited.Code != 429 || limited.Header().Get("Retry-After") == "" {
		t.Fatal("not limited")
	}
	// Login counters are independent of public registration counters.
	if limited.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cached management response")
	}
}

func TestAdminTrustedHTTPSAndLocalException(t *testing.T) {
	for _, scenario := range []struct {
		trusted                   []string
		allow                     bool
		peer, host, origin, proto string
		want                      int
	}{
		{peer: "192.0.2.2:1", host: "ranking.test", origin: "https://ranking.test", proto: "https", want: 403},
		{trusted: []string{"192.0.2.0/24"}, peer: "192.0.2.2:1", host: "ranking.test", origin: "https://ranking.test", proto: "https", want: 200},
		{allow: true, peer: "127.0.0.1:1", host: "127.0.0.1:4188", origin: "http://127.0.0.1:4188", want: 200},
		{allow: true, peer: "192.0.2.2:1", host: "127.0.0.1:4188", origin: "http://127.0.0.1:4188", want: 200},
		{allow: true, peer: "127.0.0.1:1", host: "public.test", origin: "http://public.test", want: 200},
		{allow: true, peer: "192.0.2.2:1", host: "public.test", origin: "http://evil.test", want: 403},
	} {
		router := adminRouter(t, true, httpserver.AdminHTTPOptions{AllowLocalHTTP: scenario.allow, TrustedProxies: scenario.trusted})
		request := httptest.NewRequest("POST", "http://"+scenario.host+"/v1/admin/session", strings.NewReader(`{"username":"owner","password":"isolated-test-password"}`))
		request.RemoteAddr = scenario.peer
		request.Header.Set("Origin", scenario.origin)
		request.Header.Set("X-Forwarded-Proto", scenario.proto)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if response.Code != scenario.want {
			t.Fatalf("%+v: %d", scenario, response.Code)
		}
		if response.Code == 200 && response.Result().Cookies()[0].Secure == scenario.allow {
			t.Fatal("cookie security mismatch")
		}
	}
}
