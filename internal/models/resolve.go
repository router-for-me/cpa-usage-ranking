package models

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrInvalidReportedName identifies malformed input, distinct from a valid
// name that simply is not in the current catalog.
var ErrInvalidReportedName = errors.New("invalid reported model name")

// Resolution retains the normalized name even when no model matches.
// ID is positive only for a match; zero does not denote a total here.
type Resolution struct {
	NormalizedName string
	CanonicalName  string
	ID             int64
	Matched        bool
}

// 受控网关模式/渠道末尾后缀白名单；较长后缀排在前面，避免部分截断（如 -non-thinking 先于 -thinking）。
var gatewayLiteralSuffixes = []string{
	":free", ":thinking", ":online", ":nitro", ":floor", ":extended", ":beta", ":exacto",
	"[1m]", "[2m]",
	"-non-thinking", "-nothinking", "-thinking",
	"-minimal", "-low", "-medium", "-xhigh", "-high", "-tiered",
}

// Resolve 清理空白与路径前缀后，按不区分 ASCII 大小写查找当前目录；
// 未命中时最多剥离两层受控白名单末尾网关后缀再回退匹配。
func (r *Registry) Resolve(raw string) (Resolution, error) {
	name, err := NormalizeReportedName(raw)
	if err != nil {
		return Resolution{}, err
	}
	result := Resolution{NormalizedName: name}
	candidate := asciiLower(name)
	if index, ok := r.catalog.byName[candidate]; ok {
		return r.matchedResolution(name, index), nil
	}
	for range 2 {
		stripped, ok := stripGatewaySuffix(candidate)
		if !ok {
			break
		}
		candidate = stripped
		if index, ok := r.catalog.byName[candidate]; ok {
			return r.matchedResolution(name, index), nil
		}
	}
	return result, nil
}

func (r *Registry) matchedResolution(normalized string, index int) Resolution {
	canonical := r.catalog.models[index].Name
	return Resolution{
		NormalizedName: normalized,
		CanonicalName:  canonical,
		ID:             r.ids[canonical],
		Matched:        true,
	}
}

func stripGatewaySuffix(lower string) (string, bool) {
	for _, suffix := range gatewayLiteralSuffixes {
		if len(lower) > len(suffix) && strings.HasSuffix(lower, suffix) {
			return lower[:len(lower)-len(suffix)], true
		}
	}
	if strings.HasSuffix(lower, ")") {
		open := strings.LastIndexByte(lower, '(')
		if open > 0 {
			inside := lower[open+1 : len(lower)-1]
			if isAllowedParenBudget(inside) {
				return lower[:open], true
			}
		}
	}
	return "", false
}

func isAllowedParenBudget(inside string) bool {
	switch inside {
	case "minimal", "low", "medium", "high", "xhigh", "max", "none", "off", "auto":
		return true
	}
	if len(inside) < 1 || len(inside) > 8 {
		return false
	}
	for i := 0; i < len(inside); i++ {
		if inside[i] < '0' || inside[i] > '9' {
			return false
		}
	}
	return true
}

// NormalizeReportedName validates and normalizes without consulting a catalog.
func NormalizeReportedName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	if name == "" {
		return "", fmt.Errorf("%w: final name is empty", ErrInvalidReportedName)
	}
	if !utf8.ValidString(name) || len(name) > 128 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%w: expected valid UTF-8 of at most 128 bytes without control characters", ErrInvalidReportedName)
	}
	return name, nil
}
