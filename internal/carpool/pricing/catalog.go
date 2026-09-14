// Package pricing provides an immutable, exact model price catalog.
package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

const defaultLongContextThreshold int64 = 200_000

// TierPrices contains optional prices for a service tier (for example flex or priority).
type TierPrices struct {
	InputPerToken      *big.Rat
	OutputPerToken     *big.Rat
	CacheReadPerToken  *big.Rat
	CacheWritePerToken *big.Rat
}

// ModelPrice contains prices in USD per token. Nil fields mean that dimension
// is not present in the source catalog and cannot be priced when used.
type ModelPrice struct {
	Model                     string
	InputPerToken             *big.Rat
	OutputPerToken            *big.Rat
	CacheReadPerToken         *big.Rat
	CacheWritePerToken        *big.Rat
	ReasoningPerToken         *big.Rat
	LongContextThreshold      int64
	LongContextInputPerToken  *big.Rat
	LongContextOutputPerToken *big.Rat
	LongContextCacheRead      *big.Rat
	LongContextCacheWrite     *big.Rat
	Tiers                     map[string]TierPrices
}

func (p ModelPrice) clone() ModelPrice {
	p.InputPerToken = cloneRat(p.InputPerToken)
	p.OutputPerToken = cloneRat(p.OutputPerToken)
	p.CacheReadPerToken = cloneRat(p.CacheReadPerToken)
	p.CacheWritePerToken = cloneRat(p.CacheWritePerToken)
	p.ReasoningPerToken = cloneRat(p.ReasoningPerToken)
	p.LongContextInputPerToken = cloneRat(p.LongContextInputPerToken)
	p.LongContextOutputPerToken = cloneRat(p.LongContextOutputPerToken)
	p.LongContextCacheRead = cloneRat(p.LongContextCacheRead)
	p.LongContextCacheWrite = cloneRat(p.LongContextCacheWrite)
	if p.Tiers != nil {
		tiers := p.Tiers
		p.Tiers = make(map[string]TierPrices, len(tiers))
		for k, tier := range tiers {
			tier.InputPerToken = cloneRat(tier.InputPerToken)
			tier.OutputPerToken = cloneRat(tier.OutputPerToken)
			tier.CacheReadPerToken = cloneRat(tier.CacheReadPerToken)
			tier.CacheWritePerToken = cloneRat(tier.CacheWritePerToken)
			p.Tiers[k] = tier
		}
	}
	return p
}

// Catalog is an immutable parsed price snapshot.
type Catalog struct {
	Source   string
	Hash     string
	Raw      []byte
	Models   map[string]ModelPrice
	LoadedAt string
}

// ParseCatalog parses a LiteLLM-compatible model price JSON object.
func ParseCatalog(raw []byte, source string) (*Catalog, error) {
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("pricing catalog: decode JSON: %w", err)
	}
	if len(entries) == 0 {
		return nil, errors.New("pricing catalog: no model entries")
	}
	models := make(map[string]ModelPrice, len(entries))
	for name, rawModel := range entries {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(rawModel, &fields); err != nil {
			return nil, fmt.Errorf("pricing catalog: model %q is not an object: %w", name, err)
		}
		price := ModelPrice{Model: name, LongContextThreshold: defaultLongContextThreshold, Tiers: map[string]TierPrices{}}
		price.InputPerToken = fieldRat(fields, "input_cost_per_token")
		price.OutputPerToken = fieldRat(fields, "output_cost_per_token")
		price.CacheReadPerToken = firstRat(fields, "cache_read_input_token_cost", "cache_read_input_cost")
		price.CacheWritePerToken = firstRat(fields, "cache_creation_input_token_cost", "cache_write_input_token_cost", "cache_write_input_cost")
		price.ReasoningPerToken = firstRat(fields, "reasoning_cost_per_token", "output_reasoning_cost_per_token")
		if v, ok := fieldInt(fields, "long_context_threshold"); ok && v >= 0 {
			price.LongContextThreshold = v
		}
		price.LongContextInputPerToken = firstRat(fields, "input_cost_per_token_above_200k_tokens", "input_cost_per_token_above_long_context")
		price.LongContextOutputPerToken = firstRat(fields, "output_cost_per_token_above_200k_tokens", "output_cost_per_token_above_long_context")
		price.LongContextCacheRead = firstRat(fields, "cache_read_input_token_cost_above_200k_tokens")
		price.LongContextCacheWrite = firstRat(fields, "cache_creation_input_token_cost_above_200k_tokens")
		for _, tierName := range []string{"flex", "priority", "batch"} {
			tier := TierPrices{
				InputPerToken:      firstRat(fields, "input_cost_per_token_"+tierName),
				OutputPerToken:     firstRat(fields, "output_cost_per_token_"+tierName),
				CacheReadPerToken:  firstRat(fields, "cache_read_input_token_cost_"+tierName),
				CacheWritePerToken: firstRat(fields, "cache_creation_input_token_cost_"+tierName),
			}
			if tier.InputPerToken != nil || tier.OutputPerToken != nil || tier.CacheReadPerToken != nil || tier.CacheWritePerToken != nil {
				price.Tiers[tierName] = tier
			}
		}
		if price.InputPerToken == nil && price.OutputPerToken == nil && price.CacheReadPerToken == nil && price.CacheWritePerToken == nil {
			// LiteLLM contains context-only entries. Keep them out of the billable map.
			continue
		}
		models[normalize(name)] = price
	}
	if len(models) == 0 {
		return nil, errors.New("pricing catalog: no billable model prices")
	}
	hash := sha256.Sum256(raw)
	return &Catalog{Source: strings.TrimSpace(source), Hash: hex.EncodeToString(hash[:]), Raw: append([]byte(nil), raw...), Models: models}, nil
}

