package test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/models"
)

func TestResolveExactNamesAliasesAndPaths(t *testing.T) {
	db := openModelDB(t, filepath.Join(t.TempDir(), "ranking.db"))
	max := strings.Repeat("x", 128)
	registry := register(t, db,
		item("Model", []string{"model-20261001"}, `{}`),
		item("Model-High", []string{}, `{}`),
		item("é", []string{}, `{}`),
		item(max, []string{}, `{}`),
	)
	id := modelID(t, registry, "Model")
	highID := modelID(t, registry, "Model-High")
	cases := []struct {
		raw, normalized, canonical string
		id                         int64
	}{
		{"model", "model", "Model", id},
		{"\u00a0model  ", "model", "Model", id},
		{"\tmodel\n", "model", "Model", id},
		{"provider/route/model", "model", "Model", id},
		{strings.Repeat("prefix/", 25) + "model", "model", "Model", id},
		{"/provider//MODEL", "MODEL", "Model", id},
		{"vendor/MODEL-20261001", "MODEL-20261001", "Model", id},
		{"Model", "Model", "Model", id},
		{"MoDeL", "MoDeL", "Model", id},
		// 精确命中优先于后缀剥离。
		{"model-high", "model-high", "Model-High", highID},
		{"vendor/model-high[1m]", "model-high[1m]", "Model-High", highID},
		// 不剥离日期后缀、-max、-latest 及非白名单后缀。
		{"vendor/model-20261002", "model-20261002", "", 0},
		{"model-max", "model-max", "", 0},
		{"model-latest", "model-latest", "", 0},
		{"vendor/unknown", "unknown", "", 0},
		{"vendor/ model", " model", "", 0},
		{"e\u0301", "e\u0301", "", 0},
		{`vendor\model`, `vendor\model`, "", 0},
		{max, max, max, modelID(t, registry, max)},
	}
	var before int
	if err := db.Reader.QueryRow("SELECT COUNT(*) FROM models").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			result, err := registry.Resolve(tc.raw)
			if err != nil || result.NormalizedName != tc.normalized || result.CanonicalName != tc.canonical || result.ID != tc.id || result.Matched != (tc.id != 0) {
				t.Fatalf("Resolve(%q) = %+v, %v", tc.raw, result, err)
			}
		})
	}
	var after int
	if err := db.Reader.QueryRow("SELECT COUNT(*) FROM models").Scan(&after); err != nil || after != before {
		t.Fatalf("lookups changed dictionary: before=%d after=%d err=%v", before, after, err)
	}
}

func TestResolveRejectsMalformedRawNames(t *testing.T) {
	db := openModelDB(t, filepath.Join(t.TempDir(), "ranking.db"))
	registry := register(t, db, item("model", []string{}, `{}`))
	for _, raw := range []string{
		"", "    ", "/", "vendor/", " vendor/  ",
		"mo\ndel", "mod\tel", "vendor/\x00model", "vendor/\u0085model",
		string([]byte{'x', '/', 0xff}), strings.Repeat("a", 129), strings.Repeat("界", 43),
		"x/" + strings.Repeat("a", 129),
	} {
		t.Run(raw, func(t *testing.T) {
			if result, err := registry.Resolve(raw); !errors.Is(err, models.ErrInvalidReportedName) || result.Matched {
				t.Fatalf("Resolve(%q) = %+v, %v; expected format error", raw, result, err)
			}
		})
	}
}

func TestResolveUsesCurrentCatalogNotHistoricalRows(t *testing.T) {
	db := openModelDB(t, filepath.Join(t.TempDir(), "ranking.db"))
	old := item("alpha", []string{"old-alias"}, `{}`)
	beta := item("beta", []string{"beta-alias"}, `{}`)
	first := register(t, db, old, beta)
	alphaID, betaID := modelID(t, first, "alpha"), modelID(t, first, "beta")
	changed := register(t, db, item("alpha", []string{"new-alias"}, `{}`))
	for _, raw := range []string{"old-alias", "beta", "beta-alias"} {
		result, err := changed.Resolve("vendor/" + raw)
		if err != nil || result.Matched || result.ID != 0 {
			t.Fatalf("historical name %q was treated as current: %+v, %v", raw, result, err)
		}
	}
	resolved, err := changed.Resolve("vendor/new-alias")
	if err != nil || !resolved.Matched || resolved.ID != alphaID {
		t.Fatalf("changed alias lost canonical ID: %+v, %v", resolved, err)
	}
	reintroduced := register(t, db, item("alpha", []string{"new-alias"}, `{}`), beta)
	resolved, err = reintroduced.Resolve("x/beta-alias")
	if err != nil || !resolved.Matched || resolved.ID != betaID {
		t.Fatalf("reintroduced alias lost historical canonical ID: %+v, %v", resolved, err)
	}
}

