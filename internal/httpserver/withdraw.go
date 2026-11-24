package httpserver

import (
	"errors"
	"io"
	"net/http"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/clients"
	"github.com/gin-gonic/gin"
)

func MountWithdraw(router *gin.Engine, repo *clients.Repository, requestRefresh func()) {
	mountExact(router, http.MethodPost, "/v1/clients/withdraw", func(c *gin.Context) {
		hash, err := auth.BearerHash(c.Request.Header)
		if err != nil {
			writeInvalidCredential(c)
			return
		}
		// 忽略可选查询串与小体积正文（如客户端附带的 `{}`），仅做有界排空以利连接复用。
		if c.Request.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(c.Request.Body, 4096))
		}
		changed, err := repo.Withdraw(c.Request.Context(), hash)
		switch {
		case errors.Is(err, clients.ErrNotFound):
			writeInvalidCredential(c)
		case err != nil:
			writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
		default:
			// 确认退出只代表落库。重复退出不打断正在进行的缓存刷新。
			if changed && requestRefresh != nil {
				requestRefresh()
			}
			c.Status(http.StatusNoContent)
		}
	})
}
