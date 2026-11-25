package httpserver

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"cpa-usage-ranking/internal/registration"
	"github.com/gin-gonic/gin"
)

const (
	githubCompletionCSP = "default-src 'none'; style-src 'unsafe-inline'"

	githubApprovedHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorization Complete / 授权完成</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px;font-family:system-ui,-apple-system,sans-serif;background:#f7f8fa;color:#111827}
main{max-width:440px;width:100%;background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:24px;box-sizing:border-box}
h1{margin:0 0 12px;font-size:20px;line-height:1.3}
p{margin:8px 0;font-size:14px;line-height:1.6;color:#4b5563}
</style>
</head>
<body>
<main>
<h1>Authorization Complete / 授权已完成</h1>
<p>GitHub authorization succeeded. You can close this tab and return to your client.</p>
<p>GitHub 授权已成功完成，现在可以关闭此标签页并返回客户端。</p>
</main>
</body>
</html>`

	githubDeniedHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorization Cancelled / 授权已取消</title>
<style>
body{margin:0;min-height:100vh;display:grid;place-items:center;padding:24px;font-family:system-ui,-apple-system,sans-serif;background:#f7f8fa;color:#111827}
main{max-width:440px;width:100%;background:#fff;border:1px solid #e5e7eb;border-radius:12px;padding:24px;box-sizing:border-box}
h1{margin:0 0 12px;font-size:20px;line-height:1.3}
p{margin:8px 0;font-size:14px;line-height:1.6;color:#4b5563}
</style>
</head>
<body>
<main>
<h1>Authorization Cancelled / 授权已取消</h1>
<p>GitHub authorization was cancelled. You can close this tab and return to your client.</p>
<p>GitHub 授权已取消，现在可以关闭此标签页并返回客户端。</p>
</main>
</body>
</html>`
)

func isPrefetchRequest(r *http.Request) bool {
	for _, header := range []string{"Purpose", "Sec-Purpose"} {
		for _, value := range r.Header.Values(header) {
			if strings.Contains(strings.ToLower(value), "prefetch") {
				return true
			}
		}
	}
	return false
}

func mountGitHubBrowser(router *gin.Engine, service *registration.Service, github *registration.GitHub) {
	mountExact(router, http.MethodGet, "/auth/github/authorize", func(c *gin.Context) {
		c.Header("Referrer-Policy", "no-referrer")
		if github == nil {
			writeRegistrationError(c, registration.ErrRegistrationClosed)
			return
		}
		if isPrefetchRequest(c.Request) {
			// 浏览器/聊天软件预取不消耗或轮换授权会话，直接返回 204。
			c.Header("Cache-Control", "no-store")
			c.Status(http.StatusNoContent)
			return
		}
		query, err := url.ParseQuery(c.Request.URL.RawQuery)
		if err != nil || len(query["request_id"]) != 1 || query.Get("request_id") == "" {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return
		}
		auth, err := service.Authorize(c.Request.Context(), query.Get("request_id"))
		if err != nil {
			writeRegistrationError(c, err)
			return
		}
		cookie := githubCookie(auth.State, github.SecureCookie())
		cookie.Value, cookie.Expires = auth.BrowserSession, auth.Expires
		http.SetCookie(c.Writer, cookie)
		c.Header("Location", github.AuthorizeURL(auth))
		c.Status(http.StatusFound)
	})
	mountExact(router, http.MethodGet, "/auth/github/callback", func(c *gin.Context) {
		c.Header("Referrer-Policy", "no-referrer")
		if github == nil {
			writeRegistrationError(c, registration.ErrRegistrationClosed)
			return
		}
		query, err := url.ParseQuery(c.Request.URL.RawQuery)
		code := query.Get("code")
		oauthErr := query.Get("error")
		validCode := len(query["code"]) == 1 && len(query["error"]) == 0 && code != "" && len(code) <= 2048
		validDenial := len(query["error"]) == 1 && len(query["code"]) == 0 && oauthErr != ""
		if err != nil || len(query["state"]) != 1 || !(validCode || validDenial) {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return
		}
		state := query.Get("state")
		cookie := githubCookie(state, github.SecureCookie())
		sessions := c.Request.CookiesNamed(cookie.Name)
		if len(sessions) != 1 {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return
		}
		returnTo, err := service.Callback(c.Request.Context(), github, state, sessions[0].Value, code)
		if err == nil || errors.Is(err, registration.ErrGitHub) {
			cookie.MaxAge = -1
			http.SetCookie(c.Writer, cookie)
		}
		if err != nil {
			writeRegistrationError(c, err)
			return
		}
		if returnTo == "" {
			c.Header("Content-Security-Policy", githubCompletionCSP)
			body := githubApprovedHTML
			if code == "" {
				body = githubDeniedHTML
			}
			c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(body))
			return
		}
		// 直接写 Location，避免 http.Redirect 重写用户保存的路径或编码。
		c.Header("Location", returnTo)
		c.Status(http.StatusFound)
	})
}

func githubCookie(state string, secure bool) *http.Cookie {
	// 每笔授权的 Cookie 独立；生产 HTTPS 使用 __Host- 防子域覆盖，本地回环 HTTP 联调降级为普通 Cookie。
	name := "ranking-github-" + state
	if secure {
		name = "__Host-" + name
	}
	return &http.Cookie{Name: name, Path: "/", Secure: secure, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}
