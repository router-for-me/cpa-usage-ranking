package capacity

// This file implements an opt-in synthetic-client experiment. It is not used
// by the production server. An HTTP write timestamp is local to the generator:
// in particular it does not establish when the peer behind an SSH tunnel read it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type BurstOptions struct {
	URL, Ledger, Events, Samples string
	Duration, Cycle              time.Duration
	RequestTimeout               time.Duration
	RetrySpread, MaxRetryDelay   time.Duration
	MaxAttempts, QueuePerClient  int
	MaxPending, Changed          int
}

type burstTask struct {
	ID            int    `json:"id"`
	Day           string `json:"day"`
	Revision      int64  `json:"revision"`
	Changed       int    `json:"changed"`
	TaskID        string `json:"task_id"`
	Source        string `json:"source"`
	PlannedUnixNS int64  `json:"planned_unix_ns"`
	CreatedUnixNS int64  `json:"created_unix_ns"`
	prepared      *http.Request
	body          []byte
}

type burstEvent struct {
	Type       string     `json:"type"`
	TimeUnixNS int64      `json:"time_unix_ns"`
	Task       *burstTask `json:"task,omitempty"`
	Reason     string     `json:"reason,omitempty"`
}

type burstCounts struct {
	produced, confirmed, background, initialConfirmed atomic.Int64
	started, written, retries, badACK, initialWritten atomic.Int64
	waiting, inFlight, peak, lateTrace, maxProduceLag atomic.Int64
	firstWriteMin, firstWriteMax                      atomic.Int64
	statuses                                          [600]atomic.Int64
}

type BurstSample struct {
	Phase                       string        `json:"phase"`
	TimeUnixNS                  int64         `json:"time_unix_ns"`
	ElapsedSeconds              float64       `json:"elapsed_seconds"`
	Produced                    int64         `json:"produced"`
	BackgroundProduced          int64         `json:"background_produced"`
	Confirmed                   int64         `json:"confirmed"`
	InitialTodayConfirmed       int64         `json:"initial_today_confirmed"`
	Pending                     int64         `json:"pending"`
	AttemptsStarted             int64         `json:"attempts_started"`
	AttemptsWritten             int64         `json:"attempts_written"`
	Retries                     int64         `json:"retries"`
	BadACK                      int64         `json:"bad_ack"`
	InitialFirstAttemptsWritten int64         `json:"initial_first_attempts_written"`
	FirstInitialWriteUnixNS     int64         `json:"first_initial_write_unix_ns"`
	LastInitialWriteUnixNS      int64         `json:"last_initial_write_unix_ns"`
	ConnectionWaiting           int64         `json:"connection_waiting"`
	WrittenUnfinished           int64         `json:"written_unfinished"`
	WrittenUnfinishedPeak       int64         `json:"written_unfinished_peak"`
	LateTraceCallbacks          int64         `json:"late_trace_callbacks"`
	MaxProducerLagMS            float64       `json:"max_producer_lag_ms"`
	Status                      map[int]int64 `json:"status"`
}

type BurstSummary struct {
	BurstSample
	Clients                       int      `json:"clients"`
	InitialYesterday              int      `json:"initial_yesterday"`
	ReleasedUnixNS                int64    `json:"released_unix_ns"`
	InitialWriteSpanMS            *float64 `json:"initial_write_span_ms"`
	ObservedInitialWriteSpanMS    *float64 `json:"observed_initial_write_span_ms"`
	LastInitialWriteFromReleaseMS *float64 `json:"last_initial_write_from_release_ms"`
	Reason                        string   `json:"reason"`
}

func setInitialWriteSpans(summary *BurstSummary) {
	if summary.FirstInitialWriteUnixNS == 0 {
		return
	}
	span := float64(summary.LastInitialWriteUnixNS-summary.FirstInitialWriteUnixNS) / float64(time.Millisecond)
	delay := float64(summary.LastInitialWriteUnixNS-summary.ReleasedUnixNS) / float64(time.Millisecond)
	summary.ObservedInitialWriteSpanMS = &span
	if summary.InitialFirstAttemptsWritten == int64(summary.Clients) {
		summary.InitialWriteSpanMS = &span
	}
	summary.LastInitialWriteFromReleaseMS = &delay
}

