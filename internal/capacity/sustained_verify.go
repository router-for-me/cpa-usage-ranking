package capacity

// Independent reconstruction for sustained runs. It does not call
// SustainedModel.Next or the production aggregation function: journal deltas
// are summed independently and checked against every immutable snapshot.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strconv"
	"time"

	"cpa-usage-ranking/internal/reports"
)

type SustainedVerification struct {
	Produced               int64  `json:"produced"`
	Confirmed              int64  `json:"confirmed"`
	Pending                int64  `json:"pending"`
	UnconfirmedCommits     int64  `json:"unconfirmed_commits"`
	CheckedClients         int64  `json:"checked_clients"`
	CheckedDays            int64  `json:"checked_days"`
	CheckedDailyRows       int64  `json:"checked_daily_rows"`
	CheckedPeriodRows      int64  `json:"checked_period_rows"`
	ClosedSourcesRemaining int64  `json:"closed_sources_remaining"`
	ExpectedDB             string `json:"expected_db"`
}

type verificationKey struct {
	client int
	day    string
}
type verificationDay struct {
	revision int64
	rows     map[reports.Key]reports.Metrics
}
type verificationJob struct {
	task      SustainedTask
	attempts  int
	ambiguous bool
}
type verificationActor struct {
	queue              []*verificationJob
	inFlight           bool
	action             string
	attempt            int
	started, notBefore int64
	class              string
	limit              int
	hot                bool
	seenTraits         bool
}

type verifySnapshot struct {
	body     []byte
	digest   [32]byte
	revision int64
}

type verificationState struct {
	fixture                            Fixture
	modelIDs                           map[string]int64
	days                               map[verificationKey]*verificationDay
	actors                             map[int]*verificationActor
	acks                               map[verificationKey]verifySnapshot
	created, arrivals, debt, confirmed int64
	stage                              string
	rate                               float64
	debtRemaining                      int
	activeSince                        int64
	activeElapsed                      time.Duration
	sequence                           int64
	maxAttempts                        int
	reportAttempts, reportRetries      int64
	reportStatus, reportCodes          map[string]int64
}

func verificationAdd(a, b reports.Metrics) (reports.Metrics, error) {
	for _, pair := range []struct {
		dst *int64
		v   int64
	}{
		{&a.TotalTokens, b.TotalTokens}, {&a.InputTokens, b.InputTokens}, {&a.OutputTokens, b.OutputTokens},
		{&a.CacheReadTokens, b.CacheReadTokens}, {&a.CacheWriteTokens, b.CacheWriteTokens}, {&a.ReasoningTokens, b.ReasoningTokens},
		{&a.RequestCount, b.RequestCount}, {&a.SuccessCount, b.SuccessCount}, {&a.TTFTSamples, b.TTFTSamples}, {&a.LatencySamples, b.LatencySamples},
	} {
		if pair.v < 0 || *pair.dst < 0 || *pair.dst > math.MaxInt64-pair.v {
			return a, errors.New("metric overflow or negative increment")
		}
		*pair.dst += pair.v
	}
	a.TTFTSumMS += b.TTFTSumMS
	a.LatencySumMS += b.LatencySumMS
	if a.TTFTSumMS < 0 || a.LatencySumMS < 0 || math.IsInf(a.TTFTSumMS, 0) || math.IsInf(a.LatencySumMS, 0) || math.IsNaN(a.TTFTSumMS) || math.IsNaN(a.LatencySumMS) {
		return a, errors.New("invalid duration sum")
	}
	return a, nil
}

