package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type sustainedTraceCounts struct {
	written, peak, late atomic.Int64
}

type sustainedHTTPResult struct {
	record    sustainedRecord
	retryable bool
}

func sustainedRequest(ctx context.Context, client *http.Client, base string, f Fixture, task SustainedTask, attempt int, timeout time.Duration, counters *sustainedTraceCounts) sustainedHTTPResult {
	result := sustainedHTTPResult{record: sustainedRecord{Kind: "finish", Sequence: task.Sequence, Client: task.Client, Action: "report", Attempt: attempt}}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPut, strings.TrimSuffix(base, "/")+"/v1/clients/report/"+task.Day, bytes.NewReader(task.Body))
	if err != nil {
		result.record.Error = "invalid_request"
		result.record.TimeUnixNS = time.Now().UnixNano()
		return result
	}
	// Go's transport otherwise transparently retries a replayable PUT on some
	// reused-connection failures, hiding a wire attempt from the journal. The
	// driver itself performs and records every idempotent retry instead.
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.Secret(task.Client))
	var mu sync.Mutex
	var got, wrote int64
	finished := false
	tr := &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			if finished {
				counters.late.Add(1)
				return
			}
			if got == 0 {
				got = time.Now().UnixNano()
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			if finished {
				counters.late.Add(1)
				return
			}
			if info.Err == nil && wrote == 0 {
				wrote = time.Now().UnixNano()
				active := counters.written.Add(1)
				updateMax(&counters.peak, active)
			}
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), tr))
	resp, callErr := client.Do(req)
	var responseBody []byte
	var readErr error
	if callErr == nil {
		result.record.Status = resp.StatusCode
		result.record.RetryAfterNS = int64(parseSustainedRetryAfter(resp.Header.Get("Retry-After"), time.Now()))
		responseBody, readErr = io.ReadAll(io.LimitReader(resp.Body, 4097))
		_ = resp.Body.Close()
	}
	mu.Lock()
	finished = true
	if wrote != 0 {
		counters.written.Add(-1)
	}
	result.record.GotConnUnixNS, result.record.WroteUnixNS = got, wrote
	mu.Unlock()
	result.record.TimeUnixNS = time.Now().UnixNano()
	if callErr != nil {
		result.retryable = true
		switch {
		case errors.Is(callErr, context.Canceled):
			result.record.Error = "canceled"
		case errors.Is(callErr, context.DeadlineExceeded):
			result.record.Error = "timeout"
		default:
			var nerr net.Error
			if errors.As(callErr, &nerr) && nerr.Timeout() {
				result.record.Error = "timeout"
			} else {
				result.record.Error = "transport"
			}
		}
		return result
	}
	if readErr != nil || len(responseBody) > 4096 {
		result.record.Error = "response_read"
		result.retryable = true // an acknowledged commit may have been hidden
		return result
	}
	if resp.StatusCode == http.StatusOK {
		var ack struct {
			Revision string `json:"revision"`
		}
		if json.Unmarshal(responseBody, &ack) != nil || ack.Revision != strconv.FormatInt(task.Revision, 10) {
			result.record.Error = "bad_ack"
			return result
		}
		result.record.Confirmed = true
		result.record.AckRevision = ack.Revision
		return result
	}
	var failure struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(responseBody, &failure) == nil && len(failure.Error.Code) <= 80 {
		// Only known-format codes are retained, never arbitrary response text.
		ok := failure.Error.Code != ""
		for _, c := range failure.Error.Code {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
				ok = false
				break
			}
		}
		if ok {
			result.record.ErrorCode = failure.Error.Code
		}
	}
	result.retryable = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout || (resp.StatusCode >= 500 && resp.StatusCode <= 599)
	if !result.retryable {
		result.record.Error = "unexpected_status"
	}
	return result
}

func parseSustainedRetryAfter(value string, now time.Time) time.Duration {
	if value == "" {
		return 0
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 32); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}

func sustainedBackoff(seed uint64, id int, attempt int, retryAfter time.Duration) time.Duration {
	shift := attempt - 1
	if shift > 5 {
		shift = 5
	}
	d := 500 * time.Millisecond * time.Duration(1<<shift)
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	rng := splitMix{state: seed ^ uint64(id)*0x9e3779b97f4a7c15 ^ uint64(attempt)}
	d += time.Duration(rng.intn(500)) * time.Millisecond
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	if retryAfter > d {
		d = retryAfter
	}
	return d
}
