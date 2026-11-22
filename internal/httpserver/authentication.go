package httpserver

import (
	"errors"
	"net/http"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/clients"
	"github.com/gin-gonic/gin"
)

const authenticatedClientKey = "cpa-usage-ranking.authenticated-client"

// ClientAuthentication is an internal building block, not a login/session API.
func ClientAuthentication(authenticator *auth.Authenticator) gin.HandlerFunc {
	return func(c *gin.Context) {
		client, err := authenticator.Authenticate(c.Request.Context(), c.Request.Header)
		switch {
		case errors.Is(err, auth.ErrInvalidCredential):
			writeInvalidCredential(c)
		case errors.Is(err, clients.ErrWithdrawn):
			writeError(c, http.StatusForbidden, "client_withdrawn", "Client has withdrawn")
		case errors.Is(err, clients.ErrDisabled):
			writeError(c, http.StatusForbidden, "client_disabled", "Client is disabled")
		case err != nil:
			writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
		default:
			c.Set(authenticatedClientKey, client)
			c.Next()
		}
	}
}

// AuthenticatedClient reads only middleware-established writer and seat, never input IDs.
func AuthenticatedClient(c *gin.Context) (clients.Writer, bool) {
	value, _ := c.Get(authenticatedClientKey)
	writer, ok := value.(clients.Writer)
	return writer, ok
}

func writeInvalidCredential(c *gin.Context) {
	c.Header("WWW-Authenticate", "Bearer")
	writeError(c, http.StatusUnauthorized, "invalid_credential", "Invalid client credential")
}
