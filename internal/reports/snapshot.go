package reports

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"math"
	"slices"
	"unicode/utf8"

	"cpa-usage-ranking/internal/models"
)

// Prepared is a validated, immutable-by-caller snapshot of the complete
// input. Digesting it does not depend on any catalog, price or database.
type Prepared struct {
	revision int64
	items    []Item
	digest   [sha256.Size]byte
	valid    bool
}

func (p Prepared) Revision() int64           { return p.revision }
func (p Prepared) Digest() [sha256.Size]byte { return p.digest }

// Prepare performs only catalog-independent checks. In particular, it must
// succeed before looking up the current revision, without aggregating models.
func Prepare(report Report) (Prepared, error) {
	if err := Validate(report); err != nil {
		return Prepared{}, err
	}
	items := make([]Item, len(report.Items))
	copy(items, report.Items)
	for i := range items {
		item := &items[i]
		if !utf8.ValidString(item.Model) {
			return Prepared{}, ErrInvalid
		}
		// Copy pointers and normalize signed zero, so semantically equal JSON
		// numeric spellings and later caller mutations cannot change the input.
		if item.TTFTAvgMS != nil {
			v := *item.TTFTAvgMS
			if v == 0 {
				v = 0
			}
			item.TTFTAvgMS = &v
		}
		if item.LatencyAvgMS != nil {
			v := *item.LatencyAvgMS
			if v == 0 {
				v = 0
			}
			item.LatencyAvgMS = &v
		}
	}
	// 允许同名 Model 多项并存，按全部字段确定性全序排列，确保乱序重发时 Digest() 完全一致。
	slices.SortFunc(items, compareItem)
	encoded, err := json.Marshal(struct {
		SchemaVersion int
		Revision      int64
		Items         []Item
	}{1, report.Revision, items})
	if err != nil {
		return Prepared{}, ErrInvalid
	}
	return Prepared{revision: report.Revision, items: items, digest: sha256.Sum256(encoded), valid: true}, nil
}

func compareItem(a, b Item) int {
	if c := cmp.Compare(a.Model, b.Model); c != 0 {
		return c
	}
	if c := cmp.Compare(a.TotalTokens, b.TotalTokens); c != 0 {
		return c
	}
	if c := cmp.Compare(a.InputTokens, b.InputTokens); c != 0 {
		return c
	}
	if c := cmp.Compare(a.OutputTokens, b.OutputTokens); c != 0 {
		return c
	}
	if c := cmp.Compare(a.CacheReadTokens, b.CacheReadTokens); c != 0 {
		return c
	}
	if c := cmp.Compare(a.CacheWriteTokens, b.CacheWriteTokens); c != 0 {
		return c
	}
	if c := cmp.Compare(a.ReasoningTokens, b.ReasoningTokens); c != 0 {
		return c
	}
	if c := cmp.Compare(a.RequestCount, b.RequestCount); c != 0 {
		return c
	}
	if c := cmp.Compare(a.SuccessCount, b.SuccessCount); c != 0 {
		return c
	}
	if c := cmp.Compare(a.TTFTSamples, b.TTFTSamples); c != 0 {
		return c
	}
	if c := cmp.Compare(a.LatencySamples, b.LatencySamples); c != 0 {
		return c
	}
	if c := compareOptionalFloat(a.TTFTAvgMS, b.TTFTAvgMS); c != 0 {
		return c
	}
	return compareOptionalFloat(a.LatencyAvgMS, b.LatencyAvgMS)
}

func compareOptionalFloat(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	default:
		return cmp.Compare(*a, *b)
	}
}

// Key identifies a stored row. ModelID 0 is reserved for the all-model total;
// real catalog model IDs are positive.
type Key struct {
	ModelID int64
}

// Aggregate uses the current registry only for a new replacement. Valid
// unknown and zero-execution items remain in the digest but not the rows.
// Duplicate names or aliases resolving to the same canonical model are merged additively.
func (p Prepared) Aggregate(registry *models.Registry) (map[Key]Metrics, error) {
	rows := make(map[Key]Metrics)
	for _, item := range p.items {
		resolved, err := registry.Resolve(item.Model)
		if err != nil || !resolved.Matched {
			// 单条陌生或异常模型名静默跳过，避免整日合法模型用量被连坐拒收。
			continue
		}
		metric, err := itemMetrics(item)
		if err != nil {
			return nil, ErrInvalid
		}
		if metric.RequestCount == 0 {
			continue
		}
		key := Key{ModelID: resolved.ID}
		if existing, ok := rows[key]; ok {
			merged, ok := addMetrics(existing, metric)
			if !ok {
				return nil, ErrInvalid
			}
			rows[key] = merged
		} else {
			rows[key] = metric
		}
	}
	if len(rows) == 0 {
		return rows, nil
	}
	// 按模型 ID 固定顺序累加全模型总计，消除 map 遍历随机顺序对浮点耗时和的微小影响。
	keys := make([]int64, 0, len(rows))
	for key := range rows {
		keys = append(keys, key.ModelID)
	}
	slices.Sort(keys)
	var total Metrics
	for _, id := range keys {
		var ok bool
		total, ok = addMetrics(total, rows[Key{ModelID: id}])
		if !ok {
			return nil, ErrInvalid
		}
	}
	if total.RequestCount != 0 {
		rows[Key{}] = total
	}
	return rows, nil
}

// AddMetrics 合并非负原始计数及耗时总和；溢出或非有限值不会产生部分结果。
func AddMetrics(a, b Metrics) (Metrics, bool) {
	return addMetrics(a, b)
}

func addMetrics(a, b Metrics) (Metrics, bool) {
	if a.TTFTSumMS < 0 || b.TTFTSumMS < 0 || a.LatencySumMS < 0 || b.LatencySumMS < 0 {
		return Metrics{}, false
	}
	pairs := []struct {
		dst   *int64
		value int64
	}{
		{&a.TotalTokens, b.TotalTokens}, {&a.InputTokens, b.InputTokens}, {&a.OutputTokens, b.OutputTokens},
		{&a.CacheReadTokens, b.CacheReadTokens}, {&a.CacheWriteTokens, b.CacheWriteTokens}, {&a.ReasoningTokens, b.ReasoningTokens},
		{&a.RequestCount, b.RequestCount}, {&a.SuccessCount, b.SuccessCount},
		{&a.TTFTSamples, b.TTFTSamples}, {&a.LatencySamples, b.LatencySamples},
	}
	for _, pair := range pairs {
		var ok bool
		*pair.dst, ok = addInt(*pair.dst, pair.value)
		if !ok {
			return Metrics{}, false
		}
	}
	a.TTFTSumMS += b.TTFTSumMS
	a.LatencySumMS += b.LatencySumMS
	if math.IsInf(a.TTFTSumMS, 0) || math.IsNaN(a.TTFTSumMS) || math.IsInf(a.LatencySumMS, 0) || math.IsNaN(a.LatencySumMS) {
		return Metrics{}, false
	}
	return a, true
}