func snapshotRows(body []byte, ids map[string]int64) (verifySnapshot, map[reports.Key]reports.Metrics, error) {
	var out verifySnapshot
	input, err := reports.DecodeLimited(body, 128)
	if err != nil {
		return out, nil, err
	}
	prepared, err := reports.Prepare(input)
	if err != nil {
		return out, nil, err
	}
	out = verifySnapshot{body: bytes.Clone(body), revision: input.Revision, digest: prepared.Digest()}
	rows := map[reports.Key]reports.Metrics{}
	for _, v := range input.Items {
		id := ids[v.Model]
		if id == 0 || v.RequestCount == 0 {
			return out, nil, errors.New("unknown or empty synthetic model row")
		}
		k := reports.Key{ModelID: id}
		if _, ok := rows[k]; ok {
			return out, nil, errors.New("duplicate synthetic model")
		}
		m := reports.Metrics{TotalTokens: v.TotalTokens, InputTokens: v.InputTokens, OutputTokens: v.OutputTokens, CacheReadTokens: v.CacheReadTokens, CacheWriteTokens: v.CacheWriteTokens, ReasoningTokens: v.ReasoningTokens, RequestCount: v.RequestCount, SuccessCount: v.SuccessCount, TTFTSamples: v.TTFTSamples, LatencySamples: v.LatencySamples}
		if v.TTFTAvgMS != nil {
			m.TTFTSumMS = *v.TTFTAvgMS * float64(v.TTFTSamples)
		}
		if v.LatencyAvgMS != nil {
			m.LatencySumMS = *v.LatencyAvgMS * float64(v.LatencySamples)
		}
		rows[k] = m
	}
	return out, rows, nil
}

func newVerificationState(ctx context.Context, fixture Fixture, seed *sql.DB) (*verificationState, error) {
	s := &verificationState{fixture: fixture, modelIDs: map[string]int64{}, days: map[verificationKey]*verificationDay{}, actors: map[int]*verificationActor{}, acks: map[verificationKey]verifySnapshot{}, maxAttempts: 30, reportStatus: map[string]int64{}, reportCodes: map[string]int64{}}
	rows, err := seed.QueryContext(ctx, "SELECT id,model_name FROM models")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var name string
		if err = rows.Scan(&id, &name); err != nil {
			rows.Close()
			return nil, err
		}
		s.modelIDs[name] = id
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	day, _ := time.Parse(time.DateOnly, fixture.Day)
	yesterday := day.AddDate(0, 0, -1).Format(time.DateOnly)
	rows, err = seed.QueryContext(ctx, "SELECT day,client_id,revision FROM daily_writer_versions WHERE day IN (?,?)", fixture.Day, yesterday)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k verificationKey
		var rev int64
		if err = rows.Scan(&k.day, &k.client, &rev); err != nil {
			rows.Close()
			return nil, err
		}
		if k.client < 1 || k.client > fixture.Clients || rev != 1 {
			rows.Close()
			return nil, errors.New("invalid stock seed revision")
		}
		s.days[k] = &verificationDay{revision: rev, rows: map[reports.Key]reports.Metrics{}}
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(s.days) != 2*fixture.Clients {
		return nil, errors.New("stock seed does not have both open days")
	}
	rows, err = seed.QueryContext(ctx, "SELECT day,client_id,model_id,"+metricColumns+" FROM daily_stats WHERE day IN (?,?) AND model_id>0", fixture.Day, yesterday)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k verificationKey
		var key reports.Key
		var v reports.Metrics
		if err = rows.Scan(&k.day, &k.client, &key.ModelID, &v.TotalTokens, &v.InputTokens, &v.OutputTokens, &v.CacheReadTokens, &v.CacheWriteTokens, &v.ReasoningTokens, &v.RequestCount, &v.SuccessCount, &v.TTFTSumMS, &v.TTFTSamples, &v.LatencySumMS, &v.LatencySamples); err != nil {
			rows.Close()
			return nil, err
		}
		if s.days[k] == nil {
			rows.Close()
			return nil, errors.New("orphaned seed detail")
		}
		s.days[k].rows[key] = v
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return s, nil
}

func (s *verificationState) actor(id int) *verificationActor {
	if a := s.actors[id]; a != nil {
		return a
	}
	a := &verificationActor{}
	s.actors[id] = a
	return a
}

