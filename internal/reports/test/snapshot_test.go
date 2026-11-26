package test

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/models"
	"cpa-usage-ranking/internal/reports"
	"cpa-usage-ranking/internal/storage"
)

func reportDB(t *testing.T) *storage.DB {
	t.Helper()
	db, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "reports.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := storage.InitSchema(context.Background(), db.Writer); err != nil {
		t.Fatal(err)
	}
	return db
}

func reportRegistry(t *testing.T, db *storage.DB, entries string) *models.Registry {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	data := `{"currency":"USD","price_unit":"per_million_tokens","models":[` + entries + `]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	catalog, err := models.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := models.Register(context.Background(), db.Writer, catalog)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func usage(model string, tokens, requests int64, avg float64) reports.Item {
	return reports.Item{Model: model, TotalTokens: tokens, InputTokens: tokens, RequestCount: requests,
		SuccessCount: requests, TTFTAvgMS: number(avg), TTFTSamples: requests}
}

func TestPreparedDigestAndCatalogAggregation(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":["alias"],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}`)
	a := usage("route/alpha", 10, 2, 3)
	b := usage("beta", 20, 1, 6)
	unknown := usage("unknown", 7, 1, 2)
	zero := reports.Item{Model: "zero"}
	first := reports.Report{Revision: 8, Items: []reports.Item{a, b, unknown, zero}}
	p, err := reports.Prepare(first)
	if err != nil {
		t.Fatal(err)
	}
	reordered, err := reports.Prepare(reports.Report{Revision: 8, Items: []reports.Item{zero, unknown, b, a}})
	if err != nil || p.Digest() != reordered.Digest() {
		t.Fatalf("order changed digest: %v", err)
	}
	rows, err := p.Aggregate(registry)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := registry.CanonicalID("alpha")
	alphaRow := rows[reports.Key{ModelID: id}]
	if len(rows) != 3 || alphaRow.TotalTokens != 10 || alphaRow.RequestCount != 2 || alphaRow.TTFTSumMS != 6 || alphaRow.TTFTSamples != 2 || rows[reports.Key{}].TotalTokens != 30 || rows[reports.Key{}].TTFTSumMS != 12 || rows[reports.Key{}].TTFTSamples != 3 {
		t.Fatalf("bad aggregation: %+v", rows)
	}
	// Caller mutation and current catalog/price changes cannot change the prepared digest.
	*a.TTFTAvgMS = 99
	changed := reportRegistry(t, db, `{"model_name":"alpha","alias":[],"prices":{"default":{"input":"1"}}},{"model_name":"unknown","alias":[],"prices":{}}`)
	if p.Digest() != reordered.Digest() {
		t.Fatal("mutable source changed digest")
	}
	newRows, err := p.Aggregate(changed)
	if err != nil || newRows[reports.Key{}].TotalTokens != 17 {
		t.Fatalf("new catalog not used on new aggregation: %+v %v", newRows, err)
	}
	a.TTFTAvgMS = number(3)
	changedUnknown := unknown
	changedUnknown.TotalTokens++
	changedUnknown.InputTokens++
	other, err := reports.Prepare(reports.Report{Revision: 8, Items: []reports.Item{a, b, changedUnknown, zero}})
	if err != nil || other.Digest() == p.Digest() {
		t.Fatal("unknown item did not affect digest")
	}
	withoutZero, err := reports.Prepare(reports.Report{Revision: 8, Items: []reports.Item{a, b, unknown}})
	if err != nil || withoutZero.Digest() == p.Digest() {
		t.Fatal("zero item did not affect digest")
	}
}

