package capacity

// Streaming descriptive statistics for a verified sustained journal. This
// is not an independent DB verification or a substitute for stage assessment.
import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"time"
)

type SustainedQuantiles struct {
	Count int64 `json:"count"`
	P50   int64 `json:"p50_upper"`
	P95   int64 `json:"p95_upper"`
	P99   int64 `json:"p99_upper"`
}

type SustainedStageStatistics struct {
	Stage          string             `json:"stage"`
	Rate           float64            `json:"target_rate"`
	Created        int64              `json:"created"`
	Confirmed      int64              `json:"confirmed"`
	TouchedClients int64              `json:"touched_clients"`
	TodayTasks     int64              `json:"today_tasks"`
	YesterdayTasks int64              `json:"yesterday_tasks"`
	HotTasks       int64              `json:"hot_tasks"`
	DebtTasks      int64              `json:"debt_tasks"`
	ReportAttempts int64              `json:"report_attempts"`
	BodyBytes      SustainedQuantiles `json:"body_bytes"`
	ModelRows      SustainedQuantiles `json:"model_rows"`
	ChangedRows    SustainedQuantiles `json:"changed_rows"`
	ConfirmDelayMS SustainedQuantiles `json:"confirm_delay_ms"`
	DelayOverHour  int64              `json:"delay_over_one_hour"`
	ModelCalls     map[string]int64   `json:"model_calls"`
}

type SustainedStatistics struct {
	Stages         []SustainedStageStatistics `json:"stages"`
	Produced       int64                      `json:"produced"`
	Confirmed      int64                      `json:"confirmed"`
	Pending        int64                      `json:"pending"`
	QuantileMethod string                     `json:"quantile_method"`
}

type sustainedDistribution map[int64]int64

func (d sustainedDistribution) add(v int64) { d[v]++ }

