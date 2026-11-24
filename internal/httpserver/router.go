// Package httpserver owns HTTP routing and serving boundaries.
package httpserver

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// NewRouter exposes liveness without claiming unfinished business readiness.
func NewRouter(logger *slog.Logger) *gin.Engine {
	return NewRouterWithReadiness(logger, nil)
}

// NewRouterWithReadiness 接入内部就绪检查；不在请求路径中构建缓存。
func NewRouterWithReadiness(logger *slog.Logger, ready func(context.Context) bool) *gin.Engine {
	router := gin.New()
	router.HandleMethodNotAllowed = true
	router.RedirectTrailingSlash = false
	router.RedirectFixedPath = false
	if err := router.SetTrustedProxies(nil); err != nil {
		panic(err)
	}
	router.Use(func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	}, func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				// 不打印 panic 值、请求头或正文，避免异常路径泄漏接入凭据。
				logger.Error("http handler panicked")
				if c.Writer.Written() {
					// 已开始的响应无法安全改写为 JSON 错误，让 net/http 中断连接。
					panic(http.ErrAbortHandler)
				}
				writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			}
		}()
		c.Next()
	})
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	router.GET("/readyz", func(c *gin.Context) {
		if ready != nil && ready(c.Request.Context()) {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "reason": "initializing_or_unavailable"})
	})
	router.NoRoute(func(c *gin.Context) {
		c.Header("Referrer-Policy", "no-referrer")
		writeError(c, http.StatusNotFound, "not_found", "Not found")
	})
	router.NoMethod(func(c *gin.Context) {
		writeError(c, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
	})
	return router
}

// mountExact mounts a route directly without trailing-slash redirects.
func mountExact(router *gin.Engine, method, path string, handlers ...gin.HandlerFunc) {
	router.Handle(method, path, handlers...)
}

func writeError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": message}})
}
