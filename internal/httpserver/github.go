package httpserver

import (
	"errors"
	"net/http"
	"net/url"

	"cpa-usage-ranking/internal/registration"
	"github.com/gin-gonic/gin"
)

func mountGitHubBrowser(router *gin.Engine, service *registration.Service, github *registration.GitHub) {
	mountExact(router, http.MethodGet, "/auth/github/authorize", func(c *gin.Context) {
		c.Header("Referrer-Policy", "no-referrer")
		if github == nil {
			writeRegistrationError(c, registration.ErrRegistrationClosed)
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
		cookie := githubCookie(auth.State)
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
		cookie := githubCookie(state)
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
		// 直接写 Location，避免 http.Redirect 重写用户保存的路径或编码。
		c.Header("Location", returnTo)
		c.Status(http.StatusFound)
	})
}

func githubCookie(state string) *http.Cookie {
	// 每笔授权的 Cookie 独立；Lax 允许 GitHub 顶层 GET 回跳，__Host- 防止子域覆盖。
	return &http.Cookie{Name: "__Host-ranking-github-" + state, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}
