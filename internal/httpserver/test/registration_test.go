package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/avatars"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/httpserver"
	"cpa-usage-ranking/internal/registration"
	"cpa-usage-ranking/internal/storage"
	"cpa-usage-ranking/internal/testfixture"
	"github.com/gin-gonic/gin"
)

const (
	startPath    = "/v1/clients/register/start"
	completePath = "/v1/clients/register/complete"
	withdrawPath = "/v1/clients/withdraw"
	cpaUUID      = "550e8400-e29b-41d4-a716-446655440000"
	returnURL    = "http://localhost:8080/settings?tab=ranking&aaa=xxx#status"
)

var validStartBody = startBody(cpaUUID, returnURL)

func startBody(uuid, dest string) string {
	return fmt.Sprintf(`{"cpa_uuid":%q,"return_to":%q}`, uuid, dest)
}

func startBodyWithAvatar(uuid, dest, avatar string) string {
	return fmt.Sprintf(`{"cpa_uuid":%q,"return_to":%q,"avatar_id":%q}`, uuid, dest, avatar)
}

type registrationSettings struct {
	service   registration.Options
	requests  registration.RequestLimits
	proxies   []string
	bodyBytes int64
}

func defaultRegistrationSettings() registrationSettings {
	return registrationSettings{service: registration.DefaultOptions(), requests: registration.RequestLimits{SourceRequests: 1000, GlobalRequests: 10000, Window: registration.DefaultRequestLimits().Window, MaxSources: 100}, bodyBytes: 4096}
}

type registrationLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *registrationLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *registrationLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type registrationFixture struct {
	db     *storage.DB
	repo   *clients.Repository
	router *gin.Engine
	server *httptest.Server
	logs   registrationLog
	github *fakeGitHub
	reg    *registration.Service
}

func newRegistrationFixture(t *testing.T, settings registrationSettings) *registrationFixture {
	t.Helper()
	f := &registrationFixture{}
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "http-registration.db"))
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	f.repo = clients.NewRepository(db)
	files := avatars.BuiltinFS()
	catalog, err := avatars.Load(files)
	if err != nil {
		t.Fatal(err)
	}
	f.router = httpserver.NewRouter(slog.New(slog.NewJSONHandler(&f.logs, nil)))
	if err := f.router.SetTrustedProxies(settings.proxies); err != nil {
		t.Fatal(err)
	}
	if err := httpserver.MountAvatars(f.router, catalog, files); err != nil {
		t.Fatal(err)
	}
	f.mountRegistration(t, settings, catalog)
	// 仅测试异常恢复，生产路由没有该端点。
	f.router.POST("/test/secret-panic", func(c *gin.Context) { panic(c.GetHeader("Authorization")) })
	f.server = httptest.NewServer(f.router)
	t.Cleanup(f.server.Close)
	return f
}

func (f *registrationFixture) mountRegistration(t *testing.T, settings registrationSettings, catalog *avatars.Catalog) {
	t.Helper()
	var err error
	f.reg, err = registration.NewService(f.db, catalog, settings.service)
	if err != nil {
		t.Fatal(err)
	}
	limiter, err := registration.NewRequestLimiter(settings.requests, settings.service.Now)
	if err != nil {
		t.Fatal(err)
	}
	f.github = &fakeGitHub{t: t, grants: make(map[string]fakeGrant)}
	github, err := registration.NewGitHub(registration.GitHubOptions{
		PublicOrigin: "https://ranking.example:9443", ClientID: "test-app", ClientSecret: "test-app-secret",
	}, f.github)
	if err != nil {
		t.Fatal(err)
	}
	if err := httpserver.MountRegistration(f.router, f.reg, github, limiter, httpserver.RegistrationHTTPOptions{GroupIPv6By64: true, MaxBodyBytes: settings.bodyBytes}); err != nil {
		t.Fatal(err)
	}
}

type fakeGrant struct {
	challenge, userJSON string
	tokenStatus         int
}

