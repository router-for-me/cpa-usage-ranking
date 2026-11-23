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
		var serverID, returnTo string
		var rawAvatarID *string
		hash, ok := registrationInput(c, options.MaxBodyBytes, map[string]*string{"server_id": &serverID, "return_to": &returnTo}, map[string]**string{"avatar_id": &rawAvatarID})
		if !ok {
			return
		}
		var avatarID *int64
		if rawAvatarID != nil {
			parsed, ok := parsePositiveInt64String(*rawAvatarID)
			if !ok {
				writeRegistrationError(c, registration.ErrInvalidRequest)
				return
			}
			avatarID = &parsed
		}
		if github == nil {
			writeRegistrationError(c, registration.ErrRegistrationClosed)
			return
		}
		id, err := service.Start(c.Request.Context(), hash, serverID, returnTo, avatarID)
		if err != nil {
			writeRegistrationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"request_id": id, "authorize_url": github.BrowserURL(id)})
	})
	mountExact(router, http.MethodPost, "/v1/clients/register/complete", func(c *gin.Context) {
		var id string
		hash, ok := registrationInput(c, options.MaxBodyBytes, map[string]*string{"request_id": &id}, nil)
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

func registrationInput(c *gin.Context, maxBytes int64, required map[string]*string, optional map[string]**string) (clients.CredentialHash, bool) {
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
	if err != nil || !decodeRegistrationFields(body, required, optional) {
		writeRegistrationError(c, registration.ErrInvalidRequest)
		return hash, false
	}
	return hash, true
}

func decodeRegistrationFields(body []byte, required map[string]*string, optional map[string]**string) bool {
	if !utf8.Valid(body) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if tok, err := decoder.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	for decoder.More() {
		tok, err := decoder.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return false
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return false
		}
		trimmed := bytes.TrimSpace(raw)
		var value *string
		if json.Unmarshal(trimmed, &value) != nil || value == nil || !validUnicodeEscapes(trimmed) {
			return false
		}
		if field, ok := required[key]; ok {
			*field = *value
			delete(required, key)
			continue
		}
		if field, ok := optional[key]; ok {
			v := *value
			*field = &v
			delete(optional, key)
			continue
		}
		return false
	}
	if tok, err := decoder.Token(); err != nil || tok != json.Delim('}') || len(required) != 0 {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func parsePositiveInt64String(s string) (int64, bool) {
	if len(s) == 0 || s[0] < '1' || s[0] > '9' {
		return 0, false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil && v > 0
}

// encoding/json replaces unpaired surrogate escapes; reject them instead.
func validUnicodeEscapes(raw []byte) bool {
	for i := 1; i+1 < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if raw[i] != 'u' {
			continue
		}
		code, _ := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func writeRegistrationError(c *gin.Context, err error) {
	var limit *registration.LimitError
	switch {
	case errors.Is(err, registration.ErrInvalidRequest):
		writeError(c, http.StatusBadRequest, "invalid_request", "Invalid registration request")
	case errors.Is(err, registration.ErrInvalidAvatar):
		writeError(c, http.StatusUnprocessableEntity, "invalid_avatar", "Invalid avatar selection")
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