func TestPreparedValidationMergingAndOverflow(t *testing.T) {
	db := reportDB(t)
	registry := reportRegistry(t, db, `{"model_name":"alpha","alias":["alias"],"prices":{}},{"model_name":"beta","alias":[],"prices":{}}`)
	a := usage("alpha", 1, 1, 1)
	for _, items := range [][]reports.Item{
		{a, {Model: string([]byte{0xff})}},
		{a, {Model: "unknown", TotalTokens: -1}},
	} {
		if _, err := reports.Prepare(reports.Report{Items: items}); !errors.Is(err, reports.ErrInvalid) {
			t.Fatalf("accepted invalid input: %+v %v", items, err)
		}
	}
	// 单条异常模型名（空串、尾斜杠、控制字符、超长）在聚合阶段静默跳过，不连坐合法模型。
	mixed, err := reports.Prepare(reports.Report{Revision: 3, Items: []reports.Item{
		a,
		usage("", 5, 1, 1),
		usage("vendor/", 5, 1, 1),
		usage("bad\nname", 5, 1, 1),
		usage(strings.Repeat("x", 129), 5, 1, 1),
	}})
	if err != nil {
		t.Fatalf("mixed report with malformed unknown model rejected: %v", err)
	}
	mixedRows, err := mixed.Aggregate(registry)
	alphaIDCheck, _ := registry.CanonicalID("alpha")
	if err != nil || len(mixedRows) != 2 || mixedRows[reports.Key{ModelID: alphaIDCheck}].TotalTokens != 1 || mixedRows[reports.Key{}].TotalTokens != 1 {
		t.Fatalf("malformed unknown items were not skipped cleanly: %+v %v", mixedRows, err)
	}

	// 同名 Model 多项并存（含大小写与网关后缀）时，任意排列顺序必须生成完全一致的 Digest，且聚合按加法合并。
	dup1 := usage("alpha", 10, 2, 3)
	dup2 := usage("ALPHA-thinking", 20, 3, 8)
	dup3 := usage("route/alpha[1m]:free", 5, 1, 10)
	dup4 := usage("ALIAS(16384)", 15, 2, 4)
	p1, err := reports.Prepare(reports.Report{Revision: 5, Items: []reports.Item{dup1, dup2, dup3, dup4, {Model: "alias"}, usage("alpha-max", 99, 1, 1)}})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := reports.Prepare(reports.Report{Revision: 5, Items: []reports.Item{usage("alpha-max", 99, 1, 1), {Model: "alias"}, dup4, dup2, dup1, dup3}})
	if err != nil {
		t.Fatal(err)
	}
	if p1.Digest() != p2.Digest() {
		t.Fatal("duplicate model reordering changed digest")
	}
	mergedRows, err := p1.Aggregate(registry)
	if err != nil {
		t.Fatal(err)
	}
	alphaID, _ := registry.CanonicalID("alpha")
	merged := mergedRows[reports.Key{ModelID: alphaID}]
	// tokens: 10+20+5+15=50, req: 2+3+1+2=8, ttft_sum: 6+24+10+8=48, ttft_samples: 8
	if len(mergedRows) != 2 || merged.TotalTokens != 50 || merged.RequestCount != 8 || merged.TTFTSumMS != 48 || merged.TTFTSamples != 8 || mergedRows[reports.Key{}] != merged {
		t.Fatalf("duplicate model merge mismatch: %+v", mergedRows)
	}

	// 合并溢出或跨模型总计溢出仍必须返回 ErrInvalid。
	for _, pair := range [][]reports.Item{
		{usage("alpha", math.MaxInt64, 1, 1), usage("alias", 1, 1, 1)},
		{usage("alpha", math.MaxInt64, 1, 1), usage("beta", 1, 1, 1)},
		{usage("alpha", 1, 1, math.MaxFloat64), usage("beta", 1, 1, math.MaxFloat64)},
		{usage("alpha", 1, 1, math.MaxFloat64), usage("alias", 1, 1, math.MaxFloat64)},
	} {
		prepared, err := reports.Prepare(reports.Report{Items: pair})
		if err != nil {
			t.Fatalf("individual valid items rejected: %v", err)
		}
		if _, err := prepared.Aggregate(registry); !errors.Is(err, reports.ErrInvalid) {
			t.Fatalf("aggregation overflow accepted: %v", err)
		}
	}
	p, err := reports.Prepare(reports.Report{Items: []reports.Item{usage("unknown", 1, 1, 1), {Model: "alpha"}}})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := p.Aggregate(registry)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unknown/zero produced rows: %+v %v", rows, err)
	}
}

func TestEquivalentNumericSpellingsHaveSameDigest(t *testing.T) {
	first, err := reports.Decode([]byte(validPayload))
	if err != nil {
		t.Fatal(err)
	}
	second, err := reports.Decode([]byte(validPayload))
	if err != nil {
		t.Fatal(err)
	}
	first.Items[0].LatencyAvgMS = number(math.Copysign(0, -1))
	p, err := reports.Prepare(first)
	if err != nil {
		t.Fatal(err)
	}
	q, err := reports.Prepare(second)
	if err != nil || p.Digest() != q.Digest() {
		t.Fatalf("signed zero changed digest: %v", err)
	}
}