type fakeGitHub struct {
	t      *testing.T
	mu     sync.Mutex
	grants map[string]fakeGrant
	calls  int
	tokens int
}

func (g *fakeGitHub) setGrant(code string, grant fakeGrant) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.grants[code] = grant
}

func (g *fakeGitHub) counts() (calls, tokens int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.tokens
}

func (g *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	var status, body = http.StatusOK, ""
	switch r.URL.String() {
	case "https://github.com/login/oauth/access_token":
		g.tokens++
		if err := r.ParseForm(); err != nil {
			return nil, err
		}
		code := r.Form.Get("code")
		grant, ok := g.grants[code]
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || r.Method != "POST" || r.Form.Get("redirect_uri") != "https://ranking.example:9443/auth/github/callback" || r.Form.Get("client_id") != "test-app" || r.Form.Get("client_secret") != "test-app-secret" ||
			len(r.Form.Get("code_verifier")) != 43 || base64.RawURLEncoding.EncodeToString(challenge[:]) != grant.challenge {
			g.t.Error("incorrect fixed OAuth form or PKCE")
			return nil, fmt.Errorf("invalid test grant")
		}
		body = fmt.Sprintf(`{"access_token":%q}`, "fake-token-"+code)
		if grant.tokenStatus != 0 {
			status, body = grant.tokenStatus, `{"error":"upstream-secret"}`
		}
	case "https://api.github.com/user":
		code, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer fake-token-")
		if !ok || r.Method != "GET" || r.Header.Get("Referer") != "" {
			g.t.Error("invalid GitHub user request")
			return nil, fmt.Errorf("invalid test authorization")
		}
		body = g.grants[code].userJSON
	default:
		g.t.Error("unexpected OAuth destination")
		return nil, fmt.Errorf("unexpected OAuth destination")
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://destination.example/never-follow"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func testSecret(value byte) string {
	raw := make([]byte, 32)
	raw[0] = value
	return base64.RawURLEncoding.EncodeToString(raw)
}

type registrationResponse struct {
	status int
	header http.Header
	body   []byte
}

func (f *registrationFixture) api(t *testing.T, method, path, body, secret string, headers map[string]string) registrationResponse {
	t.Helper()
	var input io.Reader
	if body != "" {
		input = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, f.server.URL+path, input)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if secret != "" {
		r.Header.Set("Authorization", "Bearer "+secret)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	client := *f.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("response must not be cached")
	}
	return registrationResponse{status: response.StatusCode, header: response.Header, body: data}
}

func (f *registrationFixture) send(t *testing.T, body, secret string, headers map[string]string) registrationResponse {
	t.Helper()
	return f.api(t, "POST", startPath, body, secret, headers)
}

func (f *registrationFixture) startWithBody(t *testing.T, secret, body string) string {
	t.Helper()
	r := f.send(t, body, secret, nil)
	var reply struct {
		RequestID string `json:"request_id"`
		Authorize string `json:"authorize_url"`
	}
	if r.status != 200 || json.Unmarshal(r.body, &reply) != nil || len(reply.RequestID) != 43 || reply.Authorize != "https://ranking.example:9443/auth/github/authorize?request_id="+reply.RequestID {
		t.Fatalf("unexpected registration start response: %d %s", r.status, r.body)
	}
	return reply.RequestID
}

func (f *registrationFixture) start(t *testing.T, secret, uuid, dest string) string {
	t.Helper()
	return f.startWithBody(t, secret, startBody(uuid, dest))
}

func (f *registrationFixture) register(t *testing.T, secret string, githubID int64, name string) publicIdentity {
	t.Helper()
	id := f.start(t, secret, cpaUUID, returnURL)
	f.approve(t, id, githubID, name, "test", returnURL)
	return identity(t, f.complete(t, secret, id))
}

func (f *registrationFixture) complete(t *testing.T, secret, id string) registrationResponse {
	t.Helper()
	return f.api(t, "POST", completePath, fmt.Sprintf(`{"request_id":%q}`, id), secret, nil)
}

type browserGrant struct{ state, cookie, challenge string }

func (f *registrationFixture) open(t *testing.T, id string) browserGrant {
	t.Helper()
	r := f.api(t, "GET", "/auth/github/authorize?request_id="+id, "", "", nil)
	if r.status != 302 || r.header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("authorization response: %d %s", r.status, r.body)
	}
	to, err := url.Parse(r.header.Get("Location"))
	if err != nil || to.Scheme != "https" || to.Host != "github.com" || to.Path != "/login/oauth/authorize" {
		t.Fatal("not the fixed authorize endpoint")
	}
	q := to.Query()
	if len(q) != 5 || q.Get("client_id") != "test-app" || q.Get("redirect_uri") != "https://ranking.example:9443/auth/github/callback" || q.Get("code_challenge_method") != "S256" || len(q.Get("state")) != 43 || len(q.Get("code_challenge")) != 43 {
		t.Fatalf("authorization query: %v", q)
	}
	cookies := (&http.Response{Header: r.header}).Cookies()
	if len(cookies) != 1 || !strings.HasPrefix(cookies[0].Name, "__Host-") || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Domain != "" || cookies[0].Path != "/" || cookies[0].Value == q.Get("state") {
		t.Fatal("invalid browser binding cookie")
	}
	return browserGrant{state: q.Get("state"), cookie: cookies[0].Name + "=" + cookies[0].Value, challenge: q.Get("code_challenge")}
}

