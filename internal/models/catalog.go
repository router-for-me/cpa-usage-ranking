// Package models loads the server-maintained model catalog.
package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Tier identifies a reference-price category in the model catalog.
type Tier uint8

const (
	Default Tier = iota
	Priority
	Flex
)

// ParseTier requires an explicit supported spelling; omission is handled by the caller.
func ParseTier(value string) (Tier, error) {
	switch value {
	case "default":
		return Default, nil
	case "priority":
		return Priority, nil
	case "flex":
		return Flex, nil
	default:
		return 0, fmt.Errorf("invalid price category %q", value)
	}
}

// Price retains exact decimal strings; nil means unknown, not zero.
type Price struct {
	Input      *string `json:"input"`
	Output     *string `json:"output"`
	CacheRead  *string `json:"cache_read"`
	CacheWrite *string `json:"cache_write"`
}

// Model contains one canonical name and its explicit aliases and known tiers.
type Model struct {
	Name    string
	Aliases []string
	Prices  map[Tier]Price
}

// Catalog is immutable after loading; exact names are case-sensitive.
type Catalog struct {
	models []Model
	byName map[string]int
}

type catalogFile struct {
	Currency  string         `json:"currency"`
	PriceUnit string         `json:"price_unit"`
	Models    *[]catalogItem `json:"models"`
}

type catalogItem struct {
	Name    string                     `json:"model_name"`
	Aliases []string                   `json:"alias"`
	Prices  map[string]json.RawMessage `json:"prices"`
}

// Load reads and validates exactly one JSON catalog. It never falls back to
// accepting arbitrary models or fetching another catalog.
func Load(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model catalog %q: %w", path, err)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("model catalog %q: invalid UTF-8", path)
	}
	var input catalogFile
	if err := decodeJSON(data, &input); err != nil {
		return nil, fmt.Errorf("decode model catalog %q: %w", path, err)
	}
	// currency 与 price_unit 仅为非运行时参考元数据，缺省时补齐默认值，不阻止服务启动。
	if strings.TrimSpace(input.Currency) == "" {
		input.Currency = "USD"
	}
	if strings.TrimSpace(input.PriceUnit) == "" {
		input.PriceUnit = "per_million_tokens"
	}
	if input.Models == nil {
		return nil, fmt.Errorf("model catalog: models must be an array")
	}
	catalog := &Catalog{models: make([]Model, len(*input.Models)), byName: make(map[string]int)}
	// 先占用所有标准名，再处理别名，避免配置顺序影响冲突检查。
	for i, item := range *input.Models {
		name := item.Name
		if err := validateName(name); err != nil {
			return nil, fmt.Errorf("model %d: invalid model_name: %w", i, err)
		}
		if _, exists := catalog.byName[name]; exists {
			return nil, fmt.Errorf("model catalog: duplicate model_name %q", name)
		}
		catalog.models[i] = Model{Name: name, Prices: make(map[Tier]Price)}
		catalog.byName[name] = i
	}
	for i, item := range *input.Models {
		model := &catalog.models[i]
		for _, alias := range item.Aliases {
			if err := validateName(alias); err != nil {
				return nil, fmt.Errorf("model %q: invalid alias: %w", model.Name, err)
			}
			if owner, exists := catalog.byName[alias]; exists && owner != i {
				return nil, fmt.Errorf("model catalog: name %q belongs to both %q and %q", alias, catalog.models[owner].Name, model.Name)
			}
			catalog.byName[alias] = i
			model.Aliases = append(model.Aliases, alias)
		}
		for key, raw := range item.Prices {
			tier, err := ParseTier(key)
			if err != nil {
				continue
			}
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				continue
			}
			var price Price
			if err := decodeJSON(raw, &price); err != nil {
				return nil, fmt.Errorf("model %q: prices for %s: %w", model.Name, key, err)
			}
			for field, value := range map[string]*string{"input": price.Input, "output": price.Output, "cache_read": price.CacheRead, "cache_write": price.CacheWrite} {
				if value != nil && !decimal(*value) {
					return nil, fmt.Errorf("model %q: %s.%s must be a non-negative decimal string", model.Name, key, field)
				}
			}
			model.Prices[tier] = price
		}
	}
	return catalog, nil
}

func decodeJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

func validateName(name string) error {
	if !utf8.ValidString(name) || name == "" || len(name) > 128 || strings.TrimSpace(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("name must be nonempty, valid UTF-8, at most 128 bytes, without surrounding whitespace or paths")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("name must not contain control characters")
		}
	}
	return nil
}

func decimal(value string) bool {
	if value == "" {
		return false
	}
	dot := false
	for i, c := range value {
		if c == '.' && !dot && i > 0 && i < len(value)-1 {
			dot = true
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// List returns a deep copy so callers cannot change the validated catalog.
func (c *Catalog) List() []Model {
	result := make([]Model, len(c.models))
	for i, model := range c.models {
		result[i] = Model{Name: model.Name, Aliases: append([]string(nil), model.Aliases...), Prices: make(map[Tier]Price, len(model.Prices))}
		for tier, price := range model.Prices {
			result[i].Prices[tier] = Price{Input: copyString(price.Input), Output: copyString(price.Output), CacheRead: copyString(price.CacheRead), CacheWrite: copyString(price.CacheWrite)}
		}
	}
	return result
}

func copyString(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
