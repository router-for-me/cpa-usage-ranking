package capacity

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
)

type LoadOptions struct {
	URL      string
	RPS      float64
	Duration time.Duration
	Workers  int
	Offset   int
	Revision int64
	Changed  int
	BothDays bool
	Ledger   string
}
type Receipt struct {
	ID              int     `json:"id"`
	Day             string  `json:"day"`
	Revision        int64   `json:"revision"`
	Changed         int     `json:"changed"`
	Status          int     `json:"status"`
	Confirmed       bool    `json:"confirmed"`
	LatencyMS       float64 `json:"latency_ms"`
	StartedUnixMS   int64   `json:"started_unix_ms"`
	TaskID          string  `json:"task_id,omitempty"`
	Attempt         int     `json:"attempt,omitempty"`
	Source          string  `json:"source,omitempty"`
	PlannedUnixNS   int64   `json:"planned_unix_ns,omitempty"`
	StartedUnixNS   int64   `json:"started_unix_ns,omitempty"`
	GetConnUnixNS   int64   `json:"get_conn_unix_ns,omitempty"`
	GotConnUnixNS   int64   `json:"got_conn_unix_ns,omitempty"`
	WroteUnixNS     int64   `json:"wrote_unix_ns,omitempty"`
	CompletedUnixNS int64   `json:"completed_unix_ns,omitempty"`
	RetryAfter      string  `json:"retry_after,omitempty"`
	Error           string  `json:"error,omitempty"`
	ErrorDetail     string  `json:"error_detail,omitempty"`
}
type Summary struct {
	Scheduled        int         `json:"scheduled"`
	Sent             int         `json:"sent"`
	Confirmed        int         `json:"confirmed"`
	GeneratorDropped int         `json:"generator_dropped"`
	BadACK           int         `json:"bad_ack"`
	Status           map[int]int `json:"status"`
	Seconds          float64     `json:"seconds"`
	AcceptedRPS      float64     `json:"accepted_rps"`
	P50MS            *float64    `json:"p50_ms"`
	P95MS            *float64    `json:"p95_ms"`
	P99MS            *float64    `json:"p99_ms"`
	MaxMS            *float64    `json:"max_ms"`
	MaxScheduleLagMS float64     `json:"max_schedule_lag_ms"`
}