func (f *registrationFixture) approve(t *testing.T, id string, githubID int64, name, login, dest string) {
	t.Helper()
	browser := f.open(t, id)
	code := "test-" + id
	user, err := json.Marshal(map[string]any{"id": githubID, "name": name, "login": login})
	if err != nil {
		t.Fatal(err)
	}
	f.github.setGrant(code, fakeGrant{challenge: browser.challenge, userJSON: string(user)})
	r := f.callback(t, browser.state, browser.cookie, "code="+code)
	if r.status != 302 || r.header.Get("Location") != dest || r.header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("callback did not preserve return_to: %d %q %s", r.status, r.header.Get("Location"), r.body)
	}
}

func (f *registrationFixture) callback(t *testing.T, state, cookie, result string) registrationResponse {
	t.Helper()
	return f.api(t, "GET", "/auth/github/callback?state="+url.QueryEscape(state)+"&"+result, "", "", map[string]string{"Cookie": cookie})
}

type publicIdentity struct {
	ID       int64  `json:"client_id,string"`
	Name     string `json:"display_name"`
	AvatarID int64  `json:"avatar_id,string"`
}

func identity(t *testing.T, r registrationResponse) publicIdentity {
	t.Helper()
	if r.status != 200 && r.status != 201 {
		t.Fatalf("not a success: %d %s", r.status, r.body)
	}
	var result publicIdentity
	var fields map[string]any
	if json.Unmarshal(r.body, &result) != nil || json.Unmarshal(r.body, &fields) != nil || len(fields) != 3 || result.ID <= 0 || result.AvatarID <= 0 {
		t.Fatalf("unsafe public identity response: %s", r.body)
	}
	return result
}

func requireHTTPError(t *testing.T, r registrationResponse, status int, code string) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(r.body, &body); err != nil || r.status != status || body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("response: %d %s; want %d %s", r.status, r.body, status, code)
	}
}

func (f *registrationFixture) seed(t *testing.T, secret string) clients.Client {
	t.Helper()
	hash, err := auth.HashCredential(secret)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := testfixture.Seat(context.Background(), f.db.Writer, hash, "Seeded", 66)
	if err != nil {
		t.Fatal(err)
	}
	return writer.Client
}

