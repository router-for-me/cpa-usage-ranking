package httpserver

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"cpa-usage-ranking/internal/auth"
	"cpa-usage-ranking/internal/clients"
	"cpa-usage-ranking/internal/reports"
	"github.com/gin-gonic/gin"
)

// MountReports binds identity before admission, decoding or writing. Neither
// the JSON body nor query parameters may select a different client.
func MountReports(router *gin.Engine, authenticator *auth.Authenticator, admission *reports.Admission, writer *reports.Writer, maxBodyBytes int64) error {
	if authenticator == nil || admission == nil || writer == nil || maxBodyBytes <= 0 {
		return fmt.Errorf("invalid report HTTP configuration")
	}
	mountExact(router, http.MethodPut, "/v1/clients/report/:utc_day", ClientAuthentication(authenticator), func(c *gin.Context) {
		client, ok := AuthenticatedClient(c)
		if !ok {
			writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			return
		}
		if err := admission.Admit(client.ID); err != nil {
			var limit *reports.AdmissionError
			if errors.As(err, &limit) {
				c.Header("Retry-After", strconv.FormatInt(limit.RetryAfterSeconds(), 10))
				writeError(c, http.StatusTooManyRequests, limit.Code, "Report admission limit reached")
			} else {
				writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
			}
			return
		}
		if ct := strings.TrimSpace(c.GetHeader("Content-Type")); ct != "" {
			mediaType, _, err := mime.ParseMediaType(ct)
			if err != nil || mediaType != "application/json" {
				writeError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Use application/json")
				return
			}
		}
		encoding, ok := reportContentEncoding(c.Request.Header)
		if !ok {
			writeError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", "Use identity or gzip Content-Encoding")
			return
		}
		if c.Request.ContentLength > maxBodyBytes {
			writeError(c, http.StatusRequestEntityTooLarge, "request_too_large", "Report body is too large")
			return
		}
		var input reports.Report
		var err error
		if encoding == "gzip" {
			input, err = admission.DecodeGzip(c.Request.Body)
		} else {
			input, err = admission.Decode(c.Request.Body)
		}
		switch {
		case errors.Is(err, reports.ErrDecodeBusy):
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "report_overloaded", "Report service is busy")
			return
		case errors.Is(err, reports.ErrBodyTooLarge):
			writeError(c, http.StatusRequestEntityTooLarge, "request_too_large", "Report body is too large")
			return
		case errors.Is(err, reports.ErrTooManyItems):
			writeError(c, http.StatusUnprocessableEntity, "too_many_items", "Too many report items")
			return
		case err != nil:
			writeError(c, http.StatusUnprocessableEntity, "invalid_report", "Invalid report")
			return
		}
		prepared, err := reports.Prepare(input)
		if err != nil {
			writeError(c, http.StatusUnprocessableEntity, "invalid_report", "Invalid report")
			return
		}
		result, err := writer.Enqueue(c.Request.Context(), client.WriterID, c.Param("utc_day"), prepared)
		if err != nil {
			if errors.Is(err, reports.ErrQueueFull) || errors.Is(err, reports.ErrWriterClosed) {
				c.Header("Retry-After", "1")
				writeError(c, http.StatusServiceUnavailable, "report_overloaded", "Report service is busy")
			} else {
				writeReportResult(c, reports.WriteResult{Err: err})
			}
			return
		}
		select {
		case <-c.Request.Context().Done():
			// A lost response around commit is ambiguous; never fall through to
			// an empty HTTP 200 if the transport can still accept a response.
			writeReportResult(c, reports.WriteResult{Err: c.Request.Context().Err()})
			return
		case outcome := <-result:
			writeReportResult(c, outcome)
		}
	})
	return nil
}

func reportContentEncoding(header http.Header) (string, bool) {
	values := header.Values("Content-Encoding")
	if len(values) == 0 {
		return "", true
	}
	if len(values) > 1 {
		return "", false
	}
	encoding := strings.ToLower(strings.TrimSpace(values[0]))
	switch encoding {
	case "", "identity", "gzip":
		return encoding, true
	default:
		return "", false
	}
}

func writeReportResult(c *gin.Context, result reports.WriteResult) {
	var conflict *reports.RevisionConflict
	switch {
	case result.Err == nil:
		c.JSON(http.StatusOK, gin.H{"revision": strconv.FormatInt(result.Revision, 10)})
	case errors.As(result.Err, &conflict):
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{"error": gin.H{"code": "revision_conflict", "message": "Report revision conflicts with the writer's current revision"}, "revision": strconv.FormatInt(conflict.Current, 10)})
	case errors.Is(result.Err, reports.ErrFutureDay):
		writeError(c, http.StatusUnprocessableEntity, "future_day", "Future UTC day is not accepted")
	case errors.Is(result.Err, reports.ErrInvalidDay):
		writeError(c, http.StatusUnprocessableEntity, "invalid_day", "Invalid UTC day")
	case errors.Is(result.Err, reports.ErrExpiredDay):
		writeError(c, http.StatusGone, "expired_day", "Report window has closed")
	case errors.Is(result.Err, reports.ErrInvalid):
		writeError(c, http.StatusUnprocessableEntity, "invalid_report", "Invalid report")
	case errors.Is(result.Err, clients.ErrWithdrawn):
		writeError(c, http.StatusForbidden, "client_withdrawn", "Client has withdrawn")
	case errors.Is(result.Err, clients.ErrDisabled):
		writeError(c, http.StatusForbidden, "client_disabled", "Client is disabled")
	case errors.Is(result.Err, clients.ErrNotFound):
		writeInvalidCredential(c)
	case errors.Is(result.Err, context.DeadlineExceeded), errors.Is(result.Err, context.Canceled):
		c.Header("Retry-After", "1")
		writeError(c, http.StatusServiceUnavailable, "report_overloaded", "Report service is busy")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "Internal server error")
	}
}
