package test

import (
	"errors"
	"math"
	"testing"

	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
)

func approx(t *testing.T, got statistics.Optional, want float64) {
	t.Helper()
	if !got.Valid || math.Abs(got.Value-want) > 1e-10*math.Max(1, math.Abs(want)) {
		t.Fatalf("got %+v want %g", got, want)
	}
}
func TestDeriveWeightedMetricsAndCompleteSpeed(t *testing.T) {
	a := reports.Metrics{TotalTokens: 100, InputTokens: 60, OutputTokens: 40, CacheReadTokens: 12, CacheWriteTokens: 6, ReasoningTokens: 10, RequestCount: 2, SuccessCount: 1, TTFTSumMS: 40, TTFTSamples: 2, LatencySumMS: 200, LatencySamples: 2}
	b := reports.Metrics{TotalTokens: 50, InputTokens: 30, OutputTokens: 20, CacheReadTokens: 3, CacheWriteTokens: 3, ReasoningTokens: 5, RequestCount: 1, SuccessCount: 1, TTFTSumMS: 80, TTFTSamples: 1, LatencySumMS: 100, LatencySamples: 1}
	m, ok := reports.AddMetrics(a, b)
	if !ok {
		t.Fatal("add")
	}
	d, err := statistics.Derive(m)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, d.CacheReadRate, 15.0/90)
	approx(t, d.CacheWriteRate, 9.0/90)
	approx(t, d.ReasoningRate, 15.0/60)
	approx(t, d.OutputRate, 60.0/150)
	if d.NonReasoningOutput != 45 {
		t.Fatal(d.NonReasoningOutput)
	}
	approx(t, d.TotalPerRequest, 50)
	approx(t, d.OutputPerRequest, 20)
	approx(t, d.SuccessRate, 2.0/3)
	approx(t, d.TTFTMeanMS, 40)
	approx(t, d.LatencyMeanMS, 100)
	approx(t, d.TTFTCoverage, 1)
	approx(t, d.LatencyCoverage, 1)
	approx(t, d.OutputRatePerSecond, 200)
	approx(t, d.OutputRateExcludingTTFTPerSecond, 60000.0/180)
	if m.TotalTokens != 150 || m.RequestCount != 3 {
		t.Fatal("raw counts changed")
	}
}
func TestDeriveMissingZeroAndCoverage(t *testing.T) {
	d, err := statistics.Derive(reports.Metrics{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range []statistics.Optional{d.CacheReadRate, d.CacheWriteRate, d.ReasoningRate, d.OutputRate, d.TotalPerRequest, d.OutputPerRequest, d.SuccessRate, d.TTFTMeanMS, d.LatencyMeanMS, d.TTFTCoverage, d.LatencyCoverage, d.OutputRatePerSecond, d.OutputRateExcludingTTFTPerSecond} {
		if o.Valid {
			t.Fatalf("zero denominator: %+v", o)
		}
	}
	m := reports.Metrics{TotalTokens: 4, InputTokens: 3, OutputTokens: 1, RequestCount: 2, TTFTSamples: 1, LatencySamples: 1}
	d, err = statistics.Derive(m)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, d.CacheReadRate, 0)
	approx(t, d.SuccessRate, 0)
	approx(t, d.TTFTMeanMS, 0)
	approx(t, d.LatencyMeanMS, 0)
	approx(t, d.TTFTCoverage, 0.5)
	approx(t, d.LatencyCoverage, 0.5)
	if d.OutputRatePerSecond.Valid || d.OutputRateExcludingTTFTPerSecond.Valid {
		t.Fatal("partial coverage yielded speed")
	}
	m.LatencySamples = 2
	m.LatencySumMS = 100
	d, err = statistics.Derive(m)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, d.OutputRatePerSecond, 10)
	if d.OutputRateExcludingTTFTPerSecond.Valid {
		t.Fatal("partial TTFT yielded speed")
	}
	m.TTFTSamples = 2
	for _, sum := range []float64{100, 101} {
		m.TTFTSumMS = sum
		d, err = statistics.Derive(m)
		if err != nil {
			t.Fatal(err)
		}
		approx(t, d.OutputRatePerSecond, 10)
		if d.OutputRateExcludingTTFTPerSecond.Valid {
			t.Fatal("nonpositive generation interval")
		}
	}
	m.LatencySumMS = 0
	m.TTFTSumMS = 0
	d, err = statistics.Derive(m)
	if err != nil {
		t.Fatal(err)
	}
	if d.OutputRatePerSecond.Valid || d.OutputRateExcludingTTFTPerSecond.Valid {
		t.Fatal("zero duration")
	}
	m.OutputTokens = 0
	m.LatencySumMS = 10
	d, err = statistics.Derive(m)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, d.OutputRatePerSecond, 0)
	approx(t, d.OutputRateExcludingTTFTPerSecond, 0)
}
func TestDeriveNumericBoundaries(t *testing.T) {
	m := reports.Metrics{TotalTokens: math.MaxInt64, InputTokens: 1, OutputTokens: math.MaxInt64 - 1, ReasoningTokens: math.MaxInt64 - 2, RequestCount: math.MaxInt64, SuccessCount: math.MaxInt64, LatencySamples: math.MaxInt64, TTFTSamples: math.MaxInt64, LatencySumMS: math.MaxFloat64, TTFTSumMS: math.MaxFloat64 / 2}
	d, err := statistics.Derive(m)
	if err != nil {
		t.Fatal(err)
	}
	if d.NonReasoningOutput != 1 {
		t.Fatal(d.NonReasoningOutput)
	}
	approx(t, d.SuccessRate, 1)
	if !d.OutputRatePerSecond.Valid || math.IsInf(d.OutputRatePerSecond.Value, 0) {
		t.Fatal(d)
	}
	m = reports.Metrics{OutputTokens: 1, RequestCount: 1, LatencySamples: 1, LatencySumMS: math.SmallestNonzeroFloat64}
	if _, err = statistics.Derive(m); !errors.Is(err, statistics.ErrOverflow) {
		t.Fatalf("speed overflow: %v", err)
	}
	for _, bad := range []reports.Metrics{{TTFTSumMS: math.NaN()}, {LatencySumMS: math.Inf(1)}, {TTFTSumMS: -1}, {RequestCount: -1}} {
		if _, err = statistics.Derive(bad); !errors.Is(err, statistics.ErrInvalidMetrics) {
			t.Fatalf("invalid %+v: %v", bad, err)
		}
	}
	// 非负但子项大于父项的统计应平滑钳制在 [0, 1]，不导致缓存构建失败。
	clamped, err := statistics.Derive(reports.Metrics{
		TotalTokens: 10, InputTokens: 5, OutputTokens: 12,
		CacheReadTokens: 8, CacheWriteTokens: 9, ReasoningTokens: 20,
		RequestCount: 2, SuccessCount: 5,
		TTFTSamples: 0, TTFTSumMS: 99,
		LatencySamples: 4, LatencySumMS: 400,
	})
	if err != nil {
		t.Fatalf("inconsistent non-negative metrics rejected: %v", err)
	}
	if clamped.NonReasoningOutput != 0 || clamped.TTFTMeanMS.Valid {
		t.Fatalf("unexpected non-reasoning or zero-sample mean: %+v", clamped)
	}
	for _, rate := range []statistics.Optional{clamped.CacheReadRate, clamped.CacheWriteRate, clamped.ReasoningRate, clamped.OutputRate, clamped.SuccessRate, clamped.LatencyCoverage} {
		approx(t, rate, 1)
	}
}