func (s *verificationState) createdTask(r sustainedRecord) error {
	t := r.Task
	if t == nil || (r.Source != "arrival" && r.Source != "debt") || t.Sequence != s.sequence+1 || r.PlannedUnixNS == 0 || r.TimeUnixNS == 0 {
		return errors.New("invalid task creation")
	}
	if t.Client < 1 || t.Client > s.fixture.Clients {
		return errors.New("task refers to unknown synthetic identity")
	}
	a := s.actor(t.Client)
	if !a.seenTraits {
		a.seenTraits = true
		a.hot = t.Hot
		a.class = t.Class
		a.limit = t.Limit
	} else if a.hot != t.Hot || a.class != t.Class || a.limit != t.Limit {
		return errors.New("client traits changed")
	}
	if !(t.Class == "light" && t.Limit >= 1 && t.Limit <= 4 || t.Class == "medium" && t.Limit >= 5 && t.Limit <= 16 || t.Class == "heavy" && t.Limit >= 17 && t.Limit <= 48) {
		return errors.New("invalid client model limit")
	}
	day, _ := time.Parse(time.DateOnly, s.fixture.Day)
	if t.Day != s.fixture.Day && t.Day != day.AddDate(0, 0, -1).Format(time.DateOnly) {
		return errors.New("task is outside the open days")
	}
	key := verificationKey{client: t.Client, day: t.Day}
	state := s.days[key]
	if state == nil {
		state = &verificationDay{rows: map[reports.Key]reports.Metrics{}}
		s.days[key] = state
	}
	if t.Revision != state.revision+1 || len(t.Deltas) < 1 || len(t.Deltas) > 4 {
		return errors.New("invalid task revision or delta count")
	}
	hash := sha256.Sum256(t.Body)
	if t.BodyHash != hex.EncodeToString(hash[:]) {
		return errors.New("snapshot byte hash mismatch")
	}
	snapshot, actual, err := snapshotRows(t.Body, s.modelIDs)
	if err != nil {
		return err
	}
	if snapshot.revision != t.Revision || len(actual) > t.Limit {
		return errors.New("snapshot revision or size mismatch")
	}
	seen := map[reports.Key]bool{}
	for _, d := range t.Deltas {
		id := s.modelIDs[d.Model]
		if id == 0 {
			return errors.New("invalid delta model")
		}
		k := reports.Key{ModelID: id}
		if seen[k] {
			return errors.New("duplicate delta")
		}
		seen[k] = true
		if d.RequestCount < 1 || d.SuccessCount < 0 || d.SuccessCount > d.RequestCount || d.TTFTSamples < 0 || d.TTFTSamples > d.RequestCount || d.LatencySamples < 0 || d.LatencySamples > d.RequestCount || d.TTFTAvgMS < 0 || d.LatencyAvgMS < 0 || d.InputTokens < 0 || d.OutputTokens < 0 || d.CacheReadTokens < 0 || d.CacheWriteTokens < 0 || d.ReasoningTokens < 0 || d.TotalTokens < 0 || d.CacheReadTokens > d.InputTokens-d.CacheWriteTokens || d.ReasoningTokens > d.OutputTokens || d.InputTokens > d.TotalTokens-d.OutputTokens {
			return errors.New("invalid delta invariants")
		}
		v := reports.Metrics{TotalTokens: d.TotalTokens, InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, CacheReadTokens: d.CacheReadTokens, CacheWriteTokens: d.CacheWriteTokens, ReasoningTokens: d.ReasoningTokens, RequestCount: d.RequestCount, SuccessCount: d.SuccessCount, TTFTSumMS: float64(d.TTFTAvgMS) * float64(d.TTFTSamples), TTFTSamples: d.TTFTSamples, LatencySumMS: float64(d.LatencyAvgMS) * float64(d.LatencySamples), LatencySamples: d.LatencySamples}
		combined, e := verificationAdd(state.rows[k], v)
		if e != nil {
			return e
		}
		state.rows[k] = combined
	}
	if !reflect.DeepEqual(state.rows, actual) {
		return errors.New("snapshot does not equal independently accumulated deltas")
	}
	state.revision = t.Revision
	a.queue = append(a.queue, &verificationJob{task: *t})
	if s.rate <= 0 {
		return errors.New("task created while paused")
	}
	if r.Source == "debt" {
		if s.debtRemaining <= 0 {
			return errors.New("unplanned debt task")
		}
		s.debtRemaining--
		s.debt++
	} else {
		s.arrivals++
	}
	s.sequence++
	s.created++
	return nil
}

