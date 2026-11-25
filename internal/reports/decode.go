// Package reports implements the daily snapshot protocol independently of HTTP.
package reports

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid report")
var ErrTooManyItems = errors.New("too many report items")

// Report contains the typed input, before validation or catalog matching.
type Report struct {
	Revision int64
	Items    []Item
}

type Item struct {
	Model            string
	TotalTokens      int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	RequestCount     int64
	SuccessCount     int64
	TTFTAvgMS        *float64
	TTFTSamples      int64
	LatencyAvgMS     *float64
	LatencySamples   int64
}

// flexInt64 同时接受 JSON 整数与十进制字符串（拒绝 null、布尔、小数、科学计数法与溢出）。
type flexInt64 int64

func (v *flexInt64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ErrInvalid
	}
	var text string
	if trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &text); err != nil || text == "" {
			return ErrInvalid
		}
	} else {
		text = string(trimmed)
	}
	for i, c := range text {
		if c < '0' || c > '9' {
			if i != 0 || c != '-' || len(text) == 1 {
				return ErrInvalid
			}
		}
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return ErrInvalid
	}
	*v = flexInt64(parsed)
	return nil
}

// nullableFlexInt64 在明细可选计数字段中允许 null（解码为 0），其余规则与 flexInt64 一致。
type nullableFlexInt64 int64

func (v *nullableFlexInt64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		*v = 0
		return nil
	}
	var parsed flexInt64
	if err := parsed.UnmarshalJSON(trimmed); err != nil {
		return err
	}
	*v = nullableFlexInt64(parsed)
	return nil
}

// nullableFlexFloat64 接受 null、有限非负 JSON 数字以及有限非负十进制数字字符串。
type nullableFlexFloat64 struct {
	value *float64
}

func (v *nullableFlexFloat64) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return ErrInvalid
	}
	if bytes.Equal(trimmed, []byte("null")) {
		v.value = nil
		return nil
	}
	var parsed float64
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil || !isNonNegativeDecimalFloatText(text) {
			return ErrInvalid
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
			return ErrInvalid
		}
		parsed = f
	} else {
		if err := json.Unmarshal(trimmed, &parsed); err != nil {
			return ErrInvalid
		}
		if math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
			return ErrInvalid
		}
	}
	v.value = &parsed
	return nil
}

func isNonNegativeDecimalFloatText(text string) bool {
	if text == "" {
		return false
	}
	dot := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == '.' {
			if dot || i == 0 || i == len(text)-1 {
				return false
			}
			dot = true
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

type rawReport struct {
	SchemaVersion *int               `json:"schema_version"`
	Revision      *flexInt64         `json:"revision"`
	Items         *[]json.RawMessage `json:"items"`
}

type rawItem struct {
	Model            *string             `json:"model"`
	TotalTokens      nullableFlexInt64   `json:"total_tokens"`
	InputTokens      nullableFlexInt64   `json:"input_tokens"`
	OutputTokens     nullableFlexInt64   `json:"output_tokens"`
	CacheReadTokens  nullableFlexInt64   `json:"cache_read_tokens"`
	CacheWriteTokens nullableFlexInt64   `json:"cache_write_tokens"`
	ReasoningTokens  nullableFlexInt64   `json:"reasoning_tokens"`
	RequestCount     nullableFlexInt64   `json:"request_count"`
	SuccessCount     nullableFlexInt64   `json:"success_count"`
	TTFTAvgMS        nullableFlexFloat64 `json:"ttft_avg_ms"`
	TTFTSamples      nullableFlexInt64   `json:"ttft_samples"`
	LatencyAvgMS     nullableFlexFloat64 `json:"latency_avg_ms"`
	LatencySamples   nullableFlexInt64   `json:"latency_samples"`
}

// Decode accepts exactly one schema-version-1 report. Semantic metric and
// model-name validation is separate. Errors never contain submitted values.
func Decode(data []byte) (Report, error) {
	return decode(data, 0)
}

// DecodeLimited bounds items before individual item decoding. The limit applies
// to raw entries, not post-catalog models.
func DecodeLimited(data []byte, maxItems int) (Report, error) {
	if maxItems <= 0 {
		return Report{}, ErrInvalid
	}
	return decode(data, maxItems)
}

func decode(data []byte, maxItems int) (Report, error) {
	if !utf8.Valid(data) {
		return Report{}, ErrInvalid
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Report{}, ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var input rawReport
	if err := dec.Decode(&input); err != nil {
		return Report{}, ErrInvalid
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Report{}, ErrInvalid
	}
	if input.SchemaVersion == nil || *input.SchemaVersion != 1 {
		return Report{}, ErrInvalid
	}
	if input.Revision == nil || *input.Revision < 0 {
		return Report{}, ErrInvalid
	}
	if input.Items == nil || *input.Items == nil {
		return Report{}, ErrInvalid
	}
	entries := *input.Items
	if maxItems > 0 && len(entries) > maxItems {
		return Report{}, ErrTooManyItems
	}
	report := Report{Revision: int64(*input.Revision), Items: make([]Item, 0, len(entries))}
	for _, entry := range entries {
		item, err := decodeItem(entry)
		if err != nil {
			return Report{}, ErrInvalid
		}
		report.Items = append(report.Items, item)
	}
	return report, nil
}

func decodeItem(data []byte) (Item, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Item{}, ErrInvalid
	}
	var raw rawItem
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return Item{}, ErrInvalid
	}
	if raw.Model == nil {
		return Item{}, ErrInvalid
	}
	return Item{
		Model:            *raw.Model,
		TotalTokens:      int64(raw.TotalTokens),
		InputTokens:      int64(raw.InputTokens),
		OutputTokens:     int64(raw.OutputTokens),
		CacheReadTokens:  int64(raw.CacheReadTokens),
		CacheWriteTokens: int64(raw.CacheWriteTokens),
		ReasoningTokens:  int64(raw.ReasoningTokens),
		RequestCount:     int64(raw.RequestCount),
		SuccessCount:     int64(raw.SuccessCount),
		TTFTAvgMS:        raw.TTFTAvgMS.value,
		TTFTSamples:      int64(raw.TTFTSamples),
		LatencyAvgMS:     raw.LatencyAvgMS.value,
		LatencySamples:   int64(raw.LatencySamples),
	}, nil
}
