package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/leaderboard"
	"github.com/gin-gonic/gin"
)

type leaderboardCache interface {
	Current() (cache.Snapshot, error)
}

// MountLeaderboards 只读取已发布的缓存轮次，不触发数据库查询或计算。
func MountLeaderboards(router *gin.Engine, source leaderboardCache) {
	serve := func(c *gin.Context) {
		setPublicReadCORS(c)
		board, period := c.Param("board"), c.Param("period")
		switch board {
		case leaderboard.Overall, leaderboard.Usage, leaderboard.Cache, leaderboard.Success, leaderboard.TTFT:
		default:
			writeError(c, http.StatusBadRequest, "invalid_parameter", "Invalid leaderboard or period")
			return
		}
		switch period {
		case "today", "yesterday", "week", "month", "year", "all":
		default:
			writeError(c, http.StatusBadRequest, "invalid_parameter", "Invalid leaderboard or period")
			return
		}
		snapshot, err := source.Current()
		if errors.Is(err, cache.ErrUnavailable) {
			writeError(c, http.StatusServiceUnavailable, "leaderboard_unavailable", "Leaderboard cache is not ready")
			return
		}
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			return
		}
		entry, ok := snapshot.Get(period)
		if !ok {
			writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			return
		}
		var boards map[string]json.RawMessage
		if err := json.Unmarshal([]byte(entry.Content), &boards); err != nil || len(boards[board]) == 0 {
			writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			return
		}
		var end *string
		if entry.End != "" {
			end = &entry.End
		}
		writeJSON(c, http.StatusOK, struct {
			Board       string          `json:"board"`
			Period      string          `json:"period"`
			Start       string          `json:"start"`
			End         *string         `json:"end"`
			GeneratedAt string          `json:"generated_at"`
			Entries     json.RawMessage `json:"entries"`
		}{board, period, entry.Start, end, entry.GeneratedAt, boards[board]})
	}
	options := func(c *gin.Context) {
		setPublicReadCORS(c)
		c.Status(http.StatusNoContent)
	}
	router.GET("/v1/leaderboards/:board/:period", serve)
	router.HEAD("/v1/leaderboards/:board/:period", serve)
	router.OPTIONS("/v1/leaderboards/:board/:period", options)
}
