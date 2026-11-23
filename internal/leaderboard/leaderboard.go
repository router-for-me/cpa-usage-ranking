// Package leaderboard 从有界、已结束读事务的公开候选构建固定榜单。
package leaderboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/bits"
	"sort"
	"strconv"

	"cpa-usage-ranking/internal/cache"
	"cpa-usage-ranking/internal/periods"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/statistics"
)

const (
	Overall = "overall"
	Usage   = "usage"
	Cache   = "cache"
	Success = "success"
	TTFT    = "ttft"
	Limit   = 100
)

// Entry 是唯一公开条目白名单。指标和 ID 使用十进制字符串，避免 JavaScript 丢失 int64 精度。
type Entry struct {
	Rank        int             `json:"rank"`
	ClientID    string          `json:"client_id"`
	DisplayName string          `json:"display_name"`
	AvatarID    string          `json:"avatar_id"`
	Metric      string          `json:"metric"`
	Details     *OverallDetails `json:"details,omitempty"`
}

type candidate struct {
	id      int64
	profile statistics.Profile
	metrics reports.Metrics
	derived statistics.Derived
	wilson  float64
	score   int64 // 官方百分之一分
	value   float64
}

// Items 是全部六个固定周期；每次读一次原始候选，构建该周期的五榜。
func Items() []cache.Item {
	return []cache.Item{
		{ID: "today", Kind: periods.Day, Public: true, Encode: encode},
		{ID: "yesterday", Kind: periods.Day, DayOffset: -1, Public: true, Encode: encode},
		{ID: "week", Kind: periods.Week, CompletePeriod: true, Public: true, Encode: encode},
		{ID: "month", Kind: periods.Month, CompletePeriod: true, Public: true, Encode: encode},
		{ID: "year", Kind: periods.Year, Public: true, Encode: encode},
		{ID: "all", Kind: periods.All, Public: true, Encode: encode},
	}
}

func encode(ctx context.Context, s statistics.Snapshot, w io.Writer) error {
	boards, err := Build(ctx, s)
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(boards)
}

