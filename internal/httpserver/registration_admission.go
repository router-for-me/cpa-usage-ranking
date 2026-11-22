package httpserver

import (
	"errors"
	"net/http"
	"strconv"

	"cpa-usage-ranking/internal/network"
	"cpa-usage-ranking/internal/registration"
	"github.com/gin-gonic/gin"
)

// RegistrationAdmission applies only to start, before parsing credentials or JSON.
func RegistrationAdmission(limiter *registration.RequestLimiter, groupIPv6By64 bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		source, err := network.RegistrationSource(c, groupIPv6By64)
		if err != nil {
			writeError(c, http.StatusBadRequest, "invalid_source", "Invalid registration source")
			return
		}
		if err := limiter.Admit(source); err != nil {
			writeRegistrationLimit(c, err)
			return
		}
		c.Next()
	}
}

func writeRegistrationLimit(c *gin.Context, err error) {
	var limit *registration.LimitError
	if !errors.As(err, &limit) {
		writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
		return
	}
	c.Header("Retry-After", strconv.FormatInt(limit.RetryAfterSeconds(), 10))
	writeError(c, http.StatusTooManyRequests, limit.Code, "Registration temporarily limited")
}