func TestRegistrationHTTPCreateAndSafeRetry(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secret := testSecret(1)
	id := f.start(t, secret, cpaUUID, returnURL)
	requireHTTPError(t, f.complete(t, testSecret(2), id), 401, "invalid_credential")
	pending := f.complete(t, secret, id)
	if pending.status != 202 || pending.header.Get("Retry-After") == "" || len(pending.body) != 0 {
		t.Fatalf("premature completion: %+v", pending)
	}
	f.approve(t, id, 1<<54, "  Ａlice 🙂  ", "alice", returnURL)
	var seats int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM clients").Scan(&seats); err != nil || seats != 0 {
		t.Fatalf("browser flow created a seat before complete: %d %v", seats, err)
	}
	first := f.complete(t, secret, id)
	created := identity(t, first)
	if first.status != 201 || created.Name != "Alice 🙂" || created.AvatarID != 1 {
		t.Fatalf("new seat: %+v", created)
	}
	if err := f.repo.UpdateProfile(context.Background(), created.ID, "Current", 66); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		r := f.complete(t, secret, id)
		current := identity(t, r)
		if r.status != 200 || current.ID != created.ID || current.Name != "Current" || current.AvatarID != 66 {
			t.Fatalf("retry modified seat/profile: %+v", current)
		}
	}
	second := f.start(t, testSecret(2), strings.ToUpper(cpaUUID), returnURL)
	f.approve(t, second, 1<<54, "Changed GitHub Name", "new-login", returnURL)
	if current := identity(t, f.complete(t, testSecret(2), second)); current.ID != created.ID || current.Name != "Current" {
		t.Fatal("same numeric identity and canonical UUID did not reuse profile")
	}
	other := f.start(t, testSecret(3), cpaUUID, returnURL)
	f.approve(t, other, 1<<54+1, "Current", "alice", returnURL)
	if identity(t, f.complete(t, testSecret(3), other)).ID == created.ID {
		t.Fatal("same public profile claimed another seat")
	}
	for _, path := range []string{"/me", "/v1/clients", "/v1/clients/register", startPath + "/", completePath + "/"} {
		requireHTTPError(t, f.api(t, "POST", path, "", "", nil), 404, "not_found")
	}
	for _, path := range []string{"/auth/github/authorize/?request_id=unused", "/auth/github/callback/?code=unused"} {
		r := f.api(t, "GET", path, "", "", nil)
		requireHTTPError(t, r, 404, "not_found")
		if r.header.Get("Location") != "" || r.header.Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("noncanonical OAuth URL must not redirect or send a referrer")
		}
	}
	client := *f.server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	health, err := client.Get(f.server.URL + "/healthz/")
	if err != nil {
		t.Fatal(err)
	}
	health.Body.Close()
	if health.StatusCode != http.StatusMovedPermanently || health.Header.Get("Location") != "/healthz" {
		t.Fatalf("existing health check redirect changed: %d", health.StatusCode)
	}
	if r := f.api(t, "GET", "/readyz", "", "", nil); r.status != 503 {
		t.Fatalf("registration changed readiness: %d %s", r.status, r.body)
	}
}

