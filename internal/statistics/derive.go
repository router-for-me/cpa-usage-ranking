package statistics

import (
	"errors"
	"math"

	"cpa-usage-ranking/internal/reports"
)

var ErrInvalidMetrics = errors.New("invalid raw statistics")

// Optional 区分真实的零和不可计算；原始 int64 不通过此类型转换。
type Optional struct {
	Value float64
	Valid bool
}

type Derived struct {
	CacheReadRate, CacheWriteRate, ReasoningRate, OutputRate Optional
	NonReasoningOutput                                       int64
	TotalPerRequest, OutputPerRequest, SuccessRate           Optional
	TTFTMeanMS, LatencyMeanMS, TTFTCoverage, LatencyCoverage Optional
	OutputRatePerSecond, OutputRateExcludingTTFTPerSecond    Optional
}

// Derive 只从原始合计计算；速度不是并发吞吐，也不是逐请求速率的算术均值。
// 非有限结果返回错误，不将溢出伪装为缺失或零。
func Derive(m reports.Metrics) (Derived, error) {
	for _, v := range []int64{m.TotalTokens, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheWriteTokens, m.ReasoningTokens, m.RequestCount, m.SuccessCount, m.TTFTSamples, m.LatencySamples} {
		if v < 0 {
			return Derived{}, ErrInvalidMetrics
		}
	}
	if !finiteNonnegative(m.TTFTSumMS) || !finiteNonnegative(m.LatencySumMS) {
		return Derived{}, ErrInvalidMetrics
	}
	var d Derived
	if m.OutputTokens > m.ReasoningTokens {
		d.NonReasoningOutput = m.OutputTokens - m.ReasoningTokens
	}
	d.CacheReadRate = clampedRatio(float64(m.CacheReadTokens), float64(m.InputTokens))
	d.CacheWriteRate = clampedRatio(float64(m.CacheWriteTokens), float64(m.InputTokens))
	d.ReasoningRate = clampedRatio(float64(m.ReasoningTokens), float64(m.OutputTokens))
	d.OutputRate = clampedRatio(float64(m.OutputTokens), float64(m.TotalTokens))
	d.TotalPerRequest = ratio(float64(m.TotalTokens), float64(m.RequestCount))
	d.OutputPerRequest = ratio(float64(m.OutputTokens), float64(m.RequestCount))
	d.SuccessRate = clampedRatio(float64(m.SuccessCount), float64(m.RequestCount))
	d.TTFTMeanMS = ratio(m.TTFTSumMS, float64(m.TTFTSamples))
	d.LatencyMeanMS = ratio(m.LatencySumMS, float64(m.LatencySamples))
	d.TTFTCoverage = clampedRatio(float64(m.TTFTSamples), float64(m.RequestCount))
	d.LatencyCoverage = clampedRatio(float64(m.LatencySamples), float64(m.RequestCount))
	if m.RequestCount > 0 && m.LatencySamples == m.RequestCount {
		d.OutputRatePerSecond = ratio(float64(m.OutputTokens)*1000, m.LatencySumMS)
		if m.TTFTSamples == m.RequestCount && m.LatencySumMS > m.TTFTSumMS {
			d.OutputRateExcludingTTFTPerSecond = ratio(float64(m.OutputTokens)*1000, m.LatencySumMS-m.TTFTSumMS)
		}
	}
	var overflow bool
	if d.OutputRatePerSecond.Valid && !finiteNonnegative(d.OutputRatePerSecond.Value) {
		d.OutputRatePerSecond = Optional{}
		overflow = true
	}
	if d.OutputRateExcludingTTFTPerSecond.Valid && !finiteNonnegative(d.OutputRateExcludingTTFTPerSecond.Value) {
		d.OutputRateExcludingTTFTPerSecond = Optional{}
		overflow = true
	}
	if overflow {
		return d, ErrOverflow
	}
	return d, nil
}

func ratio(n, denominator float64) Optional {
	if denominator == 0 {
		return Optional{}
	}
	return Optional{Value: n / denominator, Valid: true}
}

func clampedRatio(n, denominator float64) Optional {
	r := ratio(n, denominator)
	if !r.Valid {
		return r
	}
	if r.Value < 0 {
		r.Value = 0
	} else if r.Value > 1 {
		r.Value = 1
	}
	return r
}
func finiteNonnegative(n float64) bool { return n >= 0 && !math.IsInf(n, 0) && !math.IsNaN(n) }
