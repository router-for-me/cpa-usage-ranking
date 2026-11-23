package test

import (
	"errors"
	"strings"
	"testing"

	"cpa-usage-ranking/internal/reports"
)

const validPayload = `{"schema_version":1,"revision":"8","items":[{"model":"vendor/model","total_tokens":"9223372036854775807","input_tokens":"10","output_tokens":"5","cache_read_tokens":"1","cache_write_tokens":"2","reasoning_tokens":"3","request_count":"2","success_count":"1","ttft_avg_ms":null,"ttft_samples":"0","latency_avg_ms":0,"latency_samples":"2"}]}`

func TestDecodeVersionedReport(t *testing.T) {
	report, err := reports.Decode([]byte(validPayload))
	if err != nil || report.Revision != 8 || len(report.Items) != 1 {
		t.Fatalf("Decode: %+v %v", report, err)
	}
	item := report.Items[0]
	if item.Model != "vendor/model" || item.TotalTokens != 9223372036854775807 || item.InputTokens != 10 || item.OutputTokens != 5 || item.CacheReadTokens != 1 || item.CacheWriteTokens != 2 || item.ReasoningTokens != 3 || item.RequestCount != 2 || item.SuccessCount != 1 || item.TTFTAvgMS != nil || item.TTFTSamples != 0 || item.LatencyAvgMS == nil || *item.LatencyAvgMS != 0 || item.LatencySamples != 2 {
		t.Fatalf("decoded item = %+v", item)
	}
	for _, payload := range []string{
		`{"schema_version":1,"revision":"0","items":[]}`,
		`{"schema_version":1,"revision":8,"items":[]}`,
		strings.Replace(validPayload, `"revision":"8"`, `"revision":"0008"`, 1),
		strings.Replace(validPayload, `"model":"vendor/model"`, `"model":"\ud83d\ude00"`, 1),
		strings.Replace(validPayload, `"model":"vendor/model"`, `"model":"\ud800"`, 1),
		strings.Replace(validPayload, `"revision":"8"`, `"revision":8,"client_id":"1","extra":{"nested":true}`, 1),
		strings.Replace(validPayload, `"model":"vendor/model"`, `"model":"vendor/model","service_tier":"default"`, 1),
		strings.Replace(validPayload, `"model":"vendor/model"`, `"model":"vendor/model","service_tier":null`, 1),
		`{"schema_version":1,"revision":1,"items":[{"model":"vendor/model","total_tokens":9223372036854775807,"request_count":3}]}`,
	} {
		if _, err := reports.Decode([]byte(payload)); err != nil {
			t.Fatalf("valid payload %s rejected: %v", payload, err)
		}
	}
	minimal, err := reports.Decode([]byte(`{"schema_version":1,"revision":12,"items":[{"model":"alpha","total_tokens":50}]}`))
	if err != nil || minimal.Revision != 12 || len(minimal.Items) != 1 {
		t.Fatalf("minimal decode: %+v %v", minimal, err)
	}
	got := minimal.Items[0]
	if got.Model != "alpha" || got.TotalTokens != 50 || got.InputTokens != 0 || got.OutputTokens != 0 || got.RequestCount != 0 || got.TTFTAvgMS != nil || got.LatencyAvgMS != nil {
		t.Fatalf("minimal defaults: %+v", got)
	}
}

func TestDecodeRejectsMissingNullIncorrectTypesAndTrailing(t *testing.T) {
	cases := map[string]string{
		"missing version":    strings.Replace(validPayload, `"schema_version":1,`, "", 1),
		"null version":       strings.Replace(validPayload, `"schema_version":1`, `"schema_version":null`, 1),
		"string version":     strings.Replace(validPayload, `"schema_version":1`, `"schema_version":"1"`, 1),
		"future version":     strings.Replace(validPayload, `"schema_version":1`, `"schema_version":2`, 1),
		"missing revision":   strings.Replace(validPayload, `"revision":"8",`, "", 1),
		"null revision":      strings.Replace(validPayload, `"revision":"8"`, `"revision":null`, 1),
		"negative revision":  strings.Replace(validPayload, `"revision":"8"`, `"revision":"-1"`, 1),
		"negative num rev":   strings.Replace(validPayload, `"revision":"8"`, `"revision":-1`, 1),
		"revision overflow":  strings.Replace(validPayload, `"revision":"8"`, `"revision":"9223372036854775808"`, 1),
		"num rev overflow":   strings.Replace(validPayload, `"revision":"8"`, `"revision":9223372036854775808`, 1),
		"plus revision":      strings.Replace(validPayload, `"revision":"8"`, `"revision":"+1"`, 1),
		"sci revision":       strings.Replace(validPayload, `"revision":"8"`, `"revision":1e2`, 1),
		"missing items":      `{"schema_version":1,"revision":"0"}`,
		"null items":         `{"schema_version":1,"revision":"0","items":null}`,
		"object items":       `{"schema_version":1,"revision":"0","items":{}}`,
		"null item":          `{"schema_version":1,"revision":"0","items":[null]}`,
		"missing model":      strings.Replace(validPayload, `"model":"vendor/model",`, "", 1),
		"null model":         strings.Replace(validPayload, `"model":"vendor/model"`, `"model":null`, 1),
		"null tokens":        strings.Replace(validPayload, `"input_tokens":"10"`, `"input_tokens":null`, 1),
		"bool tokens":        strings.Replace(validPayload, `"input_tokens":"10"`, `"input_tokens":true`, 1),
		"fraction tokens":    strings.Replace(validPayload, `"input_tokens":"10"`, `"input_tokens":"1.0"`, 1),
		"num fraction":       strings.Replace(validPayload, `"input_tokens":"10"`, `"input_tokens":1.5`, 1),
		"sci tokens":         strings.Replace(validPayload, `"input_tokens":"10"`, `"input_tokens":1e3`, 1),
		"token overflow":     strings.Replace(validPayload, `"input_tokens":"10"`, `"input_tokens":"9223372036854775808"`, 1),
		"num token overflow": strings.Replace(validPayload, `"input_tokens":"10"`, `"input_tokens":9223372036854775808`, 1),
		"string avg":         strings.Replace(validPayload, `"ttft_avg_ms":null`, `"ttft_avg_ms":"0"`, 1),
		"boolean avg":        strings.Replace(validPayload, `"ttft_avg_ms":null`, `"ttft_avg_ms":false`, 1),
		"nonfinite":          strings.Replace(validPayload, `"ttft_avg_ms":null`, `"ttft_avg_ms":1e999`, 1),
		"trailing object":    validPayload + `{}`,
		"array root":         `[]`,
		"invalid utf8":       strings.Replace(validPayload, "vendor/model", string([]byte{0xff}), 1),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := reports.Decode([]byte(input)); !errors.Is(err, reports.ErrInvalid) {
				t.Fatalf("expected invalid report, got %v", err)
			}
		})
	}
}