func (d sustainedDistribution) quantiles() SustainedQuantiles {
	keys := make([]int64, 0, len(d))
	var total int64
	for k, n := range d {
		keys = append(keys, k)
		total += n
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	result := SustainedQuantiles{Count: total}
	if total == 0 {
		return result
	}
	want := [3]int64{(total*50 + 99) / 100, (total*95 + 99) / 100, (total*99 + 99) / 100}
	answers := [3]*int64{&result.P50, &result.P95, &result.P99}
	var seen int64
	idx := 0
	for _, k := range keys {
		seen += d[k]
		for idx < len(want) && seen >= want[idx] {
			*answers[idx] = k
			idx++
		}
	}
	return result
}

type sustainedStageStats struct {
	result                       SustainedStageStatistics
	body, rows, changed, latency sustainedDistribution
}

type sustainedSummaryPending struct {
	stage   int
	planned int64
}

func latencyBucket(ms int64) int64 {
	if ms <= 10000 {
		return (ms + 9) / 10 * 10
	}
	if ms <= 60000 {
		return (ms + 99) / 100 * 100
	}
	return (ms + 999) / 1000 * 1000
}

// SummarizeSustained reads one record at a time; it holds only outstanding
// sequence IDs, one bitmap per identity and bounded histograms. It rejects
// incomplete gzip/JSONL and refuses to exceed the caller's pending, two
// million identities or 32-stage bounds. Verification of
// every delta, attempt and final database must still be done separately.
func SummarizeSustained(path string, models []string, maxPending int, migrationPaths ...string) (SustainedStatistics, error) {
	var out SustainedStatistics
	if maxPending < 1 || maxPending > 1000000 || len(models) == 0 || len(models) > 10000 {
		return out, errors.New("invalid sustained summary pending or catalog bound")
	}
	modelSet := make(map[string]bool, len(models))
	for _, model := range models {
		if model == "" || modelSet[model] {
			return out, errors.New("invalid or duplicate summary catalog model")
		}
		modelSet[model] = true
	}
	file, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer file.Close()
	if len(migrationPaths) > 2 {
		return out, errors.New("at most one reviewed retry and pending manifest are allowed")
	}
	var migrationPath, pendingPath string
	if len(migrationPaths) >= 1 {
		migrationPath = migrationPaths[0]
	}
	if len(migrationPaths) == 2 {
		pendingPath = migrationPaths[1]
	}
	authorized, err := loadSustainedRetryMigration(migrationPath, file)
	if err != nil {
		return out, err
	}
	pendingAuthorized, err := loadSustainedPendingMigration(pendingPath, file)
	if err != nil {
		return out, err
	}
	if pendingAuthorized != nil && (authorized == nil || authorized.NextPending != pendingAuthorized.PreviousPending || authorized.NextAttempts != pendingAuthorized.MaxAttempts ||
		authorized.NextConfigHash != pendingAuthorized.PreviousConfigHash || authorized.NextBinarySHA != pendingAuthorized.PreviousBinarySHA) {
		return out, errors.New("pending summary manifest does not follow retry manifest")
	}
	source, _, err := sustainedJournalReader(file)
	if err != nil {
		return out, err
	}
	defer source.Close()
	reader := bufio.NewReaderSize(source, 256*1024)
	var stages []sustainedStageStats
	stageByName := map[string]int{}
	pending := make(map[int64]sustainedSummaryPending)
	// A 32-bit stage bitmap bounds uniqueness tracking to one word per
	// identity instead of retaining every task or completed request body.
	clients := make(map[int]uint32)
	current, lineNo, lastKind := -1, 0, ""
	endCount, attemptLimit := 0, 0
	var header *sustainedHeader
	migrated, pendingMigrated := false, false
	var lastSequence int64
	var today, yesterday string
	for {
		line, e := readSustainedLine(reader)
		if len(line) == 0 && e == io.EOF {
			break
		}
		lineNo++
		if e != nil {
			return out, fmt.Errorf("incomplete summary journal line %d: %w", lineNo, e)
		}
		var r sustainedRecord
		if json.Unmarshal(line, &r) != nil {
			return out, fmt.Errorf("invalid summary journal JSON at line %d", lineNo)
		}
		previousKind := lastKind
		lastKind = r.Kind
		if lineNo == 1 {
			catalog, _ := json.Marshal(models)
			hash := sha256.Sum256(catalog)
			if r.Kind != "header" || r.Header == nil || r.Header.Version != SustainedModelVersion || r.Header.CatalogHash != hex.EncodeToString(hash[:]) || r.Header.MaxAttempts < 1 || r.Header.MaxAttempts > 512 {
				return out, errors.New("summary journal header/catalog mismatch")
			}
			day, parseErr := time.Parse(time.DateOnly, r.Header.Day)
			if parseErr != nil || day.Format(time.DateOnly) != r.Header.Day {
				return out, errors.New("invalid summary journal UTC day")
			}
			today, yesterday = r.Header.Day, day.AddDate(0, 0, -1).Format(time.DateOnly)
			header, attemptLimit = r.Header, r.Header.MaxAttempts
			continue
		}
		switch r.Kind {
		case "stage":
			if r.Command == nil {
				return out, errors.New("summary stage missing command")
			}
			if r.Command.Operation == "stage" {
				if _, exists := stageByName[r.Command.Stage]; exists || r.Command.Stage == "" || len(stages) >= 32 {
					return out, errors.New("duplicate or excessive summary stages")
				}
				current = len(stages)
				stageByName[r.Command.Stage] = current
				stages = append(stages, sustainedStageStats{result: SustainedStageStatistics{Stage: r.Command.Stage, Rate: r.Command.Rate, ModelCalls: map[string]int64{}}, body: sustainedDistribution{}, rows: sustainedDistribution{}, changed: sustainedDistribution{}, latency: sustainedDistribution{}})
			}
		case "created":
			if current < 0 || r.Task == nil || r.Task.Client <= 0 || r.PlannedUnixNS <= 0 || (r.Source != "arrival" && r.Source != "debt") || len(pending) >= maxPending {
				return out, errors.New("invalid or excessive summary pending tasks")
			}
			if r.Task.Sequence != lastSequence+1 {
				return out, errors.New("nonconsecutive summary sequence")
			}
			lastSequence = r.Task.Sequence
			var body struct {
				Items []json.RawMessage `json:"items"`
			}
			if json.Unmarshal(r.Task.Body, &body) != nil || len(body.Items) == 0 || len(body.Items) > 128 {
				return out, errors.New("invalid summary body")
			}
			pending[r.Task.Sequence] = sustainedSummaryPending{stage: current, planned: r.PlannedUnixNS}
			s := &stages[current]
			mask, exists := clients[r.Task.Client]
			if !exists && len(clients) >= 2000000 {
				return out, errors.New("summary identity memory bound reached")
			}
			bit := uint32(1) << uint(current)
			if mask&bit == 0 {
				s.result.TouchedClients++
				clients[r.Task.Client] = mask | bit
			}
			switch r.Task.Day {
			case today:
				s.result.TodayTasks++
			case yesterday:
				s.result.YesterdayTasks++
			default:
				return out, errors.New("summary task outside fixture UTC days")
			}
			s.result.Created++
			out.Produced++
			if r.Task.Hot {
				s.result.HotTasks++
			}
			if r.Source == "debt" {
				s.result.DebtTasks++
			}
			s.body.add((int64(len(r.Task.Body)) + 15) / 16 * 16)
			s.rows.add(int64(len(body.Items)))
			s.changed.add(int64(len(r.Task.Deltas)))
			if len(r.Task.Deltas) < 1 || len(r.Task.Deltas) > 4 {
				return out, errors.New("invalid summary delta count")
			}
			for _, delta := range r.Task.Deltas {
				if !modelSet[delta.Model] || delta.RequestCount < 1 || s.result.ModelCalls[delta.Model] > math.MaxInt64-delta.RequestCount {
					return out, errors.New("invalid or overflowing summary model calls")
				}
				s.result.ModelCalls[delta.Model] += delta.RequestCount
			}
		case "start":
			if r.Attempt < 1 || r.Attempt > attemptLimit {
				return out, errors.New("summary attempt exceeds declared limit")
			}
			job, ok := pending[r.Sequence]
			if !ok {
				return out, errors.New("summary attempt has no pending task")
			}
			if r.Action != "report" {
				return out, errors.New("unknown summary action")
			}
			stages[job.stage].result.ReportAttempts++
		case "finish":
			if r.Action == "report" && r.Confirmed {
				job, ok := pending[r.Sequence]
				if !ok || r.TimeUnixNS < job.planned {
					return out, errors.New("invalid summary acknowledgement")
				}
				delete(pending, r.Sequence)
				delta := r.TimeUnixNS - job.planned
				var ms int64
				if delta > 3600000*1000000 {
					ms = 3600001
					stages[job.stage].result.DelayOverHour++
				} else {
					ms = (delta + 999999) / 1000000
				}
				stages[job.stage].latency.add(latencyBucket(ms))
				stages[job.stage].result.Confirmed++
				out.Confirmed++
			}
		case "end":
			endCount++
		case "retry_migration":
			m := r.RetryMigration
			if migrated || previousKind != "end" || endCount != 2 || m == nil || authorized == nil || *m != *authorized ||
				header.MaxAttempts != m.PreviousAttempts || header.MaxPending != m.PreviousPending {
				return out, errors.New("invalid summary retry migration boundary")
			}
			if err := m.validate(file); err != nil {
				return out, err
			}
			attemptLimit, migrated = m.NextAttempts, true
		case "pending_migration":
			m := r.PendingMigration
			if pendingMigrated || !migrated || previousKind != "end" || endCount != 3 || m == nil || pendingAuthorized == nil || *m != *pendingAuthorized ||
				out.Produced != m.Produced || out.Confirmed != m.Confirmed || int64(len(pending)) != m.Pending {
				return out, errors.New("invalid summary pending migration boundary")
			}
			if err := m.validate(file); err != nil {
				return out, err
			}
			pendingMigrated = true
		case "sample", "recover":
		default:
			return out, fmt.Errorf("unsupported summary record %q", r.Kind)
		}
	}
	if authorized != nil && !migrated {
		return out, errors.New("authorized summary retry migration record missing")
	}
	if pendingAuthorized != nil && !pendingMigrated {
		return out, errors.New("authorized summary pending migration record missing")
	}
	if lineNo == 0 || lastKind != "end" {
		return out, errors.New("summary journal not cleanly closed")
	}
	out.Pending = int64(len(pending))
	if out.Pending != out.Produced-out.Confirmed {
		return out, errors.New("summary pending mismatch")
	}
	out.QuantileMethod = "nearest rank; body upper bound in 16-byte buckets; latency upper bound 10ms <=10s, 100ms <=60s, then 1s; -1 means greater than one hour"
	for _, s := range stages {
		s.result.BodyBytes = s.body.quantiles()
		s.result.ModelRows = s.rows.quantiles()
		s.result.ChangedRows = s.changed.quantiles()
		s.result.ConfirmDelayMS = s.latency.quantiles()
		for _, quantile := range []*int64{&s.result.ConfirmDelayMS.P50, &s.result.ConfirmDelayMS.P95, &s.result.ConfirmDelayMS.P99} {
			if *quantile == 3601000 {
				*quantile = -1
			}
		}
		out.Stages = append(out.Stages, s.result)
	}
	return out, nil
}