func TestRegistrationHTTPInvalidInput(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secret := testSecret(1)
	for _, tc := range []struct {
		body, token string
		headers     map[string]string
		status      int
		code        string
	}{
		{validStartBody, "", nil, 401, "invalid_credential"},
		{validStartBody, "invalid", nil, 401, "invalid_credential"},
		{`{`, secret, nil, 400, "invalid_request"},
		{`null`, secret, nil, 400, "invalid_request"},
		{`[]`, secret, nil, 400, "invalid_request"},
		{validStartBody + ` {}`, secret, nil, 400, "invalid_request"},
		{strings.TrimSuffix(validStartBody, "}") + `,"return_to":"https://another.example"}`, secret, nil, 400, "invalid_request"},
		{strings.Replace(validStartBody, `"return_to"`, `"RETURN_TO"`, 1), secret, nil, 400, "invalid_request"},
		{`{"client_id":"1","cpa_uuid":"` + cpaUUID + `"}`, secret, nil, 400, "invalid_request"},
		{`{"source":"192.0.2.1","return_to":"https://app.example"}`, secret, nil, 400, "invalid_request"},
		{`{"display_name":"Valid","avatar_id":1}`, secret, nil, 400, "invalid_request"},
		{`{"cpa_uuid":null}`, secret, nil, 400, "invalid_request"},
		{"{\"return_to\":\"\xff\"}", secret, nil, 400, "invalid_request"},
		{startBody("not-a-uuid", returnURL), secret, nil, 400, "invalid_request"},
		{startBody("00000000-0000-0000-0000-000000000000", returnURL), secret, nil, 400, "invalid_request"},
		{validStartBody, secret, map[string]string{"Content-Type": "text/plain"}, 415, "unsupported_media_type"},
		{validStartBody, secret, map[string]string{"Content-Encoding": "gzip"}, 415, "unsupported_media_type"},
	} {
		requireHTTPError(t, f.send(t, tc.body, tc.token, tc.headers), tc.status, tc.code)
	}
	for _, dest := range []string{"", "//host.example/a", "https:///outside", "https://?x", "javascript:alert(1)", "https://login@host.example", "https://host\\outside", "https://host/\r\nLocation:x", "http://host:65536", "http://[not-an-ip]"} {
		body, _ := json.Marshal(map[string]string{"cpa_uuid": cpaUUID, "return_to": dest})
		requireHTTPError(t, f.send(t, string(body), secret, nil), 400, "invalid_request")
	}
	var n int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM registration_requests").Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid request allocated state: %d %v", n, err)
	}
	id := f.start(t, secret, cpaUUID, returnURL)
	requireHTTPError(t, f.api(t, "POST", completePath, `{"request_id":"`+id+`","cpa_uuid":"`+cpaUUID+`"}`, secret, nil), 400, "invalid_request")
	requireHTTPError(t, f.api(t, "POST", completePath, `{"request_id":"`+id+`","avatar_id":"42"}`, secret, nil), 400, "invalid_request")
	requireHTTPError(t, f.complete(t, secret, "bad-id"), 400, "invalid_request")
}

func TestRegistrationHTTPAvatarSelection(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())

	// 1. Omitting avatar_id creates a seat with default avatar "1".
	secretDefault := testSecret(1)
	idDefault := f.start(t, secretDefault, cpaUUID, returnURL)
	f.approve(t, idDefault, 100, "Default User", "defaultuser", returnURL)
	rDefault := f.complete(t, secretDefault, idDefault)
	seatDefault := identity(t, rDefault)
	if rDefault.status != 201 || seatDefault.AvatarID != 1 {
		t.Fatalf("default avatar seat: status=%d %+v", rDefault.status, seatDefault)
	}

	// 2. Explicit valid avatar_id "42" creates a new seat with avatar "42" and persists it.
	const otherUUID = "550e8400-e29b-41d4-a716-446655440001"
	secretCustom := testSecret(2)
	idCustom := f.startWithBody(t, secretCustom, startBodyWithAvatar(otherUUID, returnURL, "42"))
	var reqAvatar int64
	if err := f.db.Reader.QueryRow("SELECT avatar_id FROM registration_requests WHERE id=?", idCustom).Scan(&reqAvatar); err != nil || reqAvatar != 42 {
		t.Fatalf("persisted registration_requests.avatar_id: %d %v", reqAvatar, err)
	}
	f.approve(t, idCustom, 200, "Custom User", "customuser", returnURL)
	rCustom := f.complete(t, secretCustom, idCustom)
	seatCustom := identity(t, rCustom)
	if rCustom.status != 201 || seatCustom.AvatarID != 42 || seatCustom.Name != "Custom User" {
		t.Fatalf("custom avatar seat: status=%d %+v", rCustom.status, seatCustom)
	}
	var dbAvatar int64
	if err := f.db.Reader.QueryRow("SELECT avatar_id FROM clients WHERE id=?", seatCustom.ID).Scan(&dbAvatar); err != nil || dbAvatar != 42 {
		t.Fatalf("persisted clients.avatar_id: %d %v", dbAvatar, err)
	}

	// 3. Second writer for the same (github_id, cpa_uuid) with avatar_id "15" returns 200 and preserves original avatar "42" (Strategy A).
	secretSecondWriter := testSecret(3)
	idSecondWriter := f.startWithBody(t, secretSecondWriter, startBodyWithAvatar(strings.ToUpper(otherUUID), returnURL, "15"))
	f.approve(t, idSecondWriter, 200, "Renamed User", "renameduser", returnURL)
	rSecondWriter := f.complete(t, secretSecondWriter, idSecondWriter)
	seatReused := identity(t, rSecondWriter)
	if rSecondWriter.status != 200 || seatReused.ID != seatCustom.ID || seatReused.AvatarID != 42 || seatReused.Name != "Custom User" {
		t.Fatalf("second writer modified existing seat: status=%d %+v", rSecondWriter.status, seatReused)
	}
}

