// Package reports implements the daily snapshot protocol independently of HTTP.
package reports

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

type rawReport struct {
	SchemaVersion *int               `json:"schema_version"`
	Revision      *flexInt64         `json:"revision"`
	Items         *[]json.RawMessage `json:"items"`
}

type rawItem struct {
	Model            *string   `json:"model"`
	TotalTokens      flexInt64 `json:"total_tokens"`
	InputTokens      flexInt64 `json:"input_tokens"`
	OutputTokens     flexInt64 `json:"output_tokens"`
	CacheReadTokens  flexInt64 `json:"cache_read_tokens"`
	CacheWriteTokens flexInt64 `json:"cache_write_tokens"`
	ReasoningTokens  flexInt64 `json:"reasoning_tokens"`
	RequestCount     flexInt64 `json:"request_count"`
	SuccessCount     flexInt64 `json:"success_count"`
	TTFTAvgMS        *float64  `json:"ttft_avg_ms"`
	TTFTSamples      flexInt64 `json:"ttft_samples"`
	LatencyAvgMS     *float64  `json:"latency_avg_ms"`
	LatencySamples   flexInt64 `json:"latency_samples"`
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
		TTFTAvgMS:        raw.TTFTAvgMS,
		TTFTSamples:      int64(raw.TTFTSamples),
		LatencyAvgMS:     raw.LatencyAvgMS,
		LatencySamples:   int64(raw.LatencySamples),
	}, nil
}
