package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"unicode/utf8"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/registration"
	"github.com/gin-gonic/gin"
)

type RegistrationHTTPOptions struct {
	GroupIPv6By64 bool
	MaxBodyBytes  int64
}

func MountRegistration(router *gin.Engine, service *registration.Service, github *registration.GitHub, limiter *registration.RequestLimiter, options RegistrationHTTPOptions) error {
	if service == nil || limiter == nil || options.MaxBodyBytes <= 0 {
		return fmt.Errorf("invalid registration HTTP configuration")
	}
	mountExact(router, http.MethodPost, "/v1/clients/register/start", RegistrationAdmission(limiter, options.GroupIPv6By64), func(c *gin.Context) {
		var cpaUUID, returnTo string
		hash, ok := registrationInput(c, options.MaxBodyBytes, map[string]*string{"cpa_uuid": &cpaUUID, "return_to": &returnTo})
		if !ok {
			return
		}
		if github == nil {
			writeRegistrationError(c, registration.ErrRegistrationClosed)
			return
		}
		id, err := service.Start(c.Request.Context(), hash, cpaUUID, returnTo)
		if err != nil {
			writeRegistrationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"request_id": id, "authorize_url": github.BrowserURL(id)})
	})
	mountExact(router, http.MethodPost, "/v1/clients/register/complete", func(c *gin.Context) {
		var id string
		hash, ok := registrationInput(c, options.MaxBodyBytes, map[string]*string{"request_id": &id})
		if !ok {
			return
		}
		result, err := service.Complete(c.Request.Context(), hash, id)
		switch {
		case errors.Is(err, registration.ErrPending):
			c.Header("Retry-After", strconv.Itoa(int(registration.PendingRetryAfter.Seconds())))
			c.Status(http.StatusAccepted)
		case err != nil:
			writeRegistrationError(c, err)
		default:
			status := http.StatusOK
			if result.Created {
				status = http.StatusCreated
			}
			c.JSON(status, gin.H{"client_id": strconv.FormatInt(result.Client.ID, 10), "display_name": result.Client.DisplayName, "avatar_id": strconv.FormatInt(result.Client.AvatarID, 10)})
		}
	})
	mountGitHubBrowser(router, service, github)
	return nil
}

func registrationInput(c *gin.Context, maxBytes int64, fields map[string]*string) (clients.CredentialHash, bool) {
	hash, err := auth.BearerHash(c.Request.Header)
	if err != nil {
		writeInvalidCredential(c)
		return hash, false
	}
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" || (c.GetHeader("Content-Encoding") != "" && c.GetHeader("Content-Encoding") != "identity") {
		writeError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Use uncompressed application/json")
		return hash, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(c, http.StatusRequestEntityTooLarge, "request_too_large", "Registration body is too large")
		return hash, false
	}
	if err != nil || !decodeRegistrationFields(body, fields) {
		writeRegistrationError(c, registration.ErrInvalidRequest)
		return hash, false
	}
	return hash, true
}

func decodeRegistrationFields(body []byte, fields map[string]*string) bool {
	if !utf8.Valid(body) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if tok, err := decoder.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for decoder.More() {
		tok, err := decoder.Token()
		key, _ := tok.(string)
		field := fields[key]
		var value *string
		if err != nil || field == nil || decoder.Decode(&value) != nil || value == nil {
			return false
		}
		*field = *value
		delete(fields, key)
	}
	if tok, err := decoder.Token(); err != nil || tok != json.Delim('}') || len(fields) != 0 {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func writeRegistrationError(c *gin.Context, err error) {
	var limit *registration.LimitError
	switch {
	case errors.Is(err, registration.ErrInvalidRequest):
		writeError(c, http.StatusBadRequest, "invalid_request", "Invalid registration request")
	case errors.Is(err, registration.ErrWrongCredential):
		writeInvalidCredential(c)
	case errors.Is(err, clients.ErrWithdrawn):
		writeError(c, http.StatusForbidden, "client_withdrawn", "Client has withdrawn")
	case errors.Is(err, clients.ErrDisabled):
		writeError(c, http.StatusForbidden, "client_disabled", "Client is disabled")
	case errors.Is(err, registration.ErrDenied):
		writeError(c, http.StatusForbidden, "github_authorization_denied", "GitHub authorization was denied")
	case errors.Is(err, registration.ErrExpired):
		writeError(c, http.StatusGone, "registration_request_expired", "Registration request has expired")
	case errors.Is(err, registration.ErrRegistrationClosed):
		writeError(c, http.StatusServiceUnavailable, "registration_closed", "New registration is closed")
	case errors.As(err, &limit):
		writeRegistrationLimit(c, err)
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}