func Load(ctx context.Context, f Fixture, o LoadOptions) (Summary, error) {
	out := Summary{Status: map[int]int{}}
	if err := f.Validate(); err != nil {
		return out, err
	}
	u, err := url.Parse(o.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return out, errors.New("base URL must be an HTTP(S) origin without credentials")
	}
	if math.IsNaN(o.RPS) || math.IsInf(o.RPS, 0) || o.RPS <= 0 || o.RPS > 2000 || o.Duration <= 0 || o.Duration > 20*time.Minute || o.Workers < 1 || o.Workers > 128 || o.Offset < 0 || o.Offset >= f.Clients || o.Revision < 2 || o.Revision > 1000000 || o.Changed < 1 || o.Changed > 8 {
		return out, errors.New("invalid bounded load options")
	}
	out.Scheduled = int(math.Ceil(o.Duration.Seconds() * o.RPS))
	if out.Scheduled > 1000000 {
		return out, errors.New("too many scheduled requests")
	}
	ledger, err := os.OpenFile(o.Ledger, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return out, err
	}
	defer ledger.Close()
	output := bufio.NewWriter(ledger)
	enc := json.NewEncoder(output)
	transport := &http.Transport{MaxIdleConns: o.Workers, MaxIdleConnsPerHost: o.Workers, MaxConnsPerHost: o.Workers, IdleConnTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	jobs := make(chan int)
	results := make(chan Receipt, o.Workers)
	var workers, ready sync.WaitGroup
	ready.Add(o.Workers)
	day, _ := time.Parse(time.DateOnly, f.Day)
	for n := 0; n < o.Workers; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			ready.Done()
			for index := range jobs {
				span := f.Clients
				offset := o.Offset
				if o.BothDays {
					span *= 2
					offset *= 2
				}
				revision := o.Revision + int64(index/span)
				slot := (index + offset) % span
				id := slot + 1
				date := f.Day
				if o.BothDays {
					id = slot/2 + 1
					if slot%2 == 1 {
						date = day.AddDate(0, 0, -1).Format(time.DateOnly)
					}
				}
				r := Receipt{ID: id, Day: date, Revision: revision, Changed: o.Changed}
				body, e := f.Snapshot(revision, o.Changed)
				if e != nil {
					results <- r
					continue
				}
				req, e := http.NewRequestWithContext(ctx, http.MethodPut, u.Scheme+"://"+u.Host+"/v1/clients/report/"+date, bytes.NewReader(body))
				if e != nil {
					results <- r
					continue
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+f.Secret(id))
				started := time.Now()
				r.StartedUnixMS = started.UnixMilli()
				response, e := client.Do(req)
				if e == nil {
					r.Status = response.StatusCode
					data, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
					response.Body.Close()
					if r.Status == 200 && readErr == nil && len(data) <= 4096 {
						var ack struct {
							Revision string `json:"revision"`
						}
						r.Confirmed = json.Unmarshal(data, &ack) == nil && ack.Revision == strconv.FormatInt(revision, 10)
					}
				}
				r.LatencyMS = float64(time.Since(started)) / float64(time.Millisecond)
				results <- r
			}
		}()
	}
	ready.Wait()
	start := time.Now()
	type paceResult struct {
		dropped int
		lag     float64
	}
	pacing := make(chan paceResult, 1)
	go func() {
		var p paceResult
		defer close(jobs)
		defer func() { pacing <- p }()
		for i := 0; i < out.Scheduled; i++ {
			target := start.Add(time.Duration(float64(i) * float64(time.Second) / o.RPS))
			wait := time.Until(target)
			if wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					p.dropped += out.Scheduled - i
					return
				case <-timer.C:
				}
			}
			if ctx.Err() != nil {
				p.dropped += out.Scheduled - i
				return
			}
			lag := float64(time.Since(target)) / float64(time.Millisecond)
			if lag > p.lag {
				p.lag = lag
			}
			select {
			case jobs <- i:
			default:
				p.dropped++
			}
		}
	}()
	go func() { workers.Wait(); close(results) }()
	latencies := make([]float64, 0, out.Scheduled)
	var writeErr error
	for r := range results {
		out.Sent++
		out.Status[r.Status]++
		if r.Confirmed {
			out.Confirmed++
			latencies = append(latencies, r.LatencyMS)
		} else if r.Status == 200 {
			out.BadACK++
		}
		if e := enc.Encode(r); e != nil && writeErr == nil {
			writeErr = e
		}
	}
	p := <-pacing
	out.GeneratorDropped = p.dropped
	out.MaxScheduleLagMS = p.lag
	if remaining := o.Duration - time.Since(start); remaining > 0 {
		timer := time.NewTimer(remaining)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
		}
	}
	out.Seconds = time.Since(start).Seconds()
	out.AcceptedRPS = float64(out.Confirmed) / out.Seconds
	sort.Float64s(latencies)
	quantile := func(q float64) *float64 {
		if len(latencies) == 0 {
			return nil
		}
		value := latencies[int(math.Ceil(q*float64(len(latencies))))-1]
		return &value
	}
	out.P50MS = quantile(.5)
	out.P95MS = quantile(.95)
	out.P99MS = quantile(.99)
	out.MaxMS = quantile(1)
	if e := output.Flush(); e != nil && writeErr == nil {
		writeErr = e
	}
	if writeErr != nil {
		return out, writeErr
	}
	return out, ctx.Err()
}