func updateMax(a *atomic.Int64, n int64) {
	for old := a.Load(); n > old; old = a.Load() {
		if a.CompareAndSwap(old, n) {
			return
		}
	}
}
func updateMin(a *atomic.Int64, n int64) {
	for old := a.Load(); old == 0 || n < old; old = a.Load() {
		if a.CompareAndSwap(old, n) {
			return
		}
	}
}
func (c *burstCounts) sample(start time.Time) BurstSample {
	now := time.Now()
	s := BurstSample{TimeUnixNS: now.UnixNano(), ElapsedSeconds: now.Sub(start).Seconds(), Produced: c.produced.Load(), BackgroundProduced: c.background.Load(), Confirmed: c.confirmed.Load(), InitialTodayConfirmed: c.initialConfirmed.Load(), AttemptsStarted: c.started.Load(), AttemptsWritten: c.written.Load(), Retries: c.retries.Load(), BadACK: c.badACK.Load(), InitialFirstAttemptsWritten: c.initialWritten.Load(), FirstInitialWriteUnixNS: c.firstWriteMin.Load(), LastInitialWriteUnixNS: c.firstWriteMax.Load(), ConnectionWaiting: c.waiting.Load(), WrittenUnfinished: c.inFlight.Load(), WrittenUnfinishedPeak: c.peak.Load(), LateTraceCallbacks: c.lateTrace.Load(), MaxProducerLagMS: float64(c.maxProduceLag.Load()) / float64(time.Millisecond), Status: map[int]int64{}}
	s.Pending = s.Produced - s.Confirmed
	for i := range c.statuses {
		if n := c.statuses[i].Load(); n != 0 {
			s.Status[i] = n
		}
	}
	return s
}

type burstLog struct {
	kind  byte
	value any
}

