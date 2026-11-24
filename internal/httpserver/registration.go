package httpserver

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
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

// optionalAvatarID 接受可选的正 int64 JSON 整数或十进制字符串；省略或 null 视为未传。
type optionalAvatarID struct {
	id  int64
	set bool
}

func (a *optionalAvatarID) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		a.id = 0
		a.set = false
		return nil
	}
	var text string
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return registration.ErrInvalidRequest
		}
	} else {
		text = string(trimmed)
	}
	parsed, ok := parsePositiveInt64String(text)
	if !ok {
		return registration.ErrInvalidRequest
	}
	a.id = parsed
	a.set = true
	return nil
}

type registrationStartInput struct {
	ServerID *string          `json:"server_id"`
	ReturnTo *string          `json:"return_to"`
	AvatarID optionalAvatarID `json:"avatar_id"`
}

type registrationCompleteInput struct {
	RequestID *string `json:"request_id"`
}

func MountRegistration(router *gin.Engine, service *registration.Service, github *registration.GitHub, limiter *registration.RequestLimiter, options RegistrationHTTPOptions) error {
	if service == nil || limiter == nil || options.MaxBodyBytes <= 0 {
		return fmt.Errorf("invalid registration HTTP configuration")
	}
	mountExact(router, http.MethodPost, "/v1/clients/register/start", RegistrationAdmission(limiter, options.GroupIPv6By64), func(c *gin.Context) {
		var input registrationStartInput
		hash, ok := decodeRegistrationJSON(c, options.MaxBodyBytes, &input)
		if !ok {
			return
		}
		if input.ServerID == nil || *input.ServerID == "" || input.ReturnTo == nil || *input.ReturnTo == "" {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return
		}
		var avatarID *int64
		if input.AvatarID.set {
			v := input.AvatarID.id
			avatarID = &v
		}
		if github == nil {
			writeRegistrationError(c, registration.ErrRegistrationClosed)
			return
		}
		id, err := service.Start(c.Request.Context(), hash, *input.ServerID, *input.ReturnTo, avatarID)
		if err != nil {
			writeRegistrationError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"request_id": id, "authorize_url": github.BrowserURL(id)})
	})
	mountExact(router, http.MethodPost, "/v1/clients/register/complete", func(c *gin.Context) {
		var input registrationCompleteInput
		hash, ok := decodeRegistrationJSON(c, options.MaxBodyBytes, &input)
		if !ok {
			return
		}
		if input.RequestID == nil || *input.RequestID == "" {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return
		}
		result, err := service.Complete(c.Request.Context(), hash, *input.RequestID)
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

func decodeRegistrationJSON(c *gin.Context, maxBytes int64, dst any) (clients.CredentialHash, bool) {
	hash, err := auth.BearerHash(c.Request.Header)
	if err != nil {
		writeInvalidCredential(c)
		return hash, false
	}
	if ct := strings.TrimSpace(c.GetHeader("Content-Type")); ct != "" {
		mediaType, _, err := mime.ParseMediaType(ct)
		if err != nil || mediaType != "application/json" {
			writeError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Use application/json")
			return hash, false
		}
	}
	encoding, ok := reportContentEncoding(c.Request.Header)
	if !ok {
		writeError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Use identity or gzip Content-Encoding")
		return hash, false
	}
	if c.Request.ContentLength > maxBytes {
		writeError(c, http.StatusRequestEntityTooLarge, "request_too_large", "Registration body is too large")
		return hash, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(c, http.StatusRequestEntityTooLarge, "request_too_large", "Registration body is too large")
		return hash, false
	}
	if err != nil {
		writeRegistrationError(c, registration.ErrInvalidRequest)
		return hash, false
	}
	if encoding == "gzip" {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return hash, false
		}
		defer gz.Close()
		decompressed, err := io.ReadAll(io.LimitReader(gz, maxBytes+1))
		if err != nil {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return hash, false
		}
		if int64(len(decompressed)) > maxBytes {
			writeError(c, http.StatusRequestEntityTooLarge, "request_too_large", "Registration body is too large")
			return hash, false
		}
		if err := gz.Close(); err != nil {
			writeRegistrationError(c, registration.ErrInvalidRequest)
			return hash, false
		}
		body = decompressed
	}
	if !utf8.Valid(body) {
		writeRegistrationError(c, registration.ErrInvalidRequest)
		return hash, false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeRegistrationError(c, registration.ErrInvalidRequest)
		return hash, false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decoder.Decode(dst); err != nil {
		writeRegistrationError(c, registration.ErrInvalidRequest)
		return hash, false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		writeRegistrationError(c, registration.ErrInvalidRequest)
		return hash, false
	}
	return hash, true
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
