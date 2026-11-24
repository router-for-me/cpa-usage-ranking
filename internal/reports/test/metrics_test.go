package test

import (
	"errors"
	"math"
	"testing"

	"cpa-usage-ranking/internal/reports"
)

func number(v float64) *float64 { return &v }

func TestValidateMetrics(t *testing.T) {
	base := reports.Item{Model: "unknown", TotalTokens: 20, InputTokens: 12, OutputTokens: 6,
		CacheReadTokens: 4, CacheWriteTokens: 3, ReasoningTokens: 2, RequestCount: 3, SuccessCount: 2,
		TTFTAvgMS: number(0), TTFTSamples: 1, LatencyAvgMS: number(1.5), LatencySamples: 3}
	if err := reports.Validate(reports.Report{Items: []reports.Item{base, {}}}); err != nil {
		t.Fatalf("valid metrics: %v", err)
	}
	max := int64(math.MaxInt64)
	boundary := reports.Item{TotalTokens: max, InputTokens: max, CacheReadTokens: max, RequestCount: max,
		TTFTAvgMS: number(1), TTFTSamples: max}
	if err := reports.Validate(reports.Report{Revision: max, Items: []reports.Item{boundary}}); err != nil {
		t.Fatalf("max int64 rejected: %v", err)
	}
	cases := map[string]func(*reports.Item){
		"negative token":          func(i *reports.Item) { i.TotalTokens = -1 },
		"negative count":          func(i *reports.Item) { i.SuccessCount = -1 },
		"cache sum overflow":      func(i *reports.Item) { i.CacheReadTokens = max; i.CacheWriteTokens = 1 },
		"input output overflow":   func(i *reports.Item) { i.InputTokens = max; i.OutputTokens = 1; i.TotalTokens = max },
		"clamped sum overflow":    func(i *reports.Item) { i.CacheReadTokens = max; i.OutputTokens = 1; i.InputTokens = 0 },
		"negative average":        func(i *reports.Item) { i.LatencyAvgMS = number(-1) },
		"zero sample negative":    func(i *reports.Item) { i.TTFTSamples = 0; i.TTFTAvgMS = number(-0.5) },
		"zero sample nan":         func(i *reports.Item) { i.TTFTSamples = 0; i.TTFTAvgMS = number(math.NaN()) },
		"nan average":             func(i *reports.Item) { i.TTFTAvgMS = number(math.NaN()) },
		"positive infinity":       func(i *reports.Item) { i.LatencyAvgMS = number(math.Inf(1)) },
		"negative infinity":       func(i *reports.Item) { i.TTFTAvgMS = number(math.Inf(-1)) },
		"multiplication overflow": func(i *reports.Item) { i.LatencyAvgMS = number(math.MaxFloat64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := base
			mutate(&bad)
			if err := reports.Validate(reports.Report{Items: []reports.Item{base, bad}}); !errors.Is(err, reports.ErrInvalid) {
				t.Fatalf("invalid item accepted: %v", err)
			}
		})
	}
	if err := reports.Validate(reports.Report{Revision: -1}); !errors.Is(err, reports.ErrInvalid) {
		t.Fatalf("negative revision accepted: %v", err)
	}
}

func TestNormalizeInconsistentMetrics(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{}}`)
	// 缓存和超输入、思考超输出、输入+输出超总计、零请求数但有正用量、样本数超请求数、零样本带非空均值、正样本带 nil 均值。
	item := reports.Item{
		Model:            "alpha",
		TotalTokens:      5,
		InputTokens:      4,
		OutputTokens:     2,
		CacheReadTokens:  6,
		CacheWriteTokens: 4,
		ReasoningTokens:  5,
		RequestCount:     0,
		SuccessCount:     3,
		TTFTAvgMS:        number(120.5),
		TTFTSamples:      10,
		LatencyAvgMS:     nil,
		LatencySamples:   2,
	}
	p, err := reports.Prepare(reports.Report{Revision: 1, Items: []reports.Item{item}})
	if err != nil {
		t.Fatalf("inconsistent item rejected: %v", err)
	}
	rows, err := p.Aggregate(registry)
	if err != nil {
		t.Fatal(err)
	}
	got := rows[reports.Key{}]
	// input = max(4, 6+4) = 10; output = max(2, 5) = 5; total = max(5, 10+5) = 15
	// request_count = max(1, success=3) = 3; success = 3
	// ttft_samples clamped to 3 keeping mean 120.5 => sum = 361.5; latency avg nil => samples=0, sum=0
	if got.InputTokens != 10 || got.OutputTokens != 5 || got.TotalTokens != 15 ||
		got.RequestCount != 3 || got.SuccessCount != 3 ||
		got.TTFTSamples != 3 || got.TTFTSumMS != 361.5 ||
		got.LatencySamples != 0 || got.LatencySumMS != 0 {
		t.Fatalf("unexpected normalized metrics: %+v", got)
	}

	// 正用量且 success=0、request_count=0 时 request_count 兜底为 1；success 超 request_count 时钳制；零样本带有限均值被忽略。
	second := reports.Item{
		Model:          "alpha",
		TotalTokens:    10,
		RequestCount:   2,
		SuccessCount:   5,
		TTFTAvgMS:      number(0),
		TTFTSamples:    0,
		LatencyAvgMS:   number(45),
		LatencySamples: 0,
	}
	p2, err := reports.Prepare(reports.Report{Revision: 2, Items: []reports.Item{second}})
	if err != nil {
		t.Fatalf("second inconsistent item rejected: %v", err)
	}
	rows2, err := p2.Aggregate(registry)
	if err != nil {
		t.Fatal(err)
	}
	got2 := rows2[reports.Key{}]
	if got2.RequestCount != 2 || got2.SuccessCount != 2 || got2.TTFTSamples != 0 || got2.TTFTSumMS != 0 || got2.LatencySamples != 0 || got2.LatencySumMS != 0 {
		t.Fatalf("unexpected clamped metrics: %+v", got2)
	}

	// 仅有正总 Token 且无请求数与成功数时，请求数兜底为 1。
	third := reports.Item{Model: "alpha", TotalTokens: 42}
	p3, err := reports.Prepare(reports.Report{Revision: 3, Items: []reports.Item{third}})
	if err != nil {
		t.Fatal(err)
	}
	rows3, err := p3.Aggregate(registry)
	if err != nil {
		t.Fatal(err)
	}
	if rows3[reports.Key{}].RequestCount != 1 || rows3[reports.Key{}].TotalTokens != 42 {
		t.Fatalf("token-only item not normalized: %+v", rows3[reports.Key{}])
	}
}