// Burst generates N initial today's tasks at a barrier, with an additional
// yesterday task for every tenth client, and independently produces new work.
// A client processes all its tasks serially. Retry attempts retain the same
// task, revision and body. Outstanding created tasks remain visible in Events
// if a limit, cancellation or a fatal error stops the experiment.
func Burst(ctx context.Context, f Fixture, o BurstOptions) (BurstSummary, error) {
	var summary BurstSummary
	if err := f.Validate(); err != nil {
		return summary, err
	}
	u, err := url.Parse(o.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return summary, errors.New("base URL must be an HTTP(S) origin without credentials")
	}
	if f.Clients > 20000 || o.Duration <= 0 || o.Duration > 20*time.Minute || o.Cycle < 100*time.Millisecond || o.Cycle > 24*time.Hour || o.RequestTimeout < 10*time.Millisecond || o.RequestTimeout > 2*time.Minute || o.RetrySpread < 0 || o.RetrySpread > 2*time.Minute || o.MaxRetryDelay < 10*time.Millisecond || o.MaxRetryDelay > 5*time.Minute || o.MaxAttempts < 1 || o.MaxAttempts > 100 || o.QueuePerClient < 2 || o.QueuePerClient > 64 || o.MaxPending < f.Clients+f.Clients/10 || o.MaxPending > 200000 || o.Changed < 1 || o.Changed > 8 || o.Ledger == "" || o.Events == "" || o.Samples == "" {
		return summary, errors.New("invalid bounded burst options")
	}
	// Bound the total number of generated tasks, not merely the instantaneous queue.
	maxTasks := float64(f.Clients+f.Clients/10) + math.Ceil(o.Duration.Seconds()/o.Cycle.Seconds()*float64(f.Clients+f.Clients/10)) + 2
	if maxTasks > 1000000 {
		return summary, errors.New("too many planned burst tasks")
	}
	files := make([]*os.File, 0, 3)
	for _, name := range []string{o.Ledger, o.Events, o.Samples} {
		file, e := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			for _, f := range files {
				_ = f.Close()
			}
			return summary, e
		}
		files = append(files, file)
	}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make(chan error, 1)
	fail := func(e error) {
		select {
		case failures <- e:
		default:
		}
		cancel()
	}
	logs := make(chan burstLog, 4*f.Clients+1024)
	logDone := make(chan error, 1)
	go func() {
		writers := []*bufio.Writer{bufio.NewWriterSize(files[0], 256*1024), bufio.NewWriterSize(files[1], 256*1024), bufio.NewWriterSize(files[2], 256*1024)}
		enc := []*json.Encoder{json.NewEncoder(writers[0]), json.NewEncoder(writers[1]), json.NewEncoder(writers[2])}
		var first error
		for item := range logs {
			if first == nil {
				first = enc[int(item.kind)].Encode(item.value)
				// Keep the independent clock and queue samples visible to the
				// controller during the run, not only at process exit.
				if first == nil && item.kind == 2 {
					first = writers[2].Flush()
				}
				if first != nil {
					fail(fmt.Errorf("evidence write: %w", first))
				}
			}
		}
		for _, w := range writers {
			if e := w.Flush(); e != nil && first == nil {
				first = e
			}
		}
		logDone <- first
	}()
	// A full evidence queue is a generator failure, not permission to drop
	// records. Stop the experiment and wait for the writer to accept this record.
	emit := func(item burstLog) {
		select {
		case logs <- item:
			return
		default:
			fail(errors.New("evidence queue capacity exceeded"))
		}
		logs <- item
	}
	finishLogs := func() error { close(logs); return <-logDone }

	transport := &http.Transport{MaxIdleConns: f.Clients, MaxIdleConnsPerHost: f.Clients, MaxConnsPerHost: f.Clients, IdleConnTimeout: 90 * time.Second, DisableCompression: true, ForceAttemptHTTP2: false}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	day, _ := time.Parse(time.DateOnly, f.Day)
	yesterday := day.AddDate(0, 0, -1).Format(time.DateOnly)
	initialBody, err := f.Snapshot(2, o.Changed)
	if err != nil {
		_ = finishLogs()
		return summary, err
	}
	type actor struct {
		queue  chan *burstTask
		secret string
	}
	actors := make([]actor, f.Clients)
	counts := &burstCounts{}
	makeTask := func(id int, date string, revision int64, source string, planned int64) *burstTask {
		return &burstTask{ID: id, Day: date, Revision: revision, Changed: o.Changed, TaskID: fmt.Sprintf("%d/%s/%d", id, date, revision), Source: source, PlannedUnixNS: planned, CreatedUnixNS: time.Now().UnixNano()}
	}
	create := func(task *burstTask) {
		counts.produced.Add(1)
		if task.Source == "background" {
			counts.background.Add(1)
		}
		copy := *task
		copy.prepared = nil
		copy.body = nil
		emit(burstLog{1, burstEvent{Type: "created", TimeUnixNS: time.Now().UnixNano(), Task: &copy}})
	}
	for i := range actors {
		id := i + 1
		actors[i] = actor{queue: make(chan *burstTask, o.QueuePerClient), secret: f.Secret(id)}
		t := makeTask(id, f.Day, 2, "initial_today", 0)
		req, e := http.NewRequest(http.MethodPut, u.Scheme+"://"+u.Host+"/v1/clients/report/"+f.Day, bytes.NewReader(initialBody))
		if e != nil {
			_ = finishLogs()
			return summary, e
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+actors[i].secret)
		t.prepared = req
		t.body = initialBody
		create(t)
		actors[i].queue <- t
		if id%10 == 0 {
			y := makeTask(id, yesterday, 2, "initial_yesterday", 0)
			create(y)
			actors[i].queue <- y
		}
	}
	if runCtx.Err() != nil {
		e := finishLogs()
		if e != nil {
			return summary, e
		}
		select {
		case e := <-failures:
			return summary, e
		default:
			return summary, runCtx.Err()
		}
	}
	gate := make(chan struct{})
	var ready, workers sync.WaitGroup
	ready.Add(f.Clients)
	workers.Add(f.Clients)
	var released time.Time
	for i := range actors {
		a := &actors[i]
		go func() {
			defer workers.Done()
			ready.Done()
			<-gate
			for {
				select {
				case <-runCtx.Done():
					return
				default:
				}
				select {
				case <-runCtx.Done():
					return
				case task := <-a.queue:
					if task.PlannedUnixNS == 0 {
						task.PlannedUnixNS = released.UnixNano()
					}
					if e := runBurstTask(runCtx, client, u, f, o, task, a.secret, counts, emit); e != nil {
						if runCtx.Err() == nil {
							emit(burstLog{1, burstEvent{Type: "terminal", TimeUnixNS: time.Now().UnixNano(), Task: task, Reason: e.Error()}})
							fail(e)
						}
						return
					}
				}
			}
		}()
	}
	ready.Wait()
	released = time.Now()
	emit(burstLog{1, burstEvent{Type: "release", TimeUnixNS: released.UnixNano()}})
	close(gate)
	var producers sync.WaitGroup
	produce := func(date string, count int, step int) {
		defer producers.Done()
		if count == 0 {
			return
		}
		timer := time.NewTimer(time.Hour)
		if !timer.Stop() {
			<-timer.C
		}
		defer timer.Stop()
		for index := int64(0); ; index++ {
			cycle := index / int64(count)
			slot := index%int64(count) + 1
			target := released.Add(time.Duration(cycle)*o.Cycle + time.Duration(slot*int64(o.Cycle)/int64(count)))
			if wait := time.Until(target); wait > 0 {
				timer.Reset(wait)
				select {
				case <-runCtx.Done():
					if !timer.Stop() {
						select {
						case <-timer.C:
						default:
						}
					}
					return
				case <-timer.C:
				}
			}
			if runCtx.Err() != nil {
				return
			}
			lag := time.Since(target)
			if lag > 0 {
				updateMax(&counts.maxProduceLag, int64(lag))
			}
			id := int(slot) * step
			task := makeTask(id, date, 3+cycle, "background", target.UnixNano())
			create(task)
			if counts.produced.Load()-counts.confirmed.Load() > int64(o.MaxPending) {
				fail(errors.New("pending task limit exceeded"))
				return
			}
			select {
			case actors[id-1].queue <- task:
			default:
				fail(errors.New("per-client task queue limit exceeded"))
				return
			}
		}
	}
	producers.Add(2)
	go produce(f.Day, f.Clients, 1)
	go produce(yesterday, f.Clients/10, 10)
	samplerDone := make(chan struct{})
	go func() {
		defer close(samplerDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		sample := counts.sample(released)
		sample.Phase = "running"
		emit(burstLog{2, sample})
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				sample := counts.sample(released)
				sample.Phase = "running"
				emit(burstLog{2, sample})
			}
		}
	}()
	timer := time.NewTimer(o.Duration)
	reason := "duration"
	select {
	case <-timer.C:
	case <-ctx.Done():
		reason = "canceled"
	case <-runCtx.Done():
		reason = "failed"
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	loadEnd := counts.sample(released)
	loadEnd.Phase = "load_end"
	emit(burstLog{2, loadEnd})
	cancel()
	producers.Wait()
	workers.Wait()
	<-samplerDone
	afterCancel := counts.sample(released)
	afterCancel.Phase = "after_cancel"
	emit(burstLog{2, afterCancel})
	emit(burstLog{1, burstEvent{Type: "stop", TimeUnixNS: time.Now().UnixNano(), Reason: reason}})
	logErr := finishLogs()
	summary = BurstSummary{BurstSample: counts.sample(released), Clients: f.Clients, InitialYesterday: f.Clients / 10, ReleasedUnixNS: released.UnixNano(), Reason: reason}
	setInitialWriteSpans(&summary)
	if logErr != nil {
		return summary, logErr
	}
	select {
	case e := <-failures:
		return summary, e
	default:
	}
	if ctx.Err() != nil {
		return summary, ctx.Err()
	}
	return summary, nil
}

