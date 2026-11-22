package capacity

// This file contains only the synthetic content model. It is not a load
// generator, a capacity result, or a substitute for a durable task ledger.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// SustainedModelVersion identifies the fixed synthetic distribution. Changing
// any draw or its order requires a new version, not an in-place retuning.
const SustainedModelVersion = "synthetic-sustained-v3"

// splitMix is specified here rather than depending on a runtime RNG's future
// implementation. It is for reproducible synthetic data, not credentials.
type splitMix struct{ state uint64 }

func (r *splitMix) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}
func (r *splitMix) intn(n int) int { return int(r.next() % uint64(n)) }

type syntheticKey struct {
	model int
}

type syntheticMetric struct {
	TotalTokens, InputTokens, OutputTokens, CacheReadTokens, CacheWriteTokens, ReasoningTokens int64
	RequestCount, SuccessCount, TTFTSamples, LatencySamples                                    int64
	TTFTAvgMS, LatencyAvgMS                                                                    int64
}

type syntheticRow struct {
	key syntheticKey
	syntheticMetric
}

type syntheticDay struct {
	revision int64
	rows     []syntheticRow
}

type syntheticClient struct {
	hot     bool
	class   string
	limit   int
	days    [2]syntheticDay // today, yesterday
	initial [2]json.RawMessage
}

// SustainedDelta is an incremental, independently reconstructible event. The
// two averages are fixed for a client/day/model and apply to the sample
// counts in this delta. All counts are nonnegative.
type SustainedDelta struct {
	Model            string `json:"model"`
	TotalTokens      int64  `json:"total_tokens"`
	InputTokens      int64  `json:"input_tokens"`
	OutputTokens     int64  `json:"output_tokens"`
	CacheReadTokens  int64  `json:"cache_read_tokens"`
	CacheWriteTokens int64  `json:"cache_write_tokens"`
	ReasoningTokens  int64  `json:"reasoning_tokens"`
	RequestCount     int64  `json:"request_count"`
	SuccessCount     int64  `json:"success_count"`
	TTFTAvgMS        int64  `json:"ttft_avg_ms"`
	TTFTSamples      int64  `json:"ttft_samples"`
	LatencyAvgMS     int64  `json:"latency_avg_ms"`
	LatencySamples   int64  `json:"latency_samples"`
}

// SustainedTask contains an immutable full snapshot and the corresponding
// increments. Client is a pre-seeded synthetic seat with one writer.
// Callers must persist the task before sending it.
type SustainedTask struct {
	Sequence int64            `json:"sequence"`
	Client   int              `json:"client"`
	Hot      bool             `json:"hot"`
	Class    string           `json:"class"`
	Limit    int              `json:"limit"`
	Day      string           `json:"day"`
	Revision int64            `json:"revision"`
	Deltas   []SustainedDelta `json:"deltas"`
	Body     json.RawMessage  `json:"body"`
	BodyHash string           `json:"body_sha256"`
}

// SustainedModel is single-owner state. It does not schedule arrivals, perform
// HTTP requests, persist credentials or implement retry/recovery.
type SustainedModel struct {
	day       [2]string
	models    []string
	rng       splitMix
	clients   []syntheticClient
	hot, cold []int // zero-based indices
	sequence  int64
	stock     int
}

// NewSustainedModel creates the deterministic stock population. Models must
// be the ordered, unique formal catalog names (at least 48). Stock clients have
// revision-1 snapshots on both open days.
func NewSustainedModel(day string, models []string, stock int, seed uint64) (*SustainedModel, error) {
	date, err := time.Parse(time.DateOnly, day)
	if err != nil || date.Format(time.DateOnly) != day || stock < 1 || stock > 100000 || len(models) < 48 {
		return nil, errors.New("invalid sustained model day, stock, or catalog")
	}
	seen := make(map[string]bool, len(models))
	for _, name := range models {
		if name == "" || seen[name] {
			return nil, errors.New("sustained model catalog names must be nonempty and unique")
		}
		seen[name] = true
	}
	m := &SustainedModel{day: [2]string{day, date.AddDate(0, 0, -1).Format(time.DateOnly)}, models: append([]string(nil), models...), rng: splitMix{state: seed}, stock: stock}
	for i := 0; i < stock; i++ {
		m.addClient()
	}
	return m, nil
}

func (m *SustainedModel) addClient() {
	c := syntheticClient{hot: m.rng.intn(10) == 0}
	switch n := m.rng.intn(100); {
	case n < 60:
		c.class, c.limit = "light", 1+m.rng.intn(4)
	case n < 90:
		c.class, c.limit = "medium", 5+m.rng.intn(12)
	default:
		c.class, c.limit = "heavy", 17+m.rng.intn(32)
	}
	for d := range c.days {
		day := &c.days[d]
		day.revision = 1
		count := 1 + m.rng.intn(c.limit)
		for j := 0; j < count; j++ {
			m.appendRow(day)
			row := &day.rows[len(day.rows)-1]
			for n := m.calls(); n > 0; n-- {
				addSynthetic(&row.syntheticMetric, m.call())
			}
		}
		var err error
		c.initial[d], err = m.body(day)
		if err != nil {
			panic(err) // the fixed JSON struct contains no unsupported values
		}
	}
	index := len(m.clients)
	m.clients = append(m.clients, c)
	if c.hot {
		m.hot = append(m.hot, index)
	} else {
		m.cold = append(m.cold, index)
	}
}

