package httpserver

import (
	"net/http"
	"strconv"
	"time"

	"cpa-usage-ranking/internal/admin"
	"cpa-usage-ranking/internal/clients"
	"github.com/gin-gonic/gin"
)

type adminUser struct {
	ID            string    `json:"id"`
	DisplayName   string    `json:"display_name"`
	AvatarID      string    `json:"avatar_id"`
	Disabled      bool      `json:"disabled"`
	RankingHidden bool      `json:"ranking_hidden"`
	CreatedAt     time.Time `json:"created_at"`
}

func (h *adminHTTP) ban(c *gin.Context) {
	id, err := positiveAdminID(c.Param("id"))
	if err != nil {
		h.adminError(c, err)
		return
	}
	var input struct {
		Disabled *bool `json:"disabled"`
	}
	if !decodeAdmin(c, &input) {
		return
	}
	if input.Disabled == nil {
		h.adminError(c, admin.ErrInput)
		return
	}
	if err := h.service.SetDisabled(c.Request.Context(), adminIdentity(c), id, *input.Disabled); err != nil {
		h.adminError(c, err)
		return
	}
	if h.options.RequestRefresh != nil {
		h.options.RequestRefresh()
	}
	h.logger.Info("admin action", "actor", adminIdentity(c).ID, "action", "set_ban", "target", c.Param("id"), "disabled", *input.Disabled, "result", "saved")
	c.JSON(http.StatusOK, gin.H{"disabled": *input.Disabled, "leaderboards": "refresh_requested"})
}

func (h *adminHTTP) rankingVisibility(c *gin.Context) {
	id, err := positiveAdminID(c.Param("id"))
	if err != nil {
		h.adminError(c, err)
		return
	}
	var input struct {
		Hidden *bool `json:"hidden"`
	}
	if !decodeAdmin(c, &input) {
		return
	}
	if input.Hidden == nil {
		h.adminError(c, admin.ErrInput)
		return
	}
	if err := h.service.SetRankingHidden(c.Request.Context(), adminIdentity(c), id, *input.Hidden); err != nil {
		h.adminError(c, err)
		return
	}
	if h.options.RequestRefresh != nil {
		h.options.RequestRefresh()
	}
	h.logger.Info("admin action", "actor", adminIdentity(c).ID, "action", "set_ranking_visibility", "target", c.Param("id"), "hidden", *input.Hidden, "result", "saved")
	c.JSON(http.StatusOK, gin.H{"hidden": *input.Hidden, "leaderboards": "refresh_requested"})
}

func (h *adminHTTP) users(c *gin.Context) {
	raw := c.Request.URL.Query()
	single := func(key string) (string, bool, bool) {
		values, ok := raw[key]
		if !ok {
			return "", false, true
		}
		if len(values) != 1 {
			return "", false, false
		}
		if values[0] == "" {
			return "", false, true
		}
		return values[0], true, true
	}
	query := clients.ListQuery{Limit: 50}
	var err error
	if value, present, ok := single("name"); !ok {
		h.adminError(c, admin.ErrInput)
		return
	} else if present {
		query.Name = value
	}
	if value, present, ok := single("q"); !ok {
		h.adminError(c, admin.ErrInput)
		return
	} else if present {
		query.Search = value
	}
	if value, present, ok := single("id"); !ok {
		h.adminError(c, admin.ErrInput)
		return
	} else if present {
		query.ID, err = positiveAdminID(value)
		if err != nil {
			h.adminError(c, err)
			return
		}
	}
	if value, present, ok := single("after"); !ok {
		h.adminError(c, admin.ErrInput)
		return
	} else if present {
		query.After, err = positiveAdminID(value)
		if err != nil {
			h.adminError(c, err)
			return
		}
	}
	if value, present, ok := single("limit"); !ok {
		h.adminError(c, admin.ErrInput)
		return
	} else if present {
		query.Limit, err = strconv.Atoi(value)
		if err != nil || query.Limit < 1 || query.Limit > 100 {
			h.adminError(c, admin.ErrInput)
			return
		}
	}
	if value, present, ok := single("disabled"); !ok {
		h.adminError(c, admin.ErrInput)
		return
	} else if present {
		if value != "true" && value != "false" {
			h.adminError(c, admin.ErrInput)
			return
		}
		v := value == "true"
		query.Disabled = &v
	}
	if value, present, ok := single("hidden"); !ok {
		h.adminError(c, admin.ErrInput)
		return
	} else if present {
		if value != "true" && value != "false" {
			h.adminError(c, admin.ErrInput)
			return
		}
		v := value == "true"
		query.Hidden = &v
	}
	page, err := h.service.Users(c.Request.Context(), query)
	if err != nil {
		if err == clients.ErrQuery {
			err = admin.ErrInput
		}
		h.adminError(c, err)
		return
	}
	users := make([]adminUser, 0, len(page.Clients))
	for _, client := range page.Clients {
		users = append(users, adminUser{ID: strconv.FormatInt(client.ID, 10), DisplayName: client.DisplayName, AvatarID: strconv.FormatInt(client.AvatarID, 10), Disabled: client.Disabled, RankingHidden: client.RankingHidden, CreatedAt: client.CreatedAt})
	}
	next := ""
	if page.NextID != 0 {
		next = strconv.FormatInt(page.NextID, 10)
	}
	c.JSON(http.StatusOK, gin.H{"users": users, "next_cursor": next})
}