func ambiguousStatus(r sustainedRecord) bool {
	return r.Status == 0 || r.Status >= 500 || (r.Status >= 200 && r.Status < 300 && !r.Confirmed)
}

func (s *verificationState) start(r sustainedRecord) error {
	a := s.actors[r.Client]
	if a == nil || a.inFlight || len(a.queue) == 0 || a.queue[0].task.Sequence != r.Sequence || r.TimeUnixNS < a.notBefore {
		return errors.New("invalid or premature attempt start")
	}
	if r.Action != "report" || r.Attempt != a.queue[0].attempts+1 {
		return errors.New("attempt action/order mismatch")
	}
	a.queue[0].attempts++
	s.reportAttempts++
	if r.Attempt > 1 {
		s.reportRetries++
	}
	if r.Attempt < 1 || r.Attempt > s.maxAttempts {
		return errors.New("attempt limit exceeded")
	}
	a.inFlight = true
	a.action = r.Action
	a.attempt = r.Attempt
	a.started = r.TimeUnixNS
	a.notBefore = 0
	return nil
}

func (s *verificationState) finish(r sustainedRecord) error {
	a := s.actors[r.Client]
	if a == nil || !a.inFlight || len(a.queue) == 0 || a.queue[0].task.Sequence != r.Sequence || a.action != r.Action || a.attempt != r.Attempt || r.TimeUnixNS < a.started {
		return errors.New("completion does not match its start")
	}
	if r.RetryAtUnixNS != 0 && r.RetryAtUnixNS-r.TimeUnixNS < r.RetryAfterNS {
		return errors.New("Retry-After was not honored")
	}
	if r.Confirmed && r.RetryAtUnixNS != 0 {
		return errors.New("confirmed attempt scheduled a retry")
	}
	s.reportStatus[strconv.Itoa(r.Status)]++
	if r.ErrorCode != "" {
		s.reportCodes[r.ErrorCode]++
	}
	job := a.queue[0]
	if r.Confirmed {
		if r.Status != 200 || r.AckRevision != strconv.FormatInt(job.task.Revision, 10) {
			return errors.New("invalid report confirmation")
		}
		snapshot, _, err := snapshotRows(job.task.Body, s.modelIDs)
		if err != nil {
			return err
		}
		s.acks[verificationKey{client: r.Client, day: job.task.Day}] = snapshot
		a.queue[0] = nil
		a.queue = a.queue[1:]
		s.confirmed++
	} else if ambiguousStatus(r) {
		job.ambiguous = true
	}
	a.inFlight = false
	a.notBefore = r.RetryAtUnixNS
	return nil
}

func (s *verificationState) verifySample(v *SustainedSample) error {
	if v == nil || v.Produced != s.created || v.ArrivalProduced != s.arrivals || v.DebtProduced != s.debt || v.Stage != s.stage || v.TargetRate != s.rate || v.DebtRemaining != s.debtRemaining || v.Confirmed != s.confirmed || v.Pending != s.created-s.confirmed || v.ReportAttempts != s.reportAttempts || v.ReportRetries != s.reportRetries || !reflect.DeepEqual(v.ReportStatuses, s.reportStatus) || !reflect.DeepEqual(v.ReportErrorCodes, s.reportCodes) {
		return errors.New("sample is inconsistent with task/attempt ledger")
	}
	inFlight := 0
	for _, a := range s.actors {
		if a.inFlight {
			inFlight++
		}
	}
	if v.InFlight != inFlight {
		return errors.New("sample in-flight count mismatch")
	}
	return nil
}