func (m *SustainedModel) calls() int {
	n := m.rng.intn(100)
	if n < 80 {
		return 1 + m.rng.intn(3)
	}
	if n < 98 {
		return 4 + m.rng.intn(17)
	}
	return 21 + m.rng.intn(80)
}

// call uses three bounded input-token bands (90/9/1 percent), bounded output,
// and integer-valued averages fixed per row. Invariants hold for every call.
func (m *SustainedModel) call() syntheticMetric {
	n := m.rng.intn(100)
	var input int64
	switch {
	case n < 90:
		input = int64(50 + m.rng.intn(1951))
	case n < 99:
		input = int64(2001 + m.rng.intn(10000))
	default:
		input = int64(12001 + m.rng.intn(38000))
	}
	output := int64(10 + m.rng.intn(991))
	read := input * int64(m.rng.intn(81)) / 100
	write := (input - read) * int64(m.rng.intn(31)) / 100
	reasoning := output * int64(m.rng.intn(51)) / 100
	v := syntheticMetric{TotalTokens: input + output, InputTokens: input, OutputTokens: output, CacheReadTokens: read, CacheWriteTokens: write, ReasoningTokens: reasoning, RequestCount: 1}
	if m.rng.intn(100) < 95 {
		v.SuccessCount = 1
	}
	if m.rng.intn(100) < 80 {
		v.TTFTSamples = 1
	}
	if m.rng.intn(100) < 90 {
		v.LatencySamples = 1
	}
	return v
}

func addSynthetic(a *syntheticMetric, b syntheticMetric) {
	a.TotalTokens += b.TotalTokens
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.CacheReadTokens += b.CacheReadTokens
	a.CacheWriteTokens += b.CacheWriteTokens
	a.ReasoningTokens += b.ReasoningTokens
	a.RequestCount += b.RequestCount
	a.SuccessCount += b.SuccessCount
	a.TTFTSamples += b.TTFTSamples
	a.LatencySamples += b.LatencySamples
}

func (m *SustainedModel) appendRow(day *syntheticDay) {
	// A catalog of at least 48 models and a maximum of 48 rows leave ample
	// unused models. The deterministic fallback also makes this loop bounded.
	var key syntheticKey
	found := false
	for tries := 0; tries < 256 && !found; tries++ {
		hotCount := (len(m.models) + 4) / 5
		model := m.rng.intn(hotCount)
		if m.rng.intn(100) >= 80 {
			model = hotCount + m.rng.intn(len(m.models)-hotCount)
		}
		key = syntheticKey{model}
		found = !hasSyntheticKey(day.rows, key)
	}
	if !found {
		for i := range m.models {
			key = syntheticKey{i}
			if !hasSyntheticKey(day.rows, key) {
				found = true
				break
			}
		}
	}
	if !found {
		panic("synthetic catalog exhausted") // impossible under constructor bounds
	}
	ttft := int64(40 + m.rng.intn(1961))
	latency := ttft + int64(100+m.rng.intn(8001))
	day.rows = append(day.rows, syntheticRow{key: key, syntheticMetric: syntheticMetric{TTFTAvgMS: ttft, LatencyAvgMS: latency}})
}

func hasSyntheticKey(rows []syntheticRow, key syntheticKey) bool {
	for _, row := range rows {
		if row.key == key {
			return true
		}
	}
	return false
}

// Initial returns the exact revision-1 body used to seed a stock client/day.
func (m *SustainedModel) Initial(client int, day string) (json.RawMessage, error) {
	if client < 1 || client > m.stock {
		return nil, errors.New("not a stock client")
	}
	for i, date := range m.day {
		if day == date {
			return bytes.Clone(m.clients[client-1].initial[i]), nil
		}
	}
	return nil, errors.New("day is not today or yesterday")
}

