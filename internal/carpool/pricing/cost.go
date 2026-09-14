package pricing

import (
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

// Cost is an exact event cost rounded once to nano-USD.
type Cost struct {
	NanoUSD int64
	Known   bool
	Reason  string
}

// Calculate computes a canonical token breakdown against a model price.
func Calculate(price ModelPrice, breakdown usage.TokenBreakdown, tier string) Cost {
	if !breakdown.Valid() || breakdown.Quality != usage.TokenAccountingQualityComplete || breakdown.UnclassifiedTokens != 0 {
		return Cost{Reason: "token_breakdown_incomplete"}
	}
	if breakdown.TotalTokens == 0 {
		return Cost{Known: true}
	}
	input, output, read, write := price.InputPerToken, price.OutputPerToken, price.CacheReadPerToken, price.CacheWritePerToken
	if breakdown.Input.TotalTokens > price.LongContextThreshold && price.LongContextThreshold > 0 {
		if price.LongContextInputPerToken != nil {
			input = price.LongContextInputPerToken
		}
		if price.LongContextOutputPerToken != nil {
			output = price.LongContextOutputPerToken
		}
		if breakdown.Input.CacheReadTokens > 0 && price.LongContextCacheRead != nil {
			read = price.LongContextCacheRead
		}
		if breakdown.Input.CacheWriteTokens > 0 && price.LongContextCacheWrite != nil {
			write = price.LongContextCacheWrite
		}
	}
	if selected, ok := price.Tiers[strings.ToLower(strings.TrimSpace(tier))]; ok && tier != "" && tier != "default" && tier != "auto" {
		if selected.InputPerToken != nil {
			input = selected.InputPerToken
		}
		if selected.OutputPerToken != nil {
			output = selected.OutputPerToken
		}
		if selected.CacheReadPerToken != nil {
			read = selected.CacheReadPerToken
		}
		if selected.CacheWritePerToken != nil {
			write = selected.CacheWritePerToken
		}
	}
	if breakdown.Input.UncachedTokens > 0 && input == nil {
		return Cost{Reason: "input_price_missing"}
	}
	if breakdown.Input.CacheReadTokens > 0 && read == nil {
		return Cost{Reason: "cache_read_price_missing"}
	}
	if breakdown.Input.CacheWriteTokens > 0 && write == nil {
		return Cost{Reason: "cache_write_price_missing"}
	}
	if breakdown.Output.NonReasoningTokens > 0 && output == nil {
		return Cost{Reason: "output_price_missing"}
	}
	reasoning := price.ReasoningPerToken
	if reasoning == nil {
		reasoning = output
	}
	if breakdown.Output.ReasoningTokens > 0 && reasoning == nil {
		return Cost{Reason: "reasoning_price_missing"}
	}
	total := new(big.Rat)
	add := func(tokens int64, rate *big.Rat) {
		if tokens > 0 && rate != nil {
			total.Add(total, new(big.Rat).Mul(big.NewRat(tokens, 1), rate))
		}
	}
	add(breakdown.Input.UncachedTokens, input)
	add(breakdown.Input.CacheReadTokens, read)
	add(breakdown.Input.CacheWriteTokens, write)
	add(breakdown.Output.NonReasoningTokens, output)
	add(breakdown.Output.ReasoningTokens, reasoning)
	nano := new(big.Rat).Mul(total, big.NewRat(1_000_000_000, 1))
	q := new(big.Int).Quo(nano.Num(), nano.Denom())
	rem := new(big.Int).Mod(nano.Num(), nano.Denom())
	if new(big.Int).Lsh(rem, 1).Cmp(nano.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() {
		return Cost{Reason: "cost_overflow"}
	}
	return Cost{NanoUSD: q.Int64(), Known: true}
}

var ErrPriceMissing = errors.New("pricing: model price not configured")

func ValidateModel(catalog *Catalog, model string) (ModelPrice, error) {
	p, ok := catalog.Lookup(model)
	if !ok {
		return ModelPrice{}, fmt.Errorf("%w: %s", ErrPriceMissing, strings.TrimSpace(model))
	}
	return p, nil
}
