package capacity

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"syscall"
	"time"
)

type SustainedResult struct {
	Sample SustainedSample `json:"sample"`
	Reason string          `json:"reason"`
}

// Account for arrivals due before a control boundary using the old stage's
// clock. Otherwise a queued pause or stage change could erase overdue input.
func sustainedArrivalsThrough(s *sustainedState, through time.Time, draw func(float64) time.Duration, create func(string, int64) error) error {
	for s.rate > 0 && !s.nextArrival.After(through) {
		planned := s.nextArrival.UnixNano()
		s.nextArrival = s.nextArrival.Add(draw(s.rate))
		if err := create("arrival", planned); err != nil {
			return err
		}
	}
	return nil
}

// RunSustained starts paused. The caller owns the commands channel and must
// impose the search-specific 40-minute budget and external safety guard. The
// driver's cumulative active-time limit is an additional hard ceiling, not an
// observation that any stage passed. Closing commands safely stops the run.
func RunSustained(ctx context.Context, f Fixture, o SustainedOptions, commands <-chan SustainedCommand, samples chan<- SustainedSample) (result SustainedResult, retErr error) {
	if err := validateSustainedOptions(f, o); err != nil {
		return result, err
	}
	model, err := NewSustainedModel(f.Day, o.Models, f.Clients, o.ModelSeed)
	if err != nil {
		return result, err
	}
	flags := os.O_RDWR | os.O_APPEND
	if !o.Resume {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := os.OpenFile(o.Journal, flags, 0600)
	if err != nil {
		return result, err
	}
	defer func() {
		if e := file.Close(); e != nil && retErr == nil {
			retErr = e
		}
	}()
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, fmt.Errorf("journal already locked: %w", err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	state := newSustainedState(model, o.ArrivalSeed)
	header := sustainedHeaderFor(f, o)
	compressed := strings.HasSuffix(o.Journal, ".gz")
	if (o.RetryMigrationPath != "" || o.PendingMigrationPath != "") && !o.Resume {
		return result, errors.New("reviewed migration requires a closed journal resume")
	}
	migration, err := loadSustainedRetryMigration(o.RetryMigrationPath, file)
	if err != nil {
		return result, err
	}
	pendingMigration, err := loadSustainedPendingMigration(o.PendingMigrationPath, file)
	if err != nil {
		return result, err
	}
	needsMigration, needsPendingMigration := false, false
	if o.Resume {
		if err = restoreSustained(file, header, state, compressed, migration, pendingMigration, &needsMigration, &needsPendingMigration); err != nil {
			return result, err
		}
	}
	stat, err := file.Stat()
	if err != nil {
		return result, err
	}
	counter := &sustainedByteCounter{out: file, n: stat.Size()}
	var out io.Writer = counter
	var compressor *gzip.Writer
	if compressed {
		compressor, err = gzip.NewWriterLevel(counter, gzip.BestSpeed)
		if err != nil {
			return result, err
		}
		out = compressor
	}
	journal := &sustainedJournal{file: file, out: bufio.NewWriterSize(out, 256*1024), counter: counter, gzip: compressor, bytes: stat.Size()}
	defer func() {
		if closeErr := errors.Join(journal.close(), file.Sync()); closeErr != nil {
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	if !o.Resume {
		if err = journal.write(sustainedRecord{Kind: "header", TimeUnixNS: time.Now().UnixNano(), Header: &header}); err != nil {
			return result, err
		}
	} else {
		if needsMigration {
			if err = journal.write(sustainedRecord{Kind: "retry_migration", TimeUnixNS: time.Now().UnixNano(), RetryMigration: migration}); err != nil {
				return result, err
			}
			// Record the approved boundary before any command can start input.
			if err = journal.flush(); err != nil {
				return result, err
			}
			if err = file.Sync(); err != nil {
				return result, err
			}
		}
		if needsPendingMigration {
			if err = journal.write(sustainedRecord{Kind: "pending_migration", TimeUnixNS: time.Now().UnixNano(), PendingMigration: pendingMigration}); err != nil {
				return result, err
			}
			if err = journal.flush(); err != nil {
				return result, err
			}
			if err = file.Sync(); err != nil {
				return result, err
			}
		}
		// Normally all starts finished at the clean end. Explicitly record any
		// retained ambiguous starts before another attempt is allowed.
		ids := make([]int, 0, len(state.actors))
		for id := range state.actors {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			a := state.actors[id]
			if a.inFlight {
				if err = journal.write(sustainedRecord{Kind: "recover", TimeUnixNS: time.Now().UnixNano(), Client: id, Sequence: a.queue[0].task.Sequence}); err != nil {
					return result, err
				}
				a.inFlight = false
			}
		}
	}
	if err = journal.flush(); err != nil {
		return result, err
	}
	if state.activeElapsed >= o.MaxActive {
		return result, errors.New("active-time budget exhausted")
	}
	if state.produced-state.confirmed > int64(o.MaxPending) || journal.bytes >= o.MaxJournalBytes {
		return result, errors.New("retained debt or journal already exceeds local safety bound")
	}

	transport := &http.Transport{MaxIdleConns: o.MaxInFlight, MaxIdleConnsPerHost: o.MaxInFlight, MaxConnsPerHost: o.MaxInFlight, IdleConnTimeout: 90 * time.Second, DisableCompression: true, ForceAttemptHTTP2: false}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	requestCtx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	responses := make(chan sustainedHTTPResult, o.MaxInFlight)
	trace := &sustainedTraceCounts{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var stop bool
	var stopErr error
	var stopReason string
	var arrivalsThrough func(time.Time) error

	pause := func(now time.Time) error {
		if state.rate == 0 {
			return nil
		}
		// No dispatch happens during this accounting. If a safety bound prevents
		// it, preserve the uncreated-arrival marker and fail closed instead.
		dueErr := arrivalsThrough(now)
		cmd := SustainedCommand{Operation: "pause", Stage: state.stage}
		writeErr := journal.write(sustainedRecord{Kind: "stage", TimeUnixNS: now.UnixNano(), Command: &cmd, ArrivalState: state.arrivalRNG.state})
		state.activeElapsed += now.Sub(state.activeFrom)
		state.activeFrom = time.Time{}
		state.rate = 0
		return errors.Join(dueErr, writeErr)
	}
	stopRun := func(reason string, e error) {
		if stop {
			return
		}
		stop, stopReason, stopErr = true, reason, e
		if pe := pause(time.Now()); pe != nil {
			stopErr = errors.Join(stopErr, pe)
			stopReason = "generator_or_journal_failure"
		}
		cancelRequests()
	}
	sample := func(at time.Time, boundary string) error {
		v := state.sample(journal, trace, at)
		v.Boundary = boundary
		if e := journal.write(sustainedRecord{Kind: "sample", TimeUnixNS: v.TimeUnixNS, Sample: &v}); e != nil {
			return e
		}
		if e := journal.flush(); e != nil {
			return e
		}
		if samples != nil {
			select {
			case samples <- v:
			default:
			}
		}
		return nil
	}
	drawInterval := func(rate float64) time.Duration {
		// 53 significant random bits, strictly between zero and one.
		u := (float64(state.arrivalRNG.next()>>11) + 0.5) / (1 << 53)
		ns := -math.Log(u) * float64(time.Second) / rate
		if ns < 1 {
			ns = 1
		}
		return time.Duration(ns)
	}
	applyCommand := func(c SustainedCommand) error {
		now := time.Now()
		switch c.Operation {
		case "stop":
			stopRun("requested_stop", nil)
			return nil
		case "pause":
			return pause(now)
		case "stage", "resume":
			if c.Operation == "resume" {
				if state.rate != 0 || state.stage == "" || state.lastRate <= 0 {
					return errors.New("no paused stage to resume")
				}
				c.Stage, c.Rate = state.stage, state.lastRate
			} else {
				if state.debtRemaining != 0 {
					return errors.New("previous stage's planned debt was not fully created")
				}
				if c.Stage == "" || len(c.Stage) > 80 || math.IsNaN(c.Rate) || math.IsInf(c.Rate, 0) || c.Rate <= 0 || c.Rate > 100000 {
					return errors.New("invalid stage")
				}
			}
			if time.Now().UTC().Format(time.DateOnly) != f.Day {
				return errors.New("UTC day changed; report window must not be fabricated")
			}
			if state.rate > 0 {
				if e := arrivalsThrough(now); e != nil {
					return e
				}
			}
			// Emit the precise cumulative baseline at this same logical
			// boundary. Old-stage arrivals due through now were accounted;
			// new-stage arrivals start at now even if flushing takes time.
			if e := sample(now, "before_stage"); e != nil {
				return e
			}
			if state.rate == 0 {
				state.activeFrom = now
			}
			state.stage, state.rate, state.lastRate = c.Stage, c.Rate, c.Rate
			state.stageStart = now.UnixNano()
			if c.Operation == "stage" {
				state.debtRemaining = int(math.Round(5 * c.Rate))
				state.debtPlanned = now.UnixNano()
			}
			state.nextArrival = now.Add(drawInterval(c.Rate))
			if e := journal.write(sustainedRecord{Kind: "stage", TimeUnixNS: now.UnixNano(), Command: &c, ArrivalState: state.arrivalRNG.state}); e != nil {
				return e
			}
			ids := make([]int, 0, len(state.actors))
			for id := range state.actors {
				ids = append(ids, id)
			}
			sort.Ints(ids)
			for _, id := range ids {
				state.enqueue(state.actors[id], now)
			}
			return nil
		default:
			return errors.New("unknown sustained command")
		}
	}
	create := func(source string, planned int64) error {
		if state.produced-state.confirmed >= int64(o.MaxPending) {
			return errors.New("generator pending limit reached")
		}
		if journal.bytes >= o.MaxJournalBytes {
			return errors.New("generator journal size limit reached")
		}
		if time.Now().UTC().Format(time.DateOnly) != f.Day {
			return errors.New("UTC day changed")
		}
		task, e := state.model.Next()
		if e != nil {
			return e
		}
		now := time.Now()
		state.addTask(task, source, now.UnixNano(), planned)
		if source == "debt" {
			state.debtRemaining--
		}
		r := sustainedRecord{Kind: "created", TimeUnixNS: now.UnixNano(), Task: &task, Source: source, PlannedUnixNS: planned, ArrivalState: state.arrivalRNG.state}
		if e = journal.write(r); e != nil {
			return e
		}
		if source == "arrival" {
			lag := now.Sub(time.Unix(0, planned))
			if lag > state.maxArrivalLag {
				state.maxArrivalLag = lag
			}
			if lag > o.MaxArrivalLag {
				return errors.New("generator arrival lag limit reached")
			}
		}
		state.enqueue(state.actors[task.Client], now)
		return nil
	}
	arrivalsThrough = func(through time.Time) error {
		return sustainedArrivalsThrough(state, through, drawInterval, func(source string, planned int64) error {
			before := state.produced
			e := create(source, planned)
			if e != nil && state.produced == before {
				// This scheduled input cannot be materialized within the local
				// bound. Preserve the boundary and prohibit clean resume: an
				// incomplete input stream is not a capacity observation.
				journal.unusable = true
				_ = journal.write(sustainedRecord{Kind: "uncreated_arrival", TimeUnixNS: time.Now().UnixNano(), PlannedUnixNS: planned, Reason: e.Error()})
			}
			return e
		})
	}
	dispatch := func() error {
		for state.rate > 0 && state.inFlight < o.MaxInFlight && len(state.ready) > 0 {
			id := state.ready[0]
			state.ready = state.ready[1:]
			a := state.actors[id]
			a.enqueued = false
			if a.inFlight || len(a.queue) == 0 || time.Now().Before(a.retryAt) {
				continue
			}
			job := a.queue[0]
			attempt := job.attempts + 1
			if attempt > o.MaxAttempts {
				return errors.New("retry attempt limit reached with retained debt")
			}
			r := sustainedRecord{Kind: "start", TimeUnixNS: time.Now().UnixNano(), Sequence: job.task.Sequence, Client: id, Action: "report", Attempt: attempt}
			if e := journal.write(r); e != nil {
				return e
			}
			job.attempts++
			state.reportAttempts++
			if attempt > 1 {
				state.reportRetries++
			}
			a.inFlight = true
			state.inFlight++
			go func(t SustainedTask, attempt int) {
				responses <- sustainedRequest(requestCtx, client, o.URL, f, t, attempt, o.RequestTimeout, trace)
			}(job.task, attempt)
		}
		return nil
	}
	finish := func(hr sustainedHTTPResult) error {
		r := hr.record
		a := state.actors[r.Client]
		if a == nil || !a.inFlight || len(a.queue) == 0 || a.queue[0].task.Sequence != r.Sequence {
			// A response was consumed even though its bookkeeping is invalid.
			// Stop waiting for it, but never mark this journal clean/resumable.
			journal.unusable = true
			if state.inFlight > 0 {
				state.inFlight--
			}
			return errors.New("unexpected HTTP completion")
		}
		terminal := false
		if !r.Confirmed {
			if hr.retryable && r.Attempt < o.MaxAttempts {
				delay := sustainedBackoff(o.ModelSeed, r.Client, r.Attempt, time.Duration(r.RetryAfterNS))
				r.RetryAtUnixNS = time.Now().Add(delay).UnixNano()
			} else {
				terminal = true
				if r.Error == "" {
					r.Error = "retry_attempt_limit"
				}
			}
		}
		if e := journal.write(r); e != nil {
			journal.unusable = true
			a.inFlight = false
			state.inFlight--
			return e
		}
		if e := state.replayFinish(r); e != nil {
			journal.unusable = true
			a.inFlight = false
			state.inFlight--
			return e
		}
		state.inFlight--
		if r.RetryAtUnixNS > 0 {
			heap.Push(&state.retries, retryEntry{actor: a.id, at: a.retryAt})
		} else {
			state.enqueue(a, time.Now())
		}
		if terminal && !stop {
			return fmt.Errorf("unconfirmed %s task: status=%d code=%s error=%s", r.Action, r.Status, r.ErrorCode, r.Error)
		}
		return nil
	}

	for {
		if stop && state.inFlight == 0 {
			break
		}
		// Always service completions and control before generating more work.
		select {
		case <-ctx.Done():
			stopRun("context_canceled", ctx.Err())
		case hr := <-responses:
			if e := finish(hr); e != nil {
				stopRun("http_or_journal_failure", e)
			}
		case c, ok := <-commands:
			if !ok {
				commands = nil
				stopRun("control_closed", nil)
			} else if e := applyCommand(c); e != nil {
				stopRun("invalid_control", e)
			}
		case <-ticker.C:
			if e := sample(time.Now(), ""); e != nil {
				stopRun("journal_error", e)
			}
		default:
		}
		if stop {
			if state.inFlight > 0 {
				hr := <-responses
				if e := finish(hr); e != nil && stopErr == nil {
					stopErr = e
				}
			}
			continue
		}
		now := time.Now()
		if state.rate > 0 && state.activeElapsed+now.Sub(state.activeFrom) >= o.MaxActive {
			stopRun("active_time_limit", errors.New("active-time budget reached"))
			continue
		}
		for len(state.retries) > 0 && !state.retries[0].at.After(now) {
			entry := heap.Pop(&state.retries).(retryEntry)
			a := state.actors[entry.actor]
			if a != nil && a.retryAt.Equal(entry.at) {
				a.retryAt = time.Time{}
				state.enqueue(a, now)
			}
		}
		if e := dispatch(); e != nil {
			stopRun("generator_or_retry_limit", e)
			continue
		}
		if state.rate > 0 && !now.Before(state.nextArrival) {
			if e := arrivalsThrough(now); e != nil {
				stopRun("generator_limit", e)
			}
			continue
		}
		if state.rate > 0 && state.debtRemaining > 0 {
			if e := create("debt", state.debtPlanned); e != nil {
				stopRun("generator_limit", e)
			}
			continue
		}
		wait := time.Second
		if state.rate > 0 {
			if x := time.Until(state.nextArrival); x < wait {
				wait = x
			}
			if x := o.MaxActive - state.activeElapsed - time.Since(state.activeFrom); x < wait {
				wait = x
			}
		}
		if len(state.retries) > 0 {
			if x := time.Until(state.retries[0].at); x < wait {
				wait = x
			}
		}
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			stopRun("context_canceled", ctx.Err())
		case hr := <-responses:
			if e := finish(hr); e != nil {
				stopRun("http_or_journal_failure", e)
			}
		case c, ok := <-commands:
			if !ok {
				commands = nil
				stopRun("control_closed", nil)
			} else if e := applyCommand(c); e != nil {
				stopRun("invalid_control", e)
			}
		case <-ticker.C:
			if e := sample(time.Now(), ""); e != nil {
				stopRun("journal_error", e)
			}
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	if err = sample(time.Now(), ""); err != nil && stopErr == nil {
		stopErr = err
		stopReason = "journal_error"
	}
	result = SustainedResult{Sample: state.sample(journal, trace, time.Now()), Reason: stopReason}
	if !journal.unusable && journal.failure == nil {
		if err = journal.write(sustainedRecord{Kind: "end", TimeUnixNS: time.Now().UnixNano(), Reason: stopReason}); err != nil {
			stopErr = errors.Join(stopErr, err)
		}
	} else {
		stopErr = errors.Join(stopErr, errors.New("journal incomplete; do not resume without independent recovery"))
	}
	if err = journal.flush(); err != nil {
		stopErr = errors.Join(stopErr, err)
	}
	return result, stopErr
}