// Next advances the deterministic content sequence. The caller must separately
// persist and account for this task even if it cannot yet be sent.
func (m *SustainedModel) Next() (SustainedTask, error) {
	var task SustainedTask
	group := m.cold
	if m.rng.intn(2) == 0 {
		group = m.hot
	}
	if len(group) == 0 {
		group = m.cold
		if len(group) == 0 {
			group = m.hot
		}
	}
	index := group[m.rng.intn(len(group))]
	c := &m.clients[index]
	dateIndex := 0
	if m.rng.intn(100) < 10 {
		dateIndex = 1
	}
	d := &c.days[dateIndex]
	calls := m.calls()
	changed := 1 + m.rng.intn(4)
	if changed > calls {
		changed = calls
	}
	if changed > c.limit {
		changed = c.limit
	}
	selected := make(map[int]bool, changed)
	indices := make([]int, 0, changed)
	for len(indices) < changed {
		available := make([]int, 0, len(d.rows))
		for i := range d.rows {
			if !selected[i] {
				available = append(available, i)
			}
		}
		if len(d.rows) < c.limit && (len(available) == 0 || m.rng.intn(100) < 30) {
			m.appendRow(d)
			available = append(available, len(d.rows)-1)
			// Use the newly appended row when growth was selected.
			available = available[len(available)-1:]
		}
		if len(available) == 0 {
			return task, errors.New("synthetic row selection exhausted")
		}
		idx := m.weightedRow(d, available)
		selected[idx] = true
		indices = append(indices, idx)
	}
	deltas := make([]syntheticMetric, len(indices))
	// At least one call per changed row; remaining calls use the same 80/20
	// catalog preference among the rows selected for this update.
	for i := range indices {
		addSynthetic(&deltas[i], m.call())
	}
	for n := calls - len(indices); n > 0; n-- {
		idx := m.weightedRow(d, indices)
		for i, x := range indices {
			if x == idx {
				addSynthetic(&deltas[i], m.call())
				break
			}
		}
	}
	changes := make([]SustainedDelta, 0, len(indices))
	for i, idx := range indices {
		row := &d.rows[idx]
		v := deltas[i]
		addSynthetic(&row.syntheticMetric, v)
		changes = append(changes, SustainedDelta{Model: m.models[row.key.model], TotalTokens: v.TotalTokens, InputTokens: v.InputTokens, OutputTokens: v.OutputTokens, CacheReadTokens: v.CacheReadTokens, CacheWriteTokens: v.CacheWriteTokens, ReasoningTokens: v.ReasoningTokens, RequestCount: v.RequestCount, SuccessCount: v.SuccessCount, TTFTAvgMS: row.TTFTAvgMS, TTFTSamples: v.TTFTSamples, LatencyAvgMS: row.LatencyAvgMS, LatencySamples: v.LatencySamples})
	}
	d.revision++
	body, err := m.body(d)
	if err != nil {
		return task, err
	}
	m.sequence++
	sum := sha256.Sum256(body)
	return SustainedTask{Sequence: m.sequence, Client: index + 1, Hot: c.hot, Class: c.class, Limit: c.limit, Day: m.day[dateIndex], Revision: d.revision, Deltas: changes, Body: body, BodyHash: hex.EncodeToString(sum[:])}, nil
}

func (m *SustainedModel) weightedRow(day *syntheticDay, choices []int) int {
	hotCount := (len(m.models) + 4) / 5
	wantHot := m.rng.intn(100) < 80
	filtered := make([]int, 0, len(choices))
	for _, i := range choices {
		if (day.rows[i].key.model < hotCount) == wantHot {
			filtered = append(filtered, i)
		}
	}
	if len(filtered) == 0 {
		filtered = choices
	}
	return filtered[m.rng.intn(len(filtered))]
}

type syntheticItem struct {
	Model            string   `json:"model"`
	TotalTokens      string   `json:"total_tokens"`
	InputTokens      string   `json:"input_tokens"`
	OutputTokens     string   `json:"output_tokens"`
	CacheReadTokens  string   `json:"cache_read_tokens"`
	CacheWriteTokens string   `json:"cache_write_tokens"`
	ReasoningTokens  string   `json:"reasoning_tokens"`
	RequestCount     string   `json:"request_count"`
	SuccessCount     string   `json:"success_count"`
	TTFTAvgMS        *float64 `json:"ttft_avg_ms"`
	TTFTSamples      string   `json:"ttft_samples"`
	LatencyAvgMS     *float64 `json:"latency_avg_ms"`
	LatencySamples   string   `json:"latency_samples"`
}

func (m *SustainedModel) body(day *syntheticDay) (json.RawMessage, error) {
	items := make([]syntheticItem, 0, len(day.rows))
	str := func(n int64) string { return strconv.FormatInt(n, 10) }
	for _, r := range day.rows {
		item := syntheticItem{Model: m.models[r.key.model], TotalTokens: str(r.TotalTokens), InputTokens: str(r.InputTokens), OutputTokens: str(r.OutputTokens), CacheReadTokens: str(r.CacheReadTokens), CacheWriteTokens: str(r.CacheWriteTokens), ReasoningTokens: str(r.ReasoningTokens), RequestCount: str(r.RequestCount), SuccessCount: str(r.SuccessCount), TTFTSamples: str(r.TTFTSamples), LatencySamples: str(r.LatencySamples)}
		if r.TTFTSamples > 0 {
			v := float64(r.TTFTAvgMS)
			item.TTFTAvgMS = &v
		}
		if r.LatencySamples > 0 {
			v := float64(r.LatencyAvgMS)
			item.LatencyAvgMS = &v
		}
		items = append(items, item)
	}
	body, err := json.Marshal(struct {
		SchemaVersion int             `json:"schema_version"`
		Revision      string          `json:"revision"`
		Items         []syntheticItem `json:"items"`
	}{1, str(day.revision), items})
	if err != nil {
		return nil, fmt.Errorf("encode synthetic body: %w", err)
	}
	return body, nil
}