func TestRegistrationHTTPInvalidAvatarInputs(t *testing.T) {
	f := newRegistrationFixture(t, defaultRegistrationSettings())
	secret := testSecret(1)
	withRawAvatar := func(rawValue string) string {
		return fmt.Sprintf(`{"cpa_uuid":%q,"return_to":%q,"avatar_id":%s}`, cpaUUID, returnURL, rawValue)
	}
	for _, tc := range []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{"null", withRawAvatar(`null`), 400, "invalid_request"},
		{"number", withRawAvatar(`1`), 400, "invalid_request"},
		{"bool", withRawAvatar(`true`), 400, "invalid_request"},
		{"object", withRawAvatar(`{}`), 400, "invalid_request"},
		{"array", withRawAvatar(`["1"]`), 400, "invalid_request"},
		{"empty string", withRawAvatar(`""`), 400, "invalid_request"},
		{"zero", withRawAvatar(`"0"`), 400, "invalid_request"},
		{"negative", withRawAvatar(`"-1"`), 400, "invalid_request"},
		{"plus sign", withRawAvatar(`"+1"`), 400, "invalid_request"},
		{"leading zero", withRawAvatar(`"01"`), 400, "invalid_request"},
		{"leading space", withRawAvatar(`" 1"`), 400, "invalid_request"},
		{"trailing space", withRawAvatar(`"1 "`), 400, "invalid_request"},
		{"non-numeric", withRawAvatar(`"abc"`), 400, "invalid_request"},
		{"int64 overflow", withRawAvatar(`"9223372036854775808"`), 400, "invalid_request"},
		{"duplicate avatar_id", fmt.Sprintf(`{"cpa_uuid":%q,"return_to":%q,"avatar_id":"1","avatar_id":"2"}`, cpaUUID, returnURL), 400, "invalid_request"},
		{"unpaired surrogate in avatar_id", `{"cpa_uuid":"` + cpaUUID + `","return_to":"` + returnURL + `","avatar_id":"\ud800"}`, 400, "invalid_request"},
		{"unpaired surrogate in return_to", `{"cpa_uuid":"` + cpaUUID + `","return_to":"http://localhost:8080/\ud800"}`, 400, "invalid_request"},
		{"unlisted 67", withRawAvatar(`"67"`), 422, "invalid_avatar"},
		{"unlisted 999", withRawAvatar(`"999"`), 422, "invalid_avatar"},
		{"unlisted max int64", withRawAvatar(`"9223372036854775807"`), 422, "invalid_avatar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireHTTPError(t, f.send(t, tc.body, secret, nil), tc.status, tc.code)
		})
	}
	var n int
	if err := f.db.Reader.QueryRow("SELECT count(*) FROM registration_requests").Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid avatar requests allocated state: %d %v", n, err)
	}

	// Closed registration returns 503 for syntactically valid unlisted avatar, but 400 for malformed avatar.
	closedSettings := defaultRegistrationSettings()
	closedSettings.service.Enabled = false
	closed := newRegistrationFixture(t, closedSettings)
	requireHTTPError(t, closed.send(t, withRawAvatar(`"999"`), secret, nil), 503, "registration_closed")
	requireHTTPError(t, closed.send(t, withRawAvatar(`"0"`), secret, nil), 400, "invalid_request")
}