func runBurstTask(ctx context.Context, client *http.Client, u *url.URL, f Fixture, o BurstOptions, task *burstTask, secret string, counts *burstCounts, emit func(burstLog)) error {
	body := task.body
	var err error
	if body == nil {
		body, err = f.Snapshot(task.Revision, task.Changed)
		if err != nil {
			return err
		}
	}
	for attempt := 1; attempt <= o.MaxAttempts; attempt++ {
		attemptNumber := attempt
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt > 1 {
			counts.retries.Add(1)
		}
		started := time.Now()
		receipt := Receipt{ID: task.ID, Day: task.Day, Revision: task.Revision, Changed: task.Changed, TaskID: task.TaskID, Attempt: attempt, Source: task.Source, PlannedUnixNS: task.PlannedUnixNS, StartedUnixNS: started.UnixNano(), StartedUnixMS: started.UnixMilli()}
		counts.started.Add(1)
		var mu sync.Mutex
		waiting, written, finished := false, false, false
		trace := &httptrace.ClientTrace{
			GetConn: func(string) {
				mu.Lock()
				defer mu.Unlock()
				if finished {
					counts.lateTrace.Add(1)
					return
				}
				receipt.GetConnUnixNS = time.Now().UnixNano()
				waiting = true
				counts.waiting.Add(1)
			},
			GotConn: func(httptrace.GotConnInfo) {
				mu.Lock()
				defer mu.Unlock()
				if finished {
					counts.lateTrace.Add(1)
					return
				}
				receipt.GotConnUnixNS = time.Now().UnixNano()
				if waiting {
					waiting = false
					counts.waiting.Add(-1)
				}
			},
			WroteRequest: func(info httptrace.WroteRequestInfo) {
				mu.Lock()
				defer mu.Unlock()
				if finished {
					counts.lateTrace.Add(1)
					return
				}
				if info.Err != nil || written {
					return
				}
				written = true
				receipt.WroteUnixNS = time.Now().UnixNano()
				counts.written.Add(1)
				n := counts.inFlight.Add(1)
				updateMax(&counts.peak, n)
				if task.Source == "initial_today" && attemptNumber == 1 {
					counts.initialWritten.Add(1)
					updateMin(&counts.firstWriteMin, receipt.WroteUnixNS)
					updateMax(&counts.firstWriteMax, receipt.WroteUnixNS)
				}
			},
		}
		attemptCtx, cancel := context.WithTimeout(httptrace.WithClientTrace(ctx, trace), o.RequestTimeout)
		var req *http.Request
		if attempt == 1 && task.prepared != nil {
			req = task.prepared.WithContext(attemptCtx)
		} else {
			req, err = http.NewRequestWithContext(attemptCtx, http.MethodPut, u.Scheme+"://"+u.Host+"/v1/clients/report/"+task.Day, bytes.NewReader(body))
			if err != nil {
				cancel()
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		response, doErr := client.Do(req)
		if doErr == nil {
			receipt.Status = response.StatusCode
			receipt.RetryAfter = response.Header.Get("Retry-After")
			if len(receipt.RetryAfter) > 128 {
				receipt.RetryAfter = receipt.RetryAfter[:128]
			}
			data, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
			_ = response.Body.Close()
			if readErr != nil {
				receipt.Error = "response_read"
			} else if receipt.Status == http.StatusOK && len(data) <= 4096 {
				var ack struct {
					Revision string `json:"revision"`
				}
				receipt.Confirmed = json.Unmarshal(data, &ack) == nil && ack.Revision == strconv.FormatInt(task.Revision, 10)
			}
		} else if ctx.Err() != nil {
			receipt.Error = "canceled"
		} else if errors.Is(doErr, context.DeadlineExceeded) {
			receipt.Error = "timeout"
		} else {
			receipt.Error = "transport"
			receipt.ErrorDetail = burstTransportError(doErr)
		}
		cancel()
		mu.Lock()
		finished = true
		if waiting {
			counts.waiting.Add(-1)
		}
		if written {
			counts.inFlight.Add(-1)
		}
		receipt.CompletedUnixNS = time.Now().UnixNano()
		mu.Unlock()
		receipt.LatencyMS = float64(receipt.CompletedUnixNS-receipt.StartedUnixNS) / float64(time.Millisecond)
		if receipt.Status >= 0 && receipt.Status < len(counts.statuses) {
			counts.statuses[receipt.Status].Add(1)
		}
		if receipt.Confirmed {
			counts.confirmed.Add(1)
			if task.Source == "initial_today" {
				counts.initialConfirmed.Add(1)
			}
		}
		emit(burstLog{0, receipt})
		if receipt.Confirmed {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if receipt.Status == http.StatusOK {
			counts.badACK.Add(1)
			return errors.New("invalid success acknowledgement")
		}
		if receipt.Status >= 400 && receipt.Status < 500 && receipt.Status != http.StatusTooManyRequests && receipt.Status != http.StatusRequestTimeout {
			return fmt.Errorf("non-retryable HTTP status %d", receipt.Status)
		}
		if receipt.Status > 0 && receipt.Status < 500 && receipt.Status != http.StatusTooManyRequests && receipt.Status != http.StatusRequestTimeout {
			return fmt.Errorf("unexpected HTTP status %d", receipt.Status)
		}
		if attempt == o.MaxAttempts {
			return errors.New("task retry limit exceeded")
		}
		delay, e := burstRetryDelay(receipt.RetryAfter, task.ID, task.Revision, attempt, o.RetrySpread, o.MaxRetryDelay)
		if e != nil {
			return e
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("unreachable retry state")
}

// Preserve useful failure categories without logging URLs, request headers or
// arbitrary error strings that might contain private information.
func burstTransportError(err error) string {
	for _, item := range []struct {
		err  error
		name string
	}{{io.EOF, "eof"}, {io.ErrUnexpectedEOF, "unexpected_eof"}, {syscall.ECONNRESET, "connection_reset"}, {syscall.ECONNREFUSED, "connection_refused"}, {syscall.EPIPE, "broken_pipe"}, {syscall.EMFILE, "process_fd_limit"}, {syscall.ENFILE, "system_fd_limit"}, {syscall.EADDRNOTAVAIL, "local_address_unavailable"}, {syscall.ETIMEDOUT, "network_timeout"}} {
		if errors.Is(err, item.err) {
			return item.name
		}
	}
	var op *net.OpError
	if errors.As(err, &op) {
		switch op.Op {
		case "dial", "read", "write":
			return "network_" + op.Op
		}
	}
	return "other"
}

func burstRetryDelay(header string, id int, revision int64, attempt int, spread, limit time.Duration) (time.Duration, error) {
	base := time.Second
	for n := 1; n < attempt && base < limit; n++ {
		if base > limit/2 {
			base = limit
			break
		}
		base *= 2
	}
	if base > limit {
		base = limit
	}
	if header != "" {
		var retry time.Duration
		if n, e := strconv.ParseInt(header, 10, 32); e == nil && n >= 0 {
			retry = time.Duration(n) * time.Second
		} else if when, e := http.ParseTime(header); e == nil {
			retry = time.Until(when)
			if retry < 0 {
				retry = 0
			}
		}
		if retry > limit {
			return 0, errors.New("Retry-After exceeds retry delay bound")
		}
		if retry > base {
			base = retry
		}
	}
	// Deterministic, identity/attempt-specific jitter avoids a second barrier.
	x := uint64(id)*0x9e3779b97f4a7c15 + uint64(revision)*0xbf58476d1ce4e5b9 + uint64(attempt)*0x94d049bb133111eb
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	room := limit - base
	if spread > room {
		spread = room
	}
	if spread > 0 {
		base += time.Duration(x % uint64(spread+1))
	}
	return base, nil
}
