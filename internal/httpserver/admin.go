package httpserver

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"cpa-usage-ranking/internal/admin"
	"cpa-usage-ranking/internal/network"
	"cpa-usage-ranking/internal/registration"
	"github.com/gin-gonic/gin"
)

const adminCookie = "ranking_admin"

type AdminHTTPOptions struct {
	AllowLocalHTTP bool
	TrustedProxies []string
	GroupIPv6By64  bool
	RequestRefresh func()
}

type adminHTTP struct {
	service *admin.Service
	options AdminHTTPOptions
	limiter *registration.RequestLimiter
	logger  *slog.Logger
}

// MountAdmin always exposes only safe enable/session state when disabled.
func MountAdmin(router *gin.Engine, service *admin.Service, frontend *Frontend, logger *slog.Logger, options AdminHTTPOptions) error {
	limiter, err := registration.NewRequestLimiter(registration.RequestLimits{SourceRequests: 5, GlobalRequests: 40, Window: time.Minute, MaxSources: 4096}, nil)
	if err != nil {
		return err
	}
	h := &adminHTTP{service: service, options: options, limiter: limiter, logger: logger}
	router.GET("/v1/admin/session", h.status)
	if service == nil {
		return nil
	}
	router.POST("/v1/admin/session", h.sameOrigin, h.login)
	router.DELETE("/v1/admin/session", h.sameOrigin, h.authorize, h.csrf, h.logout)
	router.GET("/v1/admin/users", h.authorize, h.users)
	router.PUT("/v1/admin/users/:id/ban", h.sameOrigin, h.authorize, h.csrf, h.ban)
	router.PUT("/v1/admin/users/:id/ranking-visibility", h.sameOrigin, h.authorize, h.csrf, h.rankingVisibility)
	router.GET("/v1/admin/accounts", h.authorize, h.ownerOnly, h.accounts)
	router.POST("/v1/admin/accounts", h.sameOrigin, h.authorize, h.ownerOnly, h.csrf, h.createAccount)
	router.DELETE("/v1/admin/accounts/:id", h.sameOrigin, h.authorize, h.ownerOnly, h.csrf, h.deleteAccount)
	if frontend != nil {
		for _, path := range []string{"/admin/login", "/admin/leaderboards", "/admin/accounts", "/admin/users"} {
			router.GET(path, func(c *gin.Context) { frontend.serveIndex(c) })
			router.HEAD(path, func(c *gin.Context) { frontend.serveIndex(c) })
		}
	}
	return nil
}

func (h *adminHTTP) status(c *gin.Context) {
	if h.service == nil {
		c.JSON(http.StatusOK, gin.H{"enabled": false, "authenticated": false})
		return
	}
	token, _ := c.Cookie(adminCookie)
	session, err := h.service.Lookup(c.Request.Context(), token)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"enabled": true, "authenticated": false})
		return
	}
	c.JSON(http.StatusOK, gin.H{"enabled": true, "authenticated": true, "user": session.Identity, "csrf_token": session.CSRF, "expires_at": session.ExpiresAt})
}

func (h *adminHTTP) login(c *gin.Context) {
	source, err := network.RegistrationSource(c, h.options.GroupIPv6By64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "invalid_request", "Invalid request")
		return
	}
	if err := h.limiter.Admit(source); err != nil {
		var limited *registration.LimitError
		if errors.As(err, &limited) {
			c.Header("Retry-After", strconv.FormatInt(limited.RetryAfterSeconds(), 10))
		}
		writeError(c, http.StatusTooManyRequests, "login_limited", "Too many login attempts; retry later")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeAdmin(c, &input) {
		return
	}
	token, session, err := h.service.Login(c.Request.Context(), input.Username, input.Password)
	if err != nil {
		if errors.Is(err, admin.ErrCredentials) {
			writeError(c, http.StatusUnauthorized, "invalid_credentials", "Invalid username or password")
			return
		}
		writeError(c, http.StatusServiceUnavailable, "login_unavailable", "Login temporarily unavailable")
		return
	}
	if old, _ := c.Cookie(adminCookie); old != "" {
		h.service.Logout(old)
	}
	h.cookie(c, token, int(admin.SessionTTL/time.Second))
	h.logger.Info("admin action", "actor", session.Identity.ID, "action", "login", "result", "ok")
	c.JSON(http.StatusOK, gin.H{"enabled": true, "authenticated": true, "user": session.Identity, "csrf_token": session.CSRF, "expires_at": session.ExpiresAt})
}

func (h *adminHTTP) cookie(c *gin.Context, token string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{Name: adminCookie, Value: token, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: !h.options.AllowLocalHTTP, SameSite: http.SameSiteStrictMode})
}

func (h *adminHTTP) authorize(c *gin.Context) {
	token, _ := c.Cookie(adminCookie)
	session, err := h.service.Lookup(c.Request.Context(), token)
	if err != nil {
		writeError(c, http.StatusUnauthorized, "session_required", "Administrator login required")
		return
	}
	c.Set("admin_session", session)
	c.Next()
}

func (h *adminHTTP) csrf(c *gin.Context) {
	session := c.MustGet("admin_session").(admin.Session)
	if !admin.ValidCSRF(session, c.GetHeader("X-CSRF-Token")) {
		writeError(c, http.StatusForbidden, "csrf_required", "Invalid CSRF token")
		return
	}
	c.Next()
}

func (h *adminHTTP) logout(c *gin.Context) {
	token, _ := c.Cookie(adminCookie)
	h.service.Logout(token)
	h.cookie(c, "", -1)
	c.Status(http.StatusNoContent)
}

// Only explicitly trusted proxy peers may assert HTTPS. When AllowLocalHTTP is
// enabled for local or container HTTP testing, non-HTTPS requests use scheme
// "http" while keeping strict same-origin, non-cross-site, and CSRF checks.
func (h *adminHTTP) sameOrigin(c *gin.Context) {
	scheme := "https"
	secure := c.Request.TLS != nil
	peer, _, _ := net.SplitHostPort(c.Request.RemoteAddr)
	ip := net.ParseIP(peer)
	for _, entry := range h.options.TrustedProxies {
		_, block, _ := net.ParseCIDR(entry)
		if (ip != nil && ip.Equal(net.ParseIP(entry))) || (block != nil && block.Contains(ip)) {
			secure = secure || c.GetHeader("X-Forwarded-Proto") == "https"
		}
	}
	if !secure {
		if h.options.AllowLocalHTTP {
			scheme = "http"
		} else {
			writeError(c, http.StatusForbidden, "https_required", "HTTPS required")
			return
		}
	}
	if c.GetHeader("Origin") != scheme+"://"+c.Request.Host || c.GetHeader("Sec-Fetch-Site") == "cross-site" {
		writeError(c, http.StatusForbidden, "same_origin_required", "Same-origin request required")
		return
	}
	c.Next()
}

func decodeAdmin(c *gin.Context, input any) bool {
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" {
		writeError(c, http.StatusUnsupportedMediaType, "json_required", "JSON required")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(input) != nil || decoder.Decode(new(any)) != io.EOF {
		writeError(c, http.StatusBadRequest, "invalid_request", "Invalid request")
		return false
	}
	return true
}