// Lookup resolves an exact model name or an explicitly supplied alias.
func (c *Catalog) Lookup(model string) (ModelPrice, bool) {
	if c == nil {
		return ModelPrice{}, false
	}
	p, ok := c.Models[normalize(model)]
	if !ok {
		return ModelPrice{}, false
	}
	return p.clone(), true
}

// AddAlias adds an exact, administrator-controlled alias to this catalog.
// It is intentionally not a fuzzy match.
func (c *Catalog) AddAlias(alias, model string) error {
	if c == nil {
		return errors.New("pricing catalog: nil catalog")
	}
	p, ok := c.Lookup(model)
	if !ok {
		return fmt.Errorf("pricing catalog: model %q not found", model)
	}
	alias = normalize(alias)
	if alias == "" {
		return errors.New("pricing catalog: empty alias")
	}
	c.Models[alias] = p
	return nil
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
func cloneRat(v *big.Rat) *big.Rat {
	if v == nil {
		return nil
	}
	return new(big.Rat).Set(v)
}

func firstRat(fields map[string]json.RawMessage, keys ...string) *big.Rat {
	for _, key := range keys {
		if value := fieldRat(fields, key); value != nil {
			return value
		}
	}
	return nil
}
func fieldRat(fields map[string]json.RawMessage, key string) *big.Rat {
	raw, ok := fields[key]
	if !ok || string(raw) == "null" {
		return nil
	}
	var value json.Number
	if err := json.Unmarshal(raw, &value); err != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return nil
		}
		value = json.Number(text)
	}
	r, err := parseDecimal(value.String())
	if err != nil || r.Sign() < 0 {
		return nil
	}
	return r
}
func fieldInt(fields map[string]json.RawMessage, key string) (int64, bool) {
	raw, ok := fields[key]
	if !ok {
		return 0, false
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	return v, err == nil
}

func parseDecimal(s string) (*big.Rat, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("empty decimal")
	}
	sign := 1
	if s[0] == '+' {
		s = s[1:]
	} else if s[0] == '-' {
		sign = -1
		s = s[1:]
	}
	parts := strings.Split(strings.ToLower(s), "e")
	if len(parts) > 2 {
		return nil, errors.New("invalid exponent")
	}
	base := parts[0]
	exp := int64(0)
	if len(parts) == 2 {
		var err error
		exp, err = strconv.ParseInt(parts[1], 10, 32)
		if err != nil {
			return nil, err
		}
	}
	dot := strings.IndexByte(base, '.')
	frac := int64(0)
	if dot >= 0 {
		frac = int64(len(base) - dot - 1)
		base = base[:dot] + base[dot+1:]
	}
	if base == "" {
		return nil, errors.New("invalid decimal")
	}
	for _, ch := range base {
		if ch < '0' || ch > '9' {
			return nil, errors.New("invalid decimal")
		}
	}
	n := new(big.Int)
	if _, ok := n.SetString(base, 10); !ok {
		return nil, errors.New("invalid decimal")
	}
	scale := frac - exp
	d := big.NewInt(1)
	if scale > 0 {
		d.Exp(big.NewInt(10), big.NewInt(scale), nil)
	} else if scale < 0 {
		n.Mul(n, new(big.Int).Exp(big.NewInt(10), big.NewInt(-scale), nil))
	}
	if sign < 0 {
		n.Neg(n)
	}
	return new(big.Rat).SetFrac(n, d), nil
}

// Snapshot exposes only read operations over an owned catalog copy.
// One snapshot may be shared by every event and attempt of a request.
type Snapshot struct{ catalog *Catalog }

// Freeze takes ownership of a deep copy, never of caller-owned maps or rationals.
func Freeze(catalog *Catalog) *Snapshot { return &Snapshot{catalog: cloneCatalog(catalog)} }
func (s *Snapshot) Lookup(model string) (ModelPrice, bool) {
	if s == nil {
		return ModelPrice{}, false
	}
	return s.catalog.Lookup(model)
}
func (s *Snapshot) Hash() string {
	if s == nil || s.catalog == nil {
		return ""
	}
	return s.catalog.Hash
}
func (s *Snapshot) LoadedAt() string {
	if s == nil || s.catalog == nil {
		return ""
	}
	return s.catalog.LoadedAt
}
