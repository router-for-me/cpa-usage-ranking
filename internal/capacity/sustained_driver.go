package capacity

// An opt-in, local synthetic load driver. It is never imported by the server.
// The journal is private: although it does not contain credentials, it contains
// complete synthetic reports and detailed timing information.

import (
	"bufio"
	"compress/gzip"
	"container/heap"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// SustainedCommand changes load without waiting for outstanding responses.
// "stage" starts an independent Poisson arrival clock and adds a separately
// labelled debt of round(5*Rate); "pause" stops arrivals and dispatch;
// "stop" cancels in-flight calls and exits after logging their outcomes.
type SustainedCommand struct {
	Operation string  `json:"operation"`
	Stage     string  `json:"stage,omitempty"`
	Rate      float64 `json:"rate,omitempty"`
}

type SustainedOptions struct {
	URL, Journal                  string
	Models                        []string
	ModelSeed, ArrivalSeed        uint64
	MaxPending, MaxInFlight       int
	MaxAttempts                   int
	MaxJournalBytes               int64
	RequestTimeout, MaxArrivalLag time.Duration
	MaxActive                     time.Duration
	Resume                        bool
	RetryMigrationPath            string // explicit reviewed C54 manifest; never implicit
	PendingMigrationPath          string // explicit reviewed C54 pending-limit decrease
}

// SustainedSample is cumulative, including work retained across pauses and
// resumes. Pending is always Produced-Confirmed, not a server queue gauge.
type SustainedSample struct {
	TimeUnixNS         int64            `json:"time_unix_ns"`
	Stage              string           `json:"stage"`
	StageStartUnixNS   int64            `json:"stage_start_unix_ns,omitempty"`
	Boundary           string           `json:"boundary,omitempty"`
	TargetRate         float64          `json:"target_rate"`
	Produced           int64            `json:"produced"`
	ArrivalProduced    int64            `json:"arrival_produced"`
	DebtProduced       int64            `json:"debt_produced"`
	Confirmed          int64            `json:"confirmed"`
	Pending            int64            `json:"pending"`
	OldestPendingMS    float64          `json:"oldest_pending_ms"`
	ReportAttempts     int64            `json:"report_attempts"`
	ReportRetries      int64            `json:"report_retries"`
	InFlight           int              `json:"in_flight"`
	WrittenUnfinished  int64            `json:"written_unfinished"`
	WrittenPeak        int64            `json:"written_peak"`
	LateTraceCallbacks int64            `json:"late_trace_callbacks"`
	ActiveSeconds      float64          `json:"active_seconds"`
	ReadyActors        int              `json:"ready_actors"`
	RetryingActors     int              `json:"retrying_actors"`
	DebtRemaining      int              `json:"debt_remaining"`
	MaxArrivalLagMS    float64          `json:"max_arrival_lag_ms"`
	MaxJournalLagMS    float64          `json:"max_journal_lag_ms"`
	JournalBytes       int64            `json:"journal_bytes"`
	ReportStatuses     map[string]int64 `json:"report_statuses"`
	ReportErrorCodes   map[string]int64 `json:"report_error_codes"`
}

type sustainedHeader struct {
	Version, Day, URL, CatalogHash, CredentialSeedHash string
	Stock                                              int
	ModelSeed, ArrivalSeed                             uint64
	MaxPending, MaxInFlight, MaxAttempts               int
	MaxJournalBytes                                    int64
	RequestTimeoutNS, MaxArrivalLagNS, MaxActiveNS     int64
}

type sustainedRecord struct {
	Kind             string                     `json:"kind"`
	TimeUnixNS       int64                      `json:"time_unix_ns"`
	Header           *sustainedHeader           `json:"header,omitempty"`
	RetryMigration   *sustainedRetryMigration   `json:"retry_migration,omitempty"`
	PendingMigration *sustainedPendingMigration `json:"pending_migration,omitempty"`
	Command          *SustainedCommand          `json:"command,omitempty"`
	Task             *SustainedTask             `json:"task,omitempty"`
	Source           string                     `json:"source,omitempty"`
	PlannedUnixNS    int64                      `json:"planned_unix_ns,omitempty"`
	ArrivalState     uint64                     `json:"arrival_state,omitempty"`
	Sequence         int64                      `json:"sequence,omitempty"`
	Client           int                        `json:"client,omitempty"`
	Action           string                     `json:"action,omitempty"`
	Attempt          int                        `json:"attempt,omitempty"`
	Status           int                        `json:"status,omitempty"`
	Confirmed        bool                       `json:"confirmed,omitempty"`
	AckRevision      string                     `json:"ack_revision,omitempty"`
	Error            string                     `json:"error,omitempty"`
	ErrorCode        string                     `json:"error_code,omitempty"`
	RetryAfterNS     int64                      `json:"retry_after_ns,omitempty"`
	RetryAtUnixNS    int64                      `json:"retry_at_unix_ns,omitempty"`
	GotConnUnixNS    int64                      `json:"got_conn_unix_ns,omitempty"`
	WroteUnixNS      int64                      `json:"wrote_unix_ns,omitempty"`
	Sample           *SustainedSample           `json:"sample,omitempty"`
	Reason           string                     `json:"reason,omitempty"`
}

// gzip journals are concatenated members, one per clean driver invocation.
// The gzip trailer detects a truncated member; the JSONL end marker still
// determines whether an invocation closed cleanly. Plain JSONL stays readable.
type sustainedByteCounter struct {
	out io.Writer
	n   int64
}

func (w *sustainedByteCounter) Write(p []byte) (int, error) {
	n, err := w.out.Write(p)
	w.n += int64(n)
	return n, err
}

func sustainedJournalReader(file *os.File) (io.ReadCloser, bool, error) {
	br := bufio.NewReaderSize(file, 256*1024)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return nil, false, err
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		return gz, true, err
	}
	return io.NopCloser(br), false, nil
}

func readSustainedLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > 2<<20 {
			return nil, errors.New("journal record exceeds 2 MiB")
		}
		line = append(line, part...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

type sustainedJournal struct {
	file     *os.File
	out      *bufio.Writer
	counter  *sustainedByteCounter
	gzip     *gzip.Writer
	bytes    int64
	lag      time.Duration
	failure  error
	unusable bool
}

func (j *sustainedJournal) write(r sustainedRecord) error {
	if j.failure != nil {
		return j.failure
	}
	start := time.Now()
	data, err := json.Marshal(r)
	if err == nil {
		data = append(data, '\n')
		_, err = j.out.Write(data)
		if j.counter != nil {
			j.bytes = j.counter.n
		} else {
			j.bytes += int64(len(data))
		}
	}
	if lag := time.Since(start); lag > j.lag {
		j.lag = lag
	}
	j.failure = err
	return err
}

func (j *sustainedJournal) flush() error {
	if j.failure != nil {
		return j.failure
	}
	start := time.Now()
	err := j.out.Flush()
	if err == nil && j.gzip != nil {
		err = j.gzip.Flush()
	}
	if j.counter != nil {
		j.bytes = j.counter.n
	}
	if lag := time.Since(start); lag > j.lag {
		j.lag = lag
	}
	j.failure = err
	return err
}

func (j *sustainedJournal) close() error {
	err := j.flush()
	if j.gzip != nil {
		err = errors.Join(err, j.gzip.Close())
		j.gzip = nil
	}
	if j.counter != nil {
		j.bytes = j.counter.n
	}
	return err
}

type sustainedJob struct {
	task             SustainedTask
	created, planned int64
	source           string
	attempts         int
	done             bool
	heapIndex        int
}

type sustainedActor struct {
	id                 int
	queue              []*sustainedJob
	inFlight, enqueued bool
	retryAt            time.Time
}

type oldestJobs []*sustainedJob

func (h oldestJobs) Len() int           { return len(h) }
func (h oldestJobs) Less(i, j int) bool { return h[i].planned < h[j].planned }
func (h oldestJobs) Swap(i, j int)      { h[i], h[j] = h[j], h[i]; h[i].heapIndex = i; h[j].heapIndex = j }
func (h *oldestJobs) Push(v any)        { x := v.(*sustainedJob); x.heapIndex = len(*h); *h = append(*h, x) }
func (h *oldestJobs) Pop() any {
	old := *h
	x := old[len(old)-1]
	x.heapIndex = -1
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return x
}

type retryEntry struct {
	actor int
	at    time.Time
}
type retryHeap []retryEntry

func (h retryHeap) Len() int           { return len(h) }
func (h retryHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h retryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *retryHeap) Push(v any)        { *h = append(*h, v.(retryEntry)) }
func (h *retryHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type sustainedState struct {
	model                               *SustainedModel
	actors                              map[int]*sustainedActor
	oldest                              oldestJobs
	retries                             retryHeap
	ready                               []int
	produced, arrivals, debt, confirmed int64
	reportAttempts, reportRetries       int64
	reportStatus, reportCodes           map[string]int64
	inFlight                            int
	stage                               string
	stageStart                          int64
	rate, lastRate                      float64
	activeFrom                          time.Time
	activeElapsed                       time.Duration
	debtRemaining                       int
	debtPlanned                         int64
	nextArrival                         time.Time
	arrivalRNG                          splitMix
	maxArrivalLag                       time.Duration
}

func newSustainedState(model *SustainedModel, seed uint64) *sustainedState {
	return &sustainedState{model: model, actors: map[int]*sustainedActor{}, reportStatus: map[string]int64{}, reportCodes: map[string]int64{}, arrivalRNG: splitMix{state: seed}}
}

func (s *sustainedState) actor(id int) *sustainedActor {
	if a := s.actors[id]; a != nil {
		return a
	}
	a := &sustainedActor{id: id}
	s.actors[id] = a
	return a
}

func (s *sustainedState) enqueue(a *sustainedActor, now time.Time) {
	if len(a.queue) == 0 || a.inFlight || a.enqueued || now.Before(a.retryAt) {
		return
	}
	a.enqueued = true
	s.ready = append(s.ready, a.id)
}

func (s *sustainedState) addTask(t SustainedTask, source string, created, planned int64) {
	a := s.actor(t.Client)
	j := &sustainedJob{task: t, source: source, created: created, planned: planned}
	a.queue = append(a.queue, j)
	heap.Push(&s.oldest, j)
	s.produced++
	if source == "arrival" {
		s.arrivals++
	} else {
		s.debt++
	}
}

func (s *sustainedState) sample(j *sustainedJournal, trace *sustainedTraceCounts, now time.Time) SustainedSample {
	for len(s.oldest) > 0 && s.oldest[0].done {
		heap.Pop(&s.oldest)
	}
	var age float64
	if len(s.oldest) > 0 {
		age = float64(now.UnixNano()-s.oldest[0].planned) / float64(time.Millisecond)
		if age < 0 {
			age = 0
		}
	}
	retrying := 0
	for _, a := range s.actors {
		if len(a.queue) > 0 && now.Before(a.retryAt) {
			retrying++
		}
	}
	copyMap := func(in map[string]int64) map[string]int64 {
		out := make(map[string]int64, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	active := s.activeElapsed
	if !s.activeFrom.IsZero() {
		active += now.Sub(s.activeFrom)
	}
	return SustainedSample{TimeUnixNS: now.UnixNano(), Stage: s.stage, StageStartUnixNS: s.stageStart, TargetRate: s.rate, Produced: s.produced, ArrivalProduced: s.arrivals, DebtProduced: s.debt, Confirmed: s.confirmed, Pending: s.produced - s.confirmed, OldestPendingMS: age, ReportAttempts: s.reportAttempts, ReportRetries: s.reportRetries, InFlight: s.inFlight, WrittenUnfinished: trace.written.Load(), WrittenPeak: trace.peak.Load(), LateTraceCallbacks: trace.late.Load(), ActiveSeconds: active.Seconds(), ReadyActors: len(s.ready), RetryingActors: retrying, DebtRemaining: s.debtRemaining, MaxArrivalLagMS: float64(s.maxArrivalLag) / float64(time.Millisecond), MaxJournalLagMS: float64(j.lag) / float64(time.Millisecond), JournalBytes: j.bytes, ReportStatuses: copyMap(s.reportStatus), ReportErrorCodes: copyMap(s.reportCodes)}
}

func sustainedHeaderFor(f Fixture, o SustainedOptions) sustainedHeader {
	catalog, _ := json.Marshal(o.Models)
	catalogHash := sha256.Sum256(catalog)
	credentialHash := sha256.Sum256([]byte(f.Seed))
	return sustainedHeader{Version: SustainedModelVersion, Day: f.Day, URL: o.URL, CatalogHash: hex.EncodeToString(catalogHash[:]), CredentialSeedHash: hex.EncodeToString(credentialHash[:]), Stock: f.Clients, ModelSeed: o.ModelSeed, ArrivalSeed: o.ArrivalSeed, MaxPending: o.MaxPending, MaxInFlight: o.MaxInFlight, MaxAttempts: o.MaxAttempts, MaxJournalBytes: o.MaxJournalBytes, RequestTimeoutNS: int64(o.RequestTimeout), MaxArrivalLagNS: int64(o.MaxArrivalLag), MaxActiveNS: int64(o.MaxActive)}
}

func validateSustainedOptions(f Fixture, o SustainedOptions) error {
	if err := f.Validate(); err != nil {
		return err
	}
	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("sustained URL must be a loopback HTTP origin")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if (!strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback())) || u.Port() == "" {
		return errors.New("sustained URL must use localhost or a numeric loopback address and port")
	}
	if o.Journal == "" || o.MaxPending < 1 || o.MaxPending > 1000000 || o.MaxInFlight < 1 || o.MaxInFlight > 20000 || o.MaxAttempts < 1 || o.MaxAttempts > 512 || o.MaxJournalBytes < 1<<20 || o.RequestTimeout < 10*time.Millisecond || o.RequestTimeout > 2*time.Minute || o.MaxArrivalLag < 10*time.Millisecond || o.MaxArrivalLag > time.Minute || o.MaxActive <= 0 || o.MaxActive > time.Hour {
		return errors.New("invalid sustained local safety bounds")
	}
	nowUTC := time.Now().UTC()
	today := nowUTC.Format(time.DateOnly)
	yesterday := nowUTC.AddDate(0, 0, -1).Format(time.DateOnly)
	if f.Day != today && f.Day != yesterday {
		return errors.New("fixture day must be UTC today or yesterday")
	}
	return nil
}

// restoreSustained replays the exact sequence and validates every immutable
// snapshot before reconstructing outstanding work. A trailing partial record is
// an error, never silently truncated. Starts without finishes remain ambiguous
// and are retried with the same task after resume.
func restoreSustained(file *os.File, header sustainedHeader, state *sustainedState, compressed bool, migration *sustainedRetryMigration, pendingMigration *sustainedPendingMigration, needsMigration, needsPendingMigration *bool) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	source, isCompressed, err := sustainedJournalReader(file)
	if err != nil {
		return err
	}
	defer source.Close()
	if isCompressed != compressed {
		return errors.New("journal format does not match path; refusing to append")
	}
	reader := bufio.NewReaderSize(source, 256*1024)
	lineNo, endCount, attemptLimit := 0, 0, 0
	var activeSince int64
	lastKind := ""
	migrated, pendingMigrated := false, false
	closeActive := func(at int64) error {
		if activeSince != 0 {
			if at < activeSince {
				return errors.New("journal clock moved backwards")
			}
			state.activeElapsed += time.Duration(at - activeSince)
			activeSince = 0
		}
		return nil
	}
	for {
		line, err := readSustainedLine(reader)
		if len(line) == 0 && err == io.EOF {
			break
		}
		lineNo++
		if err != nil || len(line) > 2<<20 {
			return fmt.Errorf("incomplete or oversized journal record at line %d", lineNo)
		}
		var r sustainedRecord
		if json.Unmarshal(line, &r) != nil {
			return fmt.Errorf("invalid journal record at line %d", lineNo)
		}
		previousKind := lastKind
		lastKind = r.Kind
		if lineNo == 1 {
			expected := header
			if migration != nil {
				targetPending := migration.NextPending
				if pendingMigration != nil {
					if pendingMigration.PreviousPending != targetPending || pendingMigration.MaxAttempts != migration.NextAttempts ||
						pendingMigration.PreviousConfigHash != migration.NextConfigHash || pendingMigration.PreviousBinarySHA != migration.NextBinarySHA {
						return errors.New("pending migration does not follow retry migration")
					}
					targetPending = pendingMigration.NextPending
				}
				if header.MaxAttempts != migration.NextAttempts || header.MaxPending != targetPending {
					return errors.New("migration target differs from runtime configuration")
				}
				expected.MaxAttempts, expected.MaxPending = migration.PreviousAttempts, migration.PreviousPending
			} else if pendingMigration != nil {
				return errors.New("pending migration requires the approved retry migration")
			}
			if r.Kind != "header" || r.Header == nil || !reflect.DeepEqual(*r.Header, expected) {
				return errors.New("journal configuration mismatch")
			}
			attemptLimit = r.Header.MaxAttempts
			continue
		}
		switch r.Kind {
		case "created":
			if r.Task == nil || (r.Source != "arrival" && r.Source != "debt") {
				return fmt.Errorf("bad task at line %d", lineNo)
			}
			want, e := state.model.Next()
			if e != nil || !reflect.DeepEqual(want, *r.Task) {
				return fmt.Errorf("non-reproducible task at line %d", lineNo)
			}
			state.addTask(*r.Task, r.Source, r.TimeUnixNS, r.PlannedUnixNS)
			if r.Source == "debt" {
				state.debtRemaining--
				if state.debtRemaining < 0 {
					return fmt.Errorf("unplanned debt at line %d", lineNo)
				}
			}
			state.arrivalRNG.state = r.ArrivalState
		case "start":
			if r.Attempt < 1 || r.Attempt > attemptLimit {
				return fmt.Errorf("attempt limit exceeded at line %d", lineNo)
			}
			a := state.actors[r.Client]
			if a == nil || len(a.queue) == 0 || a.queue[0].task.Sequence != r.Sequence || a.inFlight ||
				(!a.retryAt.IsZero() && r.TimeUnixNS < a.retryAt.UnixNano()) {
				return fmt.Errorf("bad or premature start at line %d", lineNo)
			}
			if r.Action != "report" || r.Attempt != a.queue[0].attempts+1 {
				return fmt.Errorf("bad attempt at line %d", lineNo)
			}
			a.queue[0].attempts++
			state.reportAttempts++
			if r.Attempt > 1 {
				state.reportRetries++
			}
			a.inFlight = true
		case "finish":
			if err := state.replayFinish(r); err != nil {
				return fmt.Errorf("line %d: %w", lineNo, err)
			}
		case "stage":
			if r.Command == nil {
				return fmt.Errorf("missing command at line %d", lineNo)
			}
			switch r.Command.Operation {
			case "stage":
				if state.debtRemaining != 0 {
					return fmt.Errorf("unfinished stage debt at line %d", lineNo)
				}
				state.stage, state.rate, state.lastRate = r.Command.Stage, r.Command.Rate, r.Command.Rate
				state.stageStart = r.TimeUnixNS
				state.debtRemaining = int(math.Round(5 * r.Command.Rate))
				state.debtPlanned = r.TimeUnixNS
				if activeSince == 0 {
					activeSince = r.TimeUnixNS
				}
			case "pause":
				state.rate = 0
				if err := closeActive(r.TimeUnixNS); err != nil {
					return err
				}
			case "resume":
				state.stage, state.rate, state.lastRate = r.Command.Stage, r.Command.Rate, r.Command.Rate
				state.stageStart = r.TimeUnixNS
				if activeSince == 0 {
					activeSince = r.TimeUnixNS
				}
			default:
				return fmt.Errorf("bad stage command at line %d", lineNo)
			}
			state.arrivalRNG.state = r.ArrivalState
		case "recover":
			a := state.actors[r.Client]
			if a == nil || !a.inFlight || len(a.queue) == 0 || a.queue[0].task.Sequence != r.Sequence {
				return fmt.Errorf("bad recovery at line %d", lineNo)
			}
			a.inFlight = false
		case "end":
			endCount++
			if err := closeActive(r.TimeUnixNS); err != nil {
				return err
			}
		case "retry_migration":
			if migration == nil || migrated || previousKind != "end" || endCount != 2 || state.rate != 0 || activeSince != 0 ||
				r.RetryMigration == nil || *r.RetryMigration != *migration {
				return fmt.Errorf("unapproved retry migration at line %d", lineNo)
			}
			if err := migration.validate(file); err != nil {
				return err
			}
			for _, a := range state.actors {
				if a.inFlight {
					return errors.New("retry migration with in-flight attempts")
				}
			}
			if err := verifyReviewedRetryTask(migration, state); err != nil {
				return err
			}
			migrated, attemptLimit = true, migration.NextAttempts
		case "pending_migration":
			if pendingMigration == nil || pendingMigrated || !migrated || previousKind != "end" || endCount != 3 ||
				state.rate != 0 || activeSince != 0 || r.PendingMigration == nil || *r.PendingMigration != *pendingMigration {
				return fmt.Errorf("unapproved pending migration at line %d", lineNo)
			}
			if err := pendingMigration.validate(file); err != nil {
				return err
			}
			if state.produced != pendingMigration.Produced || state.confirmed != pendingMigration.Confirmed ||
				state.produced-state.confirmed != pendingMigration.Pending {
				return errors.New("pending migration totals differ")
			}
			for _, a := range state.actors {
				if a.inFlight {
					return errors.New("pending migration with in-flight attempts")
				}
			}
			pendingMigrated = true
		case "sample":
		default:
			return fmt.Errorf("unknown journal kind at line %d", lineNo)
		}
	}
	if lineNo == 0 {
		return errors.New("empty journal")
	}
	if lastKind != "end" || activeSince != 0 {
		return errors.New("journal was not closed cleanly; active-time budget is indeterminate")
	}
	if migration != nil && !migrated {
		info, err := file.Stat()
		if err != nil || info.Size() != migration.PreviousBytes || endCount != 2 || state.rate != 0 {
			return errors.New("reviewed retry migration is not at the audited closed boundary")
		}
		for _, a := range state.actors {
			if a.inFlight {
				return errors.New("reviewed retry migration has in-flight attempts")
			}
		}
		if err := verifyReviewedRetryTask(migration, state); err != nil {
			return err
		}
		*needsMigration = true
	}
	if pendingMigration != nil && !pendingMigrated {
		info, err := file.Stat()
		if err != nil || info.Size() != pendingMigration.PreviousBytes || endCount != 3 || !migrated || state.rate != 0 ||
			state.produced != pendingMigration.Produced || state.confirmed != pendingMigration.Confirmed ||
			state.produced-state.confirmed != pendingMigration.Pending {
			return errors.New("pending migration is not at the verified closed boundary")
		}
		for _, a := range state.actors {
			if a.inFlight {
				return errors.New("pending migration has in-flight attempts")
			}
		}
		*needsPendingMigration = true
	}
	for _, a := range state.actors {
		if len(a.queue) > 0 && !a.retryAt.IsZero() {
			heap.Push(&state.retries, retryEntry{actor: a.id, at: a.retryAt})
		}
	}
	// Resume always starts paused. Any unfinished starts are explicitly
	// journaled as ambiguous recoveries by the caller before dispatch resumes.
	state.rate = 0
	_, err = file.Seek(0, io.SeekEnd)
	return err
}

func (s *sustainedState) replayFinish(r sustainedRecord) error {
	a := s.actors[r.Client]
	if a == nil || !a.inFlight || len(a.queue) == 0 || a.queue[0].task.Sequence != r.Sequence {
		return errors.New("finish has no matching start")
	}
	if r.Action != "report" || r.Attempt != a.queue[0].attempts {
		return errors.New("mismatched report finish")
	}
	s.reportStatus[strconv.Itoa(r.Status)]++
	if r.ErrorCode != "" {
		s.reportCodes[r.ErrorCode]++
	}
	if r.Confirmed {
		if r.Status != 200 || r.AckRevision != strconv.FormatInt(a.queue[0].task.Revision, 10) {
			return errors.New("invalid report confirmation")
		}
		a.queue[0].done = true
		heap.Remove(&s.oldest, a.queue[0].heapIndex)
		a.queue[0] = nil
		a.queue = a.queue[1:]
		s.confirmed++
	}

	a.inFlight = false
	if r.RetryAtUnixNS > 0 {
		a.retryAt = time.Unix(0, r.RetryAtUnixNS)
	} else {
		a.retryAt = time.Time{}
	}
	return nil
}