// Build 使用全部基础候选计算百分位及中位数，最后才截断各榜。
func Build(ctx context.Context, s statistics.Snapshot) (map[string][]Entry, error) {
	all := make([]*candidate, 0, len(s.Rows))
	positive := make([]int64, 0, len(s.Rows))
	means := make([]float64, 0, len(s.Rows))
	for id, m := range s.Rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p, ok := s.Profiles[id]
		if !ok {
			return nil, errors.New("missing public profile")
		}
		d, err := statistics.Derive(m)
		if err != nil && !errors.Is(err, statistics.ErrOverflow) {
			return nil, err
		}
		if m.RequestCount == 0 {
			continue
		}
		c := &candidate{id: id, profile: p, metrics: m, derived: d, wilson: wilson(m.SuccessCount, m.RequestCount)}
		all = append(all, c)
		if m.TotalTokens > 0 {
			positive = append(positive, m.TotalTokens)
		}
		if d.TTFTMeanMS.Valid && d.TTFTMeanMS.Value > 0 {
			means = append(means, d.TTFTMeanMS.Value)
		}
	}
	sort.Slice(positive, func(i, j int) bool { return positive[i] < positive[j] })
	sort.Float64s(means)
	var median float64
	if len(means) > 0 {
		median = means[len(means)/2]
		if len(means)%2 == 0 {
			lower := means[len(means)/2-1]
			median = lower + (median-lower)/2
		}
	}
	var reference *big.Rat
	if median > 0 {
		reference = new(big.Rat).SetFloat64(median)
	}
	for _, c := range all {
		m, d := c.metrics, c.derived
		if m.TotalTokens > 0 {
			lo := sort.Search(len(positive), func(i int) bool { return positive[i] >= m.TotalTokens })
			hi := sort.Search(len(positive), func(i int) bool { return positive[i] > m.TotalTokens })
			// 40 分 × 中间百分位，直接用整数 half-up 到百分之一分。
			numerator, denominator := int64(2000*(lo+hi)), int64(len(positive))
			c.score += (2*numerator + denominator) / (2 * denominator)
		}
		c.score += int64(math.Round(3000 * c.wilson))
		if d.CacheReadRate.Valid {
			c.score += int64(roundRatio(m.CacheReadTokens, m.InputTokens, 2000))
		}
		if d.TTFTMeanMS.Valid {
			c.score += responseContribution(reference, d.TTFTMeanMS.Value, m.TTFTSamples, m.RequestCount)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	boards := make(map[string][]Entry, 5)
	for _, kind := range []string{Overall, Usage, Cache, Success, TTFT} {
		rows := make([]*candidate, 0, len(all))
		for _, c := range all {
			switch kind {
			case Usage:
				if c.metrics.TotalTokens == 0 {
					continue
				}
			case Cache:
				if !c.derived.CacheReadRate.Valid {
					continue
				}
				c.value = float64(roundRatio(c.metrics.CacheReadTokens, c.metrics.InputTokens, 10000)) / 100
			case Success:
				c.value = round2(c.wilson * 100)
			case TTFT:
				if !c.derived.TTFTMeanMS.Valid {
					continue
				}
				c.value = round2(c.derived.TTFTMeanMS.Value)
			}
			rows = append(rows, c)
		}
		compare := func(a, b *candidate) int {
			switch kind {
			case Overall:
				return cmpInt(b.score, a.score)
			case Usage:
				return cmpInt(b.metrics.TotalTokens, a.metrics.TotalTokens)
			case TTFT:
				return cmpFloat(a.value, b.value)
			default:
				return cmpFloat(b.value, a.value)
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if order := compare(rows[i], rows[j]); order != 0 {
				return order < 0
			}
			return rows[i].id < rows[j].id
		})
		entries := make([]Entry, 0, min(len(rows), Limit))
		rank := 0
		for i, c := range rows {
			if i == Limit {
				break
			}
			if i == 0 || compare(rows[i-1], c) != 0 {
				rank = i + 1
			}
			var metric string
			switch kind {
			case Overall:
				metric = fmt.Sprintf("%d.%02d", c.score/100, c.score%100)
			case Usage:
				metric = strconv.FormatInt(c.metrics.TotalTokens, 10)
			default:
				metric = strconv.FormatFloat(c.value, 'f', 2, 64)
			}
			entry := Entry{Rank: rank, ClientID: strconv.FormatInt(c.id, 10), DisplayName: c.profile.DisplayName, AvatarID: strconv.FormatInt(c.profile.AvatarID, 10), Metric: metric}
			if kind == Overall {
				entry.Details = overallDetails(c)
			}
			entries = append(entries, entry)
		}
		boards[kind] = entries
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return boards, nil
}

func wilson(success, requests int64) float64 {
	if requests <= 0 {
		return 0
	}
	success = min(max(int64(0), success), requests)
	const z = 1.959963984540054
	n := float64(requests)
	p := float64(success) / n
	z2 := z * z
	lower := (p + z2/(2*n) - z*math.Sqrt(p*(1-p)/n+z2/(4*n*n))) / (1 + z2/n)
	return math.Max(0, math.Min(1, lower))
}

// 以已计算的有限 float64 均值为输入，用有理数合并表现和精确覆盖率，
// 避免多次浮点乘除将恰好半分的贡献舍低。无正参照时取中性 0.5。
func responseContribution(reference *big.Rat, mean float64, samples, requests int64) int64 {
	if requests <= 0 {
		return 0
	}
	samples = min(max(int64(0), samples), requests)
	if samples == 0 {
		return 0
	}
	if reference == nil {
		return int64(roundRatio(samples, requests, 500))
	}
	if mean == 0 {
		return int64(roundRatio(samples, requests, 1000))
	}
	measured := new(big.Rat).SetFloat64(mean)
	if measured.Cmp(reference) == 0 {
		return int64(roundRatio(samples, requests, 500))
	}
	divisor := new(big.Rat).Add(reference, measured)
	points := new(big.Rat).Quo(reference, divisor)
	points.Mul(points, new(big.Rat).SetFrac(big.NewInt(samples), big.NewInt(requests)))
	points.Mul(points, big.NewRat(1000, 1))
	// 非负数 half-up：floor((2*numerator + denominator)/(2*denominator))。
	numerator := new(big.Int).Lsh(points.Num(), 1)
	numerator.Add(numerator, points.Denom())
	denominator := new(big.Int).Lsh(points.Denom(), 1)
	return numerator.Quo(numerator, denominator).Int64()
}

// 原始计数是 int64；内部先对越界比例做安全钳制，再用 128 位中间值保留精确半分并杜绝 bits.Div64 商超界 panic。
func roundRatio(numerator, denominator int64, multiplier uint64) uint64 {
	if denominator <= 0 || numerator <= 0 {
		return 0
	}
	if numerator >= denominator {
		return multiplier
	}
	d := uint64(denominator)
	hi, lo := bits.Mul64(uint64(numerator), multiplier)
	lo, carry := bits.Add64(lo, d/2, 0)
	hi += carry
	result, _ := bits.Div64(hi, lo, d)
	return result
}

// 排序与展示均使用同一个两位小数值；极大的有限 TTFT 避免乘法溢出。
func round2(v float64) float64 {
	if v > math.MaxFloat64/100 {
		return v
	}
	return math.Round(v*100) / 100
}
func cmpInt(a, b int64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
func cmpFloat(a, b float64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