func TestResolveStripsControlledGatewaySuffixes(t *testing.T) {
	db := openModelDB(t, filepath.Join(t.TempDir(), "ranking.db"))
	registry := register(t, db,
		item("claude-sonnet-4-5", []string{"claude-sonnet-4-5-20250929"}, `{}`),
		item("gpt-5", []string{}, `{}`),
	)
	claudeID := modelID(t, registry, "claude-sonnet-4-5")
	gptID := modelID(t, registry, "gpt-5")

	matched := []struct {
		raw  string
		want int64
	}{
		// 冒号后缀
		{"claude-sonnet-4-5:free", claudeID},
		{"claude-sonnet-4-5:THINKING", claudeID},
		{"claude-sonnet-4-5:online", claudeID},
		{"claude-sonnet-4-5:nitro", claudeID},
		{"claude-sonnet-4-5:floor", claudeID},
		{"claude-sonnet-4-5:extended", claudeID},
		{"claude-sonnet-4-5:beta", claudeID},
		{"claude-sonnet-4-5:exacto", claudeID},
		// 方括号后缀
		{"claude-sonnet-4-5[1m]", claudeID},
		{"claude-sonnet-4-5[2M]", claudeID},
		// 圆括号思考预算后缀
		{"gpt-5(minimal)", gptID},
		{"gpt-5(low)", gptID},
		{"gpt-5(medium)", gptID},
		{"gpt-5(high)", gptID},
		{"gpt-5(xhigh)", gptID},
		{"gpt-5(max)", gptID},
		{"gpt-5(none)", gptID},
		{"gpt-5(off)", gptID},
		{"gpt-5(auto)", gptID},
		{"gpt-5(0)", gptID},
		{"gpt-5(16384)", gptID},
		{"gpt-5(99999999)", gptID},
		// 连字符思考/分层后缀
		{"gpt-5-minimal", gptID},
		{"gpt-5-low", gptID},
		{"gpt-5-medium", gptID},
		{"gpt-5-high", gptID},
		{"gpt-5-xhigh", gptID},
		{"claude-sonnet-4-5-thinking", claudeID},
		{"claude-sonnet-4-5-nothinking", claudeID},
		{"claude-sonnet-4-5-non-thinking", claudeID},
		{"claude-sonnet-4-5-tiered", claudeID},
		// 组合两层后缀及别名回退
		{"openrouter/claude-sonnet-4-5-non-thinking[1m]", claudeID},
		{"claude-sonnet-4-5-20250929(16384):free", claudeID},
		{"GPT-5-HIGH[2m]", gptID},
		{"claude-sonnet-4-5[1m]:thinking", claudeID},
	}
	for _, tc := range matched {
		res, err := registry.Resolve(tc.raw)
		if err != nil || !res.Matched || res.ID != tc.want {
			t.Fatalf("Resolve(%q) = %+v, %v; want id=%d", tc.raw, res, err, tc.want)
		}
	}

	unmatched := []string{
		// 禁止剥离日期、-max、-latest 及未知后缀
		"claude-sonnet-4-5-20251001",
		"gpt-5-max",
		"gpt-5-latest",
		"gpt-5:custom",
		"gpt-5[4m]",
		"gpt-5()",
		"gpt-5(123456789)",
		"gpt-5(-1)",
		"gpt-5(fast)",
		// 超过两层后缀组合不应继续剥离
		"gpt-5-high[1m]:free",
		// 仅有后缀本身不能匹配空串
		":free",
		"(high)",
		"-thinking",
	}
	for _, raw := range unmatched {
		res, err := registry.Resolve(raw)
		if err != nil || res.Matched || res.ID != 0 {
			t.Fatalf("Resolve(%q) unexpectedly matched: %+v, %v", raw, res, err)
		}
	}
}

func TestResolveShippedMainstreamModelsAndAliases(t *testing.T) {
	catalog, err := models.Load(filepath.Join("..", "..", "..", "config", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	db := openModelDB(t, filepath.Join(t.TempDir(), "ranking.db"))
	registry, err := models.Register(t.Context(), db.Writer, catalog)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"openai/gpt-6.2-astra":             "gpt-6.2-astra",
		"openrouter/gpt-6.2-astra-pro":     "gpt-6.2-astra",
		"devin/gpt-6-2-astra":              "gpt-6.2-astra",
		"openai/gpt-4o-2024-05-13":         "gpt-4o-2024-05-13",
		"anthropic/claude-fable-5-2":       "claude-fable-5-2",
		"copilot/claude-fable-5.2":         "claude-fable-5-2",
		"copilot/mai-code-2":               "mai-code-2",
		"antigravity/gemini-4-carbon-high": "gemini-4-carbon",
		"gemini-4-carbon-medium":           "gemini-4-carbon",
		"gemini-4-carbon-low":              "gemini-4-carbon",
		"antigravity/gemini-4-neon-xhigh":  "gemini-4-neon",
		"gemini-3.8-flash-tiered":          "gemini-3.8-flash",
		"gemini-3.6-flash-minimal":         "gemini-3.6-flash",
		"gemini-nano-banana-2.1-high":      "gemini-nano-banana-2.1",
		"alibaba/Qwen4-Max":                "qwen4-max",
		"zai/GLM-5.4":                      "glm-5.4",
		"moonshot/kimi-k3.1-pro":           "kimi-k3.1-pro",
		"deepseek/deepseek-flash":          "deepseek-v4.1-flash",
		"deepseek/deepseek-chat":           "deepseek-v4-flash-0731",
		"xai/grok-4.7":                     "grok-4.7",
		"xai/grok-3-mini-fast":             "grok-3-mini-fast",
		"tencent/hy4":                      "hy4",
		"meta/muse-wonder-1.0":             "muse-wonder-1.0",
	}
	for raw, wantCanonical := range cases {
		res, err := registry.Resolve(raw)
		if err != nil || !res.Matched || res.CanonicalName != wantCanonical || res.ID <= 0 {
			t.Fatalf("Resolve(%q) = %+v, %v; want canonical %q", raw, res, err, wantCanonical)
		}
	}

	// 已移除的冷门模型不应命中。
	for _, removed := range []string{"command-a-plus", "cohere/command-a-plus-05-2026", "mimo-v2.6-flash", "step-3.7-flash"} {
		res, err := registry.Resolve(removed)
		if err != nil || res.Matched || res.ID != 0 {
			t.Fatalf("removed model %q unexpectedly matched: %+v, %v", removed, res, err)
		}
	}
}
