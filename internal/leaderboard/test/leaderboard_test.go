package test

import (
	"context"
	"math"
	"strconv"
	"testing"

	"cpa-usage-ranking/internal/leaderboard"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
)

func snapshot() statistics.Snapshot {
	return statistics.Snapshot{Rows: map[int64]reports.Metrics{}, Profiles: map[int64]statistics.Profile{}}
}
func add(s *statistics.Snapshot, id int64, m reports.Metrics) {
	s.Rows[id] = m
	s.Profiles[id] = statistics.Profile{DisplayName: "相同昵称", AvatarID: 1}
}
func build(t *testing.T, s statistics.Snapshot) map[string][]leaderboard.Entry {
	t.Helper()
	b, err := leaderboard.Build(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func find(t *testing.T, rows []leaderboard.Entry, id int64) leaderboard.Entry {
	t.Helper()
	for _, row := range rows {
		if row.ClientID == strconv.FormatInt(id, 10) {
			return row
		}
	}
	t.Fatalf("missing id %d in %+v", id, rows)
	return leaderboard.Entry{}
}

func TestLowSamplesMissingAndRealZero(t *testing.T) {
	s := snapshot()
	add(&s, 1, reports.Metrics{RequestCount: 1, SuccessCount: 1, TotalTokens: 1, InputTokens: 1})
	add(&s, 2, reports.Metrics{RequestCount: 1})
	add(&s, 3, reports.Metrics{RequestCount: 1, TTFTSamples: 1})
	b := build(t, s)
	if got := find(t, b[leaderboard.Overall], 1).Metric; got != "26.20" {
		t.Fatal(got)
	}
	if got := find(t, b[leaderboard.Overall], 2).Metric; got != "0.00" {
		t.Fatal(got)
	}
	if got := find(t, b[leaderboard.Overall], 3).Metric; got != "5.00" {
		t.Fatal(got)
	}
	if len(b[leaderboard.Usage]) != 1 || len(b[leaderboard.Cache]) != 1 || b[leaderboard.Cache][0].Metric != "0.00" {
		t.Fatal(b)
	}
	if len(b[leaderboard.TTFT]) != 1 || b[leaderboard.TTFT][0].Metric != "0.00" {
		t.Fatal(b[leaderboard.TTFT])
	}
	if find(t, b[leaderboard.Success], 1).Metric != "20.65" {
		t.Fatal(b[leaderboard.Success])
	}
	if find(t, b[leaderboard.Overall], 1).DisplayName != find(t, b[leaderboard.Overall], 2).DisplayName {
		t.Fatal("names changed")
	}
}

func TestCacheRationalHalfUpAndInt64Boundary(t *testing.T) {
	s := snapshot()
	add(&s, 1, reports.Metrics{RequestCount: 1, TotalTokens: 800, InputTokens: 800, CacheReadTokens: 201})
	b := build(t, s)
	// 201/800 的缓存贡献恰为 5.025 分，必须 half-up 为 5.03。
	if got := b[leaderboard.Overall][0].Metric; got != "25.03" {
		t.Fatal(got)
	}
	if got := b[leaderboard.Cache][0].Metric; got != "25.13" {
		t.Fatal(got)
	}
	s = snapshot()
	scale := int64(math.MaxInt64 / 800)
	add(&s, 1, reports.Metrics{RequestCount: 1, TotalTokens: 800 * scale, InputTokens: 800 * scale, CacheReadTokens: 201 * scale})
	b = build(t, s)
	if b[leaderboard.Overall][0].Metric != "25.03" || b[leaderboard.Cache][0].Metric != "25.13" {
		t.Fatal(b)
	}
	s = snapshot()
	add(&s, 1, reports.Metrics{RequestCount: 1, TotalTokens: math.MaxInt64, InputTokens: math.MaxInt64, CacheReadTokens: math.MaxInt64})
	b = build(t, s)
	if got := b[leaderboard.Overall][0].Metric; got != "40.00" {
		t.Fatal(got)
	}
	if got := b[leaderboard.Cache][0].Metric; got != "100.00" {
		t.Fatal(got)
	}
}

func TestCacheHalfUpTiesUseDisplayedPrecision(t *testing.T) {
	s := snapshot()
	add(&s, 1, reports.Metrics{RequestCount: 1, TotalTokens: 800, InputTokens: 800, CacheReadTokens: 201})
	add(&s, 2, reports.Metrics{RequestCount: 1, TotalTokens: 10000, InputTokens: 10000, CacheReadTokens: 2513})
	add(&s, 3, reports.Metrics{RequestCount: 1, TotalTokens: 10000, InputTokens: 10000, CacheReadTokens: 2512})
	rows := build(t, s)[leaderboard.Cache]
	if rows[0].ClientID != "1" || rows[0].Metric != "25.13" || rows[1].Rank != 1 || rows[2].Rank != 3 || rows[2].Metric != "25.12" {
		t.Fatal(rows)
	}
}

func TestTTFTContributionHalfUpAtExactRationalBoundary(t *testing.T) {
	s := snapshot()
	// 正中位数为 1ms，候选均值 2ms、覆盖 3/80：10×(1/3)×(3/80)=0.125。
	add(&s, 1, reports.Metrics{RequestCount: 80, TTFTSamples: 3, TTFTSumMS: 6})
	add(&s, 2, reports.Metrics{RequestCount: 1, TTFTSamples: 1, TTFTSumMS: 1})
	add(&s, 3, reports.Metrics{RequestCount: 1, TTFTSamples: 1, TTFTSumMS: 1})
	if got := find(t, build(t, s)[leaderboard.Overall], 1).Metric; got != "0.13" {
		t.Fatal(got)
	}
	// 中性零参照和 2^53 以上计数也不能把精确半分舍低。
	s = snapshot()
	scale := int64(math.MaxInt64 / 80)
	add(&s, 1, reports.Metrics{RequestCount: 80 * scale, TTFTSamples: 74 * scale})
	if got := find(t, build(t, s)[leaderboard.Overall], 1).Metric; got != "4.63" {
		t.Fatal(got)
	}
}

func TestWilsonCoverageMedianAndRounding(t *testing.T) {
	s := snapshot()
	add(&s, 1, reports.Metrics{RequestCount: 1, SuccessCount: 1, TTFTSamples: 1, TTFTSumMS: 100})
	add(&s, 2, reports.Metrics{RequestCount: 1000, SuccessCount: 1000, TTFTSamples: 200, TTFTSumMS: 20000})
	add(&s, 3, reports.Metrics{RequestCount: 1, TTFTSamples: 1, TTFTSumMS: 0})
	b := build(t, s)
	if got := find(t, b[leaderboard.Overall], 1).Metric; got != "11.20" {
		t.Fatal(got)
	}
	if got := find(t, b[leaderboard.Overall], 2).Metric; got != "30.89" {
		t.Fatal(got)
	}
	if got := find(t, b[leaderboard.Overall], 3).Metric; got != "10.00" {
		t.Fatal(got)
	}
	if find(t, b[leaderboard.Success], 2).Rank >= find(t, b[leaderboard.Success], 1).Rank {
		t.Fatal(b[leaderboard.Success])
	}
}

func TestExactIntegersTiesPrecisionAndTop100(t *testing.T) {
	s := snapshot()
	for id := int64(1); id <= 102; id++ {
		add(&s, id, reports.Metrics{RequestCount: 1, TotalTokens: 9007199254740993, InputTokens: 10000, CacheReadTokens: 1000, TTFTSamples: 1, TTFTSumMS: 10.004})
	}
	m := s.Rows[102]
	m.TotalTokens++
	m.CacheReadTokens = 1001
	m.TTFTSumMS = 10.006
	s.Rows[102] = m
	b := build(t, s)
	rows := b[leaderboard.Usage]
	if len(rows) != 100 || rows[0].ClientID != "102" || rows[0].Metric != "9007199254740994" || rows[1].Rank != 2 || rows[99].Rank != 2 || rows[99].ClientID != "99" {
		t.Fatal(rows[0], rows[1], rows[99])
	}
	if find(t, b[leaderboard.Cache], 102).Rank != 1 || find(t, b[leaderboard.TTFT], 1).Rank != 1 {
		t.Fatal("bad decimal ordering")
	}
	// 真实不同值但同一两位显示值并列；1,1,3 跳号。
	s = snapshot()
	for i, v := range []float64{10.001, 10.004, 10.006} {
		add(&s, int64(i+1), reports.Metrics{RequestCount: 1, TTFTSamples: 1, TTFTSumMS: v})
	}
	rows = build(t, s)[leaderboard.TTFT]
	if rows[0].Rank != 1 || rows[1].Rank != 1 || rows[2].Rank != 3 || rows[2].Metric != "10.01" {
		t.Fatal(rows)
	}
}

func TestReferencesUseAllCandidatesBeforeTop100(t *testing.T) {
	s := snapshot()
	for id := int64(1); id <= 101; id++ {
		add(&s, id, reports.Metrics{RequestCount: 1, TotalTokens: id})
	}
	b := build(t, s)
	if len(b[leaderboard.Overall]) != 100 || b[leaderboard.Overall][0].Metric != "39.80" {
		t.Fatal(b[leaderboard.Overall][0])
	}
	s = snapshot()
	for id := int64(1); id <= 101; id++ {
		add(&s, id, reports.Metrics{RequestCount: 1, TTFTSamples: 1, TTFTSumMS: float64(id)})
	}
	b = build(t, s)
	if b[leaderboard.Overall][0].Metric != "9.81" || b[leaderboard.Overall][0].ClientID != "1" {
		t.Fatal(b[leaderboard.Overall][0])
	}
}

func TestInvalidAndExtremeMeasurements(t *testing.T) {
	s := snapshot()
	add(&s, 1, reports.Metrics{RequestCount: 1, TTFTSamples: 1, TTFTSumMS: math.MaxFloat64})
	add(&s, 2, reports.Metrics{RequestCount: 1, TTFTSamples: 1, TTFTSumMS: math.MaxFloat64 / 2})
	add(&s, 3, reports.Metrics{OutputTokens: 1, RequestCount: 1, LatencySamples: 1, LatencySumMS: math.SmallestNonzeroFloat64})
	b := build(t, s)
	if b[leaderboard.TTFT][0].ClientID != "2" || find(t, b[leaderboard.Overall], 3).Details.LatencyMeanMS == nil || *find(t, b[leaderboard.Overall], 3).Details.LatencyMeanMS != "0.00" {
		t.Fatal(b[leaderboard.TTFT][0], find(t, b[leaderboard.Overall], 3))
	}
	delete(s.Profiles, 1)
	if _, err := leaderboard.Build(context.Background(), s); err == nil {
		t.Fatal("missing profile accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := leaderboard.Build(ctx, s); err == nil {
		t.Fatal("canceled build accepted")
	}
}

func TestClampedInconsistentMetricsNeverPanicOrFail(t *testing.T) {
	s := snapshot()
	// 极端越界分子（包括 MaxInt64 / 1）在 roundRatio 中必须安全钳制到上限，不得触发 bits.Div64 panic。
	add(&s, 1, reports.Metrics{
		TotalTokens:     100,
		InputTokens:     1,
		CacheReadTokens: math.MaxInt64,
		RequestCount:    2,
		SuccessCount:    10,
		TTFTSamples:     5,
		TTFTSumMS:       250,
	})
	add(&s, 2, reports.Metrics{
		TotalTokens:     50,
		InputTokens:     10,
		CacheReadTokens: 20,
		RequestCount:    1,
		SuccessCount:    1,
		TTFTSamples:     0,
		TTFTSumMS:       123,
	})
	b := build(t, s)
	row1 := find(t, b[leaderboard.Overall], 1)
	if row1.Details == nil || row1.Details.SuccessRate != "100.00" || row1.Details.CacheReadRate == nil || *row1.Details.CacheReadRate != "100.00" {
		t.Fatalf("row1 details not clamped: %+v", row1.Details)
	}
	if find(t, b[leaderboard.Cache], 1).Metric != "100.00" || find(t, b[leaderboard.Cache], 2).Metric != "100.00" {
		t.Fatalf("cache board not clamped: %+v", b[leaderboard.Cache])
	}
	row2 := find(t, b[leaderboard.Overall], 2)
	if row2.Details == nil || row2.Details.TTFTMeanMS != nil {
		t.Fatalf("zero-sample residual TTFT should be nil: %+v", row2.Details)
	}
}
