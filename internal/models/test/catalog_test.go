package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/models"
)

func loadCatalog(t *testing.T, content string) (*models.Catalog, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return models.Load(path)
}

func catalogJSON(items ...string) string {
	return `{"currency":"USD","price_unit":"per_million_tokens","models":[` + strings.Join(items, ",") + `]}`
}

func item(name string, aliases []string, prices string) string {
	n, _ := json.Marshal(name)
	a, _ := json.Marshal(aliases)
	return `{"model_name":` + string(n) + `,"alias":` + string(a) + `,"prices":` + prices + `}`
}

func TestShippedCatalogLoads(t *testing.T) {
	catalog, err := models.Load(filepath.Join("..", "..", "..", "config", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	entries := catalog.List()
	aliases := 0
	for _, entry := range entries {
		aliases += len(entry.Aliases)
	}
	if len(entries) != 105 || aliases != 96 {
		t.Fatalf("shipped seed: %d models, %d aliases", len(entries), aliases)
	}
}

func TestPricesPreserveUnknownFreeAndPrecision(t *testing.T) {
	long := "123456789012345678901234567890.000000000000000000000000000123"
	catalog, err := loadCatalog(t, catalogJSON(item("Model", []string{"model"}, `{"default":{"input":"0","output":null,"cache_read":"`+long+`"},"flex":{"output":"1.25"}}`)))
	if err != nil {
		t.Fatal(err)
	}
	entry := catalog.List()[0]
	price := entry.Prices[models.Default]
	if price.Input == nil || *price.Input != "0" || price.Output != nil || price.CacheWrite != nil || price.CacheRead == nil || *price.CacheRead != long {
		t.Fatalf("price not preserved: %+v", price)
	}
	if _, ok := entry.Prices[models.Priority]; ok {
		t.Fatal("missing priority inherited a price")
	}
	if *entry.Prices[models.Flex].Output != "1.25" {
		t.Fatal("flex price not preserved")
	}
	// 返回值中的别名、map 和价格指针不能改写已校验目录。
	entry.Aliases[0] = "changed"
	*price.Input = "42"
	delete(entry.Prices, models.Default)
	copy := catalog.List()[0]
	if copy.Aliases[0] != "model" || *copy.Prices[models.Default].Input != "0" {
		t.Fatal("List exposed mutable catalog state")
	}
}

func TestInvalidCatalogsFail(t *testing.T) {
	valid := item("alpha", []string{}, `{}`)
	cases := map[string]string{
		"broken JSON": `{`, "multiple JSON values": catalogJSON(valid) + ` {}`,
		"no models":                 `{"currency":"USD","price_unit":"per_million_tokens"}`,
		"null models":               `{"currency":"USD","price_unit":"per_million_tokens","models":null}`,
		"duplicate standard":        catalogJSON(valid, valid),
		"case duplicate standard":   catalogJSON(item("Alpha", []string{}, `{}`), item("ALPHA", []string{}, `{}`)),
		"alias vs later standard":   catalogJSON(item("alpha", []string{"beta"}, `{}`), item("beta", []string{}, `{}`)),
		"case alias vs standard":    catalogJSON(item("alpha", []string{"BETA"}, `{}`), item("beta", []string{}, `{}`)),
		"alias vs earlier standard": catalogJSON(item("beta", []string{}, `{}`), item("alpha", []string{"beta"}, `{}`)),
		"alias vs alias":            catalogJSON(item("alpha", []string{"shared"}, `{}`), item("beta", []string{"shared"}, `{}`)),
		"case alias vs alias":       catalogJSON(item("alpha", []string{"Shared"}, `{}`), item("beta", []string{"SHARED"}, `{}`)),
		"numeric price":             catalogJSON(item("alpha", []string{}, `{"default":{"input":0}}`)),
		"negative price":            catalogJSON(item("alpha", []string{}, `{"default":{"input":"-0.1"}}`)),
		"exponent price":            catalogJSON(item("alpha", []string{}, `{"default":{"input":"1e2"}}`)),
		"decimal missing digits":    catalogJSON(item("alpha", []string{}, `{"default":{"input":"1."}}`)),
		"invalid UTF-8":             catalogJSON(`{"model_name":"` + string([]byte{0xff}) + `","alias":[],"prices":{}}`),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadCatalog(t, input); err == nil {
				t.Fatal("invalid catalog was accepted")
			}
		})
	}
	if _, err := models.Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestRelaxedCatalogMetadataAndDefaults(t *testing.T) {
	raw := `{
		"currency": "EUR",
		"price_unit": "per_1k_tokens",
		"extra_top_level": true,
		"models": [
			{"model_name": "minimal"},
			{"model_name": "null-optional", "alias": null, "prices": null, "extra_item": "ignored"},
			{
				"model_name": "relaxed-prices",
				"alias": ["rp-1"],
				"prices": {
					"default": null,
					"unknown_tier": {"input": "not-a-decimal"},
					"flex": {"output": "2.50", "extra_field": 123}
				}
			},
			{"model_name": "a-\ud800", "alias": ["b-\udc00"]}
		]
	}`
	catalog, err := loadCatalog(t, raw)
	if err != nil {
		t.Fatalf("relaxed catalog rejected: %v", err)
	}
	entries := catalog.List()
	if len(entries) != 4 {
		t.Fatalf("expected 4 models, got %d", len(entries))
	}
	if len(entries[0].Aliases) != 0 || len(entries[0].Prices) != 0 {
		t.Fatalf("minimal entry: %+v", entries[0])
	}
	if len(entries[1].Aliases) != 0 || len(entries[1].Prices) != 0 {
		t.Fatalf("null-optional entry: %+v", entries[1])
	}
	if len(entries[2].Prices) != 1 || entries[2].Prices[models.Flex].Output == nil || *entries[2].Prices[models.Flex].Output != "2.50" {
		t.Fatalf("relaxed-prices entry: %+v", entries[2].Prices)
	}
	if entries[3].Name != "a-\ufffd" || len(entries[3].Aliases) != 1 || entries[3].Aliases[0] != "b-\ufffd" {
		t.Fatalf("expected replacement rune for lone surrogates, got %+v", entries[3])
	}
	// 顶层省略 currency 与 price_unit 也应正常加载。
	if _, err := loadCatalog(t, `{"models":[{"model_name":"omitted-meta"}]}`); err != nil {
		t.Fatalf("omitted metadata rejected: %v", err)
	}
}

func TestCatalogNameBoundaries(t *testing.T) {
	for _, name := range []string{"", " alpha", "alpha\u00a0", "a/b", `a\b`, "a\n", "a\u0085b", strings.Repeat("a", 129), strings.Repeat("界", 43)} {
		for _, alias := range []bool{false, true} {
			t.Run(name+"/alias="+map[bool]string{true: "yes", false: "no"}[alias], func(t *testing.T) {
				entry := item(name, []string{}, `{}`)
				if alias {
					entry = item("alpha", []string{name}, `{}`)
				}
				if _, err := loadCatalog(t, catalogJSON(entry)); err == nil {
					t.Fatalf("accepted invalid name %q", name)
				}
			})
		}
	}
	for _, name := range []string{strings.Repeat("a", 128), strings.Repeat("界", 42) + "ab", "模型", "�"} {
		if _, err := loadCatalog(t, catalogJSON(item(name, []string{}, `{}`))); err != nil {
			t.Fatalf("valid name %q: %v", name, err)
		}
	}
	if _, err := loadCatalog(t, catalogJSON(`{"model_name":"\ud83d\ude00","alias":[],"prices":{}}`)); err != nil {
		t.Fatalf("valid surrogate pair: %v", err)
	}
	// 相同模型内的大小写变体别名允许共存，且保留标准名原始大小写；跨模型大小写冲突必须被拒绝。
	sameModel, err := loadCatalog(t, catalogJSON(item("Alpha", []string{"alpha"}, `{}`)))
	if err != nil || sameModel.List()[0].Name != "Alpha" {
		t.Fatalf("same-model case alias: %v", err)
	}
	if _, err := loadCatalog(t, catalogJSON(item("Alpha", []string{}, `{}`), item("ALPHA", []string{}, `{}`))); err == nil {
		t.Fatal("cross-model case-insensitive conflict was accepted")
	}
}
