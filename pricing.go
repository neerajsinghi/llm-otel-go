package llmotel

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Price is the cost of a model in US dollars per one million tokens.
type Price struct {
	// InputPerMillion is the USD price per 1M input (prompt) tokens.
	InputPerMillion float64 `json:"input_per_million"`
	// OutputPerMillion is the USD price per 1M output (completion) tokens.
	OutputPerMillion float64 `json:"output_per_million"`
}

// PriceTable maps a model name, or a model name prefix, to its [Price].
//
// Lookup first tries an exact match and then the longest key that is a prefix
// of the model name, so "gpt-4o-mini" also prices "gpt-4o-mini-2024-07-18",
// and "claude-sonnet-4" prices "claude-sonnet-4-20250514".
//
// The table is user-maintained: providers change prices and launch models
// regularly, and this package does not track them.
type PriceTable map[string]Price

// Lookup returns the price for model and whether one was found.
func (t PriceTable) Lookup(model string) (Price, bool) {
	if model == "" || len(t) == 0 {
		return Price{}, false
	}
	if p, ok := t[model]; ok {
		return p, true
	}
	best, bestLen := Price{}, 0
	for k, p := range t {
		if len(k) > bestLen && strings.HasPrefix(model, k) {
			best, bestLen = p, len(k)
		}
	}
	return best, bestLen > 0
}

// Cost returns the estimated USD cost of a call to model that consumed the
// given number of input and output tokens. ok is false when the model is not
// in the table.
func (t PriceTable) Cost(model string, inputTokens, outputTokens int64) (usd float64, ok bool) {
	p, ok := t.Lookup(model)
	if !ok {
		return 0, false
	}
	return (float64(inputTokens)*p.InputPerMillion + float64(outputTokens)*p.OutputPerMillion) / 1e6, true
}

// LoadPrices decodes a JSON price table of the form
//
//	{"gpt-4o": {"input_per_million": 2.5, "output_per_million": 10}}
func LoadPrices(r io.Reader) (PriceTable, error) {
	var t PriceTable
	if err := json.NewDecoder(r).Decode(&t); err != nil {
		return nil, fmt.Errorf("llmotel: decode price table: %w", err)
	}
	for k, p := range t {
		if p.InputPerMillion < 0 || p.OutputPerMillion < 0 {
			return nil, fmt.Errorf("llmotel: negative price for model %q", k)
		}
	}
	return t, nil
}

// ExamplePrices returns a small illustrative price table for a handful of
// popular models.
//
// The numbers are list prices observed at the time of writing and are NOT
// kept up to date. They ignore batch discounts, prompt caching discounts,
// long-context surcharges and negotiated rates. Treat the result as a
// template: copy it, check it against your provider's pricing page and your
// invoices, and maintain your own table. The returned map is a fresh copy and
// safe to modify.
func ExamplePrices() PriceTable {
	return PriceTable{
		// OpenAI
		"gpt-4o":                 {InputPerMillion: 2.50, OutputPerMillion: 10.00},
		"gpt-4o-mini":            {InputPerMillion: 0.15, OutputPerMillion: 0.60},
		"gpt-4.1":                {InputPerMillion: 2.00, OutputPerMillion: 8.00},
		"gpt-4.1-mini":           {InputPerMillion: 0.40, OutputPerMillion: 1.60},
		"text-embedding-3-small": {InputPerMillion: 0.02},
		"text-embedding-3-large": {InputPerMillion: 0.13},
		// Anthropic
		"claude-opus-4":     {InputPerMillion: 15.00, OutputPerMillion: 75.00},
		"claude-sonnet-4":   {InputPerMillion: 3.00, OutputPerMillion: 15.00},
		"claude-haiku-4-5":  {InputPerMillion: 1.00, OutputPerMillion: 5.00},
		"claude-3-5-haiku":  {InputPerMillion: 0.80, OutputPerMillion: 4.00},
		"claude-3-7-sonnet": {InputPerMillion: 3.00, OutputPerMillion: 15.00},
	}
}
