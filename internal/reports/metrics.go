package reports

import "math"

// Metrics is the shared storage and aggregation representation. Averages are
// converted to sums with their actual sample counts, never guessed from requests.
type Metrics struct {
	TotalTokens      int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	RequestCount     int64
	SuccessCount     int64
	TTFTSumMS        float64
	TTFTSamples      int64
	LatencySumMS     float64
	LatencySamples   int64
}

// Validate checks hard bounds (non-negative counts, finite non-negative averages,
// and no integer/float aggregation overflow) across all items in the report.
func Validate(report Report) error {
	if report.Revision < 0 {
		return ErrInvalid
	}
	for _, item := range report.Items {
		if _, err := itemMetrics(item); err != nil {
			return ErrInvalid
		}
	}
	return nil
}

func itemMetrics(item Item) (Metrics, error) {
	m := Metrics{
		TotalTokens: item.TotalTokens, InputTokens: item.InputTokens, OutputTokens: item.OutputTokens,
		CacheReadTokens: item.CacheReadTokens, CacheWriteTokens: item.CacheWriteTokens, ReasoningTokens: item.ReasoningTokens,
		RequestCount: item.RequestCount, SuccessCount: item.SuccessCount,
		TTFTSamples: item.TTFTSamples, LatencySamples: item.LatencySamples,
	}
	for _, n := range []int64{m.TotalTokens, m.InputTokens, m.OutputTokens, m.CacheReadTokens, m.CacheWriteTokens,
		m.ReasoningTokens, m.RequestCount, m.SuccessCount, m.TTFTSamples, m.LatencySamples} {
		if n < 0 {
			return Metrics{}, ErrInvalid
		}
	}
	// 三级递进 max 下界兜底，兼容不同上游在缓存、思考与总 Token 口径上的细微偏差。
	cacheSum, ok := addInt(m.CacheReadTokens, m.CacheWriteTokens)
	if !ok {
		return Metrics{}, ErrInvalid
	}
	if m.InputTokens < cacheSum {
		m.InputTokens = cacheSum
	}
	if m.OutputTokens < m.ReasoningTokens {
		m.OutputTokens = m.ReasoningTokens
	}
	classified, ok := addInt(m.InputTokens, m.OutputTokens)
	if !ok {
		return Metrics{}, ErrInvalid
	}
	if m.TotalTokens < classified {
		m.TotalTokens = classified
	}
	// 若存在正用量或正成功数但未填请求数，按至少 1 次或成功数兜底，避免整份快照被拒收。
	if m.RequestCount == 0 && (m.TotalTokens > 0 || m.SuccessCount > 0) {
		m.RequestCount = max(int64(1), m.SuccessCount)
	}
	if m.SuccessCount > m.RequestCount {
		m.SuccessCount = m.RequestCount
	}
	var err error
	if m.TTFTSumMS, m.TTFTSamples, err = normalizeDuration(item.TTFTAvgMS, m.TTFTSamples, m.RequestCount); err != nil {
		return Metrics{}, err
	}
	if m.LatencySumMS, m.LatencySamples, err = normalizeDuration(item.LatencyAvgMS, m.LatencySamples, m.RequestCount); err != nil {
		return Metrics{}, err
	}
	return m, nil
}

// normalizeDuration 校验均值合法性并将样本数钳制在请求数内，保持观测均值不变。
// 样本数为 0 时忽略有限非负均值；样本数大于 0 但均值为 nil 时将样本数归 0。
func normalizeDuration(avg *float64, samples, maxSamples int64) (float64, int64, error) {
	if avg != nil && (math.IsNaN(*avg) || math.IsInf(*avg, 0) || *avg < 0) {
		return 0, 0, ErrInvalid
	}
	if samples > maxSamples {
		samples = maxSamples
	}
	if samples == 0 || avg == nil {
		return 0, 0, nil
	}
	sum := *avg * float64(samples)
	if math.IsInf(sum, 0) || math.IsNaN(sum) {
		return 0, 0, ErrInvalid
	}
	if sum == 0 {
		sum = 0
	}
	return sum, samples, nil
}

func addInt(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}