func openReadDB(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := url.Values{"mode": {"ro"}, "_query_only": {"1"}, "_busy_timeout": {"5000"}}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite3", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}

// Verify checks every touched identity/day against its attempted snapshots.
// A 4xx response is a definite rejection; a 5xx, transport failure or invalid
// success body can be an ambiguous commit. Repeated revisions are accepted only
// as explicitly numbered attempts of the same logical task and same content.
func Verify(ctx context.Context, f Fixture, path string, ledgers []string) (map[string]int, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if len(ledgers) == 0 {
		return nil, errors.New("verify requires complete receipt history")
	}
	day, _ := time.Parse(time.DateOnly, f.Day)
	yesterday := day.AddDate(0, 0, -1).Format(time.DateOnly)
	type key struct {
		id  int
		day string
	}
	type attemptGroup struct {
		changed   int
		taskID    string
		numbers   map[int]bool
		rejected  int
		plausible bool // confirmation or an ambiguous attempt could have committed
	}
	type taskIdentity struct {
		key      key
		revision int64
	}
	attempts := map[key]map[int64]*attemptGroup{}
	taskIDs := map[string]taskIdentity{}
	confirmed := map[key]int64{}
	for _, name := range ledgers {
		file, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		scan := bufio.NewScanner(file)
		for scan.Scan() {
			var r Receipt
			if err = json.Unmarshal(scan.Bytes(), &r); err != nil {
				file.Close()
				return nil, err
			}
			if r.ID < 1 || r.ID > f.Clients || (r.Day != f.Day && r.Day != yesterday) || r.Revision < 2 || r.Changed < 1 || r.Changed > len(f.Models) || r.Status < 0 || r.Status > 599 || (r.Status > 0 && r.Status < 100) || (r.Confirmed && r.Status != http.StatusOK) {
				file.Close()
				return nil, errors.New("invalid ledger receipt")
			}
			k := key{r.ID, r.Day}
			if r.TaskID != "" {
				identity := taskIdentity{k, r.Revision}
				if old, ok := taskIDs[r.TaskID]; ok && old != identity {
					file.Close()
					return nil, errors.New("task ID reused for a different identity/day/revision")
				}
				taskIDs[r.TaskID] = identity
			}
			if attempts[k] == nil {
				attempts[k] = map[int64]*attemptGroup{}
			}
			group := attempts[k][r.Revision]
			if group == nil {
				group = &attemptGroup{changed: r.Changed, taskID: r.TaskID, numbers: map[int]bool{}}
				attempts[k][r.Revision] = group
			} else if r.TaskID == "" || group.taskID == "" || r.TaskID != group.taskID || r.Changed != group.changed {
				file.Close()
				return nil, errors.New("reused identity/day revision with different or missing task/content")
			}
			if r.TaskID != "" {
				if r.Attempt < 1 || group.numbers[r.Attempt] {
					file.Close()
					return nil, errors.New("invalid or duplicated task attempt number")
				}
				group.numbers[r.Attempt] = true
			} else if r.Attempt != 0 {
				file.Close()
				return nil, errors.New("attempt number requires task ID")
			}
			if r.Status >= 400 && r.Status < 500 {
				group.rejected++
			} else {
				group.plausible = true
			}
			if r.Confirmed && r.Revision > confirmed[k] {
				confirmed[k] = r.Revision
			}
		}
		err = scan.Err()
		file.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(attempts) == 0 {
		return nil, errors.New("verify received no receipts")
	}
	for _, versions := range attempts {
		for _, group := range versions {
			for n := 1; n <= len(group.numbers); n++ {
				if !group.numbers[n] {
					return nil, errors.New("incomplete task attempt history")
				}
			}
		}
	}
	db, err := readDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	modelIDs := make([]int64, len(f.Models))
	for i, name := range f.Models {
		if err = db.QueryRowContext(ctx, `SELECT id FROM models WHERE model_name=?`, name).Scan(&modelIDs[i]); err != nil {
			return nil, fmt.Errorf("fixture model not found: %w", err)
		}
	}
	type version struct {
		revision int64
		changed  int
	}
	type expected struct {
		digest [32]byte
		rows   map[int64]reports.Metrics
	}
	expectedVersions := map[version]expected{}
	result := map[string]int{"checked": 0, "unconfirmed_commits": 0, "unchanged_at_seed": 0, "rejections_checked": 0, "rejections_superseded": 0, "rejections_mixed": 0}
	for k, versions := range attempts {
		var revision int64
		var digest []byte
		if err = db.QueryRowContext(ctx, `SELECT revision,digest FROM daily_writer_versions WHERE day=? AND writer_id=?`, k.day, k.id).Scan(&revision, &digest); err != nil {
			return nil, err
		}
		if revision < confirmed[k] {
			return nil, fmt.Errorf("lost confirmation for synthetic client %d", k.id)
		}
		for attemptedRevision, group := range versions {
			if group.rejected == 0 {
				continue
			}
			switch {
			case revision == attemptedRevision && !group.plausible:
				return nil, fmt.Errorf("explicitly rejected revision committed for synthetic client %d", k.id)
			case revision == attemptedRevision:
				// The same content also had a confirmed or ambiguous attempt.
				// Its final presence cannot prove that the 4xx wrote anything.
				result["rejections_mixed"] += group.rejected
			case revision < attemptedRevision:
				result["rejections_checked"] += group.rejected
			default:
				// A later commit hides whether this rejected revision briefly
				// committed; do not count it as a verified rejection.
				result["rejections_superseded"] += group.rejected
			}
		}
		changed := 2
		if revision != 1 {
			group, ok := versions[revision]
			if !ok {
				return nil, fmt.Errorf("unattempted revision for synthetic client %d", k.id)
			}
			changed = group.changed
			if revision > confirmed[k] {
				result["unconfirmed_commits"]++
			}
		} else {
			result["unchanged_at_seed"]++
		}
		v := version{revision, changed}
		want, ok := expectedVersions[v]
		if !ok {
			body, e := f.Snapshot(revision, changed)
			if e != nil {
				return nil, e
			}
			report, e := reports.Decode(body)
			if e != nil {
				return nil, e
			}
			prepared, e := reports.Prepare(report)
			if e != nil {
				return nil, e
			}
			want = expected{digest: prepared.Digest(), rows: make(map[int64]reports.Metrics, len(modelIDs)+1)}
			var total reports.Metrics
			for i, id := range modelIDs {
				delta := int64(0)
				if i < changed {
					delta = revision - 1
				}
				m := reports.Metrics{TotalTokens: 1000 + delta, InputTokens: 600 + delta, OutputTokens: 400, CacheReadTokens: 200, CacheWriteTokens: 20, ReasoningTokens: 50, RequestCount: 10, SuccessCount: 9, TTFTSumMS: float64(100+i) * 8, TTFTSamples: 8, LatencySumMS: float64(500+i) * 10, LatencySamples: 10}
				want.rows[id] = m
				total, ok = reports.AddMetrics(total, m)
				if !ok {
					return nil, errors.New("fixture metrics overflow")
				}
			}
			want.rows[0] = total
			expectedVersions[v] = want
		}
		if !bytes.Equal(digest, want.digest[:]) {
			return nil, errors.New("committed digest mismatch")
		}
		rows, e := db.QueryContext(ctx, `SELECT model_id,`+metricColumns+` FROM daily_stats WHERE day=? AND client_id=?`, k.day, k.id)
		if e != nil {
			return nil, e
		}
		count := 0
		for rows.Next() {
			var id int64
			var m reports.Metrics
			if e = rows.Scan(&id, &m.TotalTokens, &m.InputTokens, &m.OutputTokens, &m.CacheReadTokens, &m.CacheWriteTokens, &m.ReasoningTokens, &m.RequestCount, &m.SuccessCount, &m.TTFTSumMS, &m.TTFTSamples, &m.LatencySumMS, &m.LatencySamples); e != nil {
				rows.Close()
				return nil, e
			}
			expectedRow, found := want.rows[id]
			if !found || m != expectedRow {
				rows.Close()
				return nil, fmt.Errorf("snapshot metrics mismatch for synthetic client %d", k.id)
			}
			count++
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return nil, e
		}
		if count != len(want.rows) {
			return nil, fmt.Errorf("snapshot row count mismatch for synthetic client %d", k.id)
		}
		result["checked"]++
	}
	return result, nil
}

// Probe measures actual candidate reads without introducing a production hook.
// Run separately under the same resource cap; it is not concurrent-service timing.
func Probe(ctx context.Context, path, day string) ([]map[string]any, error) {
	db, err := readDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	reader, err := statistics.NewReader(db, statistics.DefaultMaxCandidates, statistics.DefaultReadTimeout)
	if err != nil {
		return nil, err
	}
	date, err := time.Parse(time.DateOnly, day)
	if err != nil || date.Format(time.DateOnly) != day {
		return nil, errors.New("invalid probe day")
	}
	result := []map[string]any{}
	for _, kind := range []string{"day", "week", "month", "year", "all"} {
		start := time.Now()
		s, e := reader.Read(ctx, statistics.Selection{Kind: kind, Day: periods.LeaderboardDay(kind, date), Public: true})
		if e != nil {
			return nil, e
		}
		elapsed := time.Since(start)
		var total reports.Metrics
		for _, m := range s.Rows {
			var ok bool
			total, ok = reports.AddMetrics(total, m)
			if !ok {
				return nil, errors.New("probe aggregate metrics overflow")
			}
		}
		result = append(result, map[string]any{"kind": kind, "milliseconds": float64(elapsed) / float64(time.Millisecond), "candidates": len(s.Rows), "metrics": total})
	}
	return result, nil
}
