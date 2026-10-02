package gateway

import "strings"

// modelPrice is per-token cost in USD (input and output priced separately).
type modelPrice struct{ inPer, outPer float64 }

// priceTable maps a model family to its approximate public API price, used to
// turn token counts into an equivalent-dollar figure. The host pays a flat
// subscription, not per token — this dollar amount is a common yardstick for
// usage and quota, not a real bill. Prices are per-million-token list prices
// (as of early 2026) divided into per-token; they drift, and under the
// subscription model exactness doesn't matter — adjust freely.
//
// Model names are matched by family substring against the model reported in
// the provider's response, so date-suffixed and versioned names still resolve.
var (
	priceOpus       = perM(15, 75)
	priceSonnet     = perM(3, 15)
	priceHaiku      = perM(0.80, 4)
	priceGPT5       = perM(1.25, 10)
	priceGrok       = perM(3, 15)
	priceFallback   = perM(3, 15) // unknown model: a middle-of-the-road estimate
	providerDefault = map[string]modelPrice{
		"anthropic": priceSonnet,
		"openai":    priceGPT5,
		"xai":       priceGrok,
	}
)

// perM converts per-million-token list prices to per-token.
func perM(in, out float64) modelPrice { return modelPrice{in / 1e6, out / 1e6} }

// priceFor resolves a model name (from the response) to a price. provider is
// the fallback when the model is empty or unrecognized.
func priceFor(model, provider string) modelPrice {
	m := strings.ToLower(model)
	switch {
	case m == "":
		// fall through to provider default below
	case strings.Contains(m, "opus"):
		return priceOpus
	case strings.Contains(m, "sonnet"):
		return priceSonnet
	case strings.Contains(m, "haiku"):
		return priceHaiku
	case strings.Contains(m, "grok"):
		return priceGrok
	case strings.HasPrefix(m, "gpt-5") || strings.HasPrefix(m, "gpt5") || strings.Contains(m, "codex") || strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3"):
		return priceGPT5
	}
	if p, ok := providerDefault[provider]; ok {
		return p
	}
	return priceFallback
}

// costUSD is the equivalent-dollar cost of a request given its token counts and
// the model that served it (no cache traffic).
func costUSD(model, provider string, in, out int) float64 {
	p := priceFor(model, provider)
	return float64(in)*p.inPer + float64(out)*p.outPer
}

// Cache pricing relative to the input price: both vendors list cache reads
// at roughly a tenth of input, and Anthropic lists cache writes at 1.25×.
const (
	cacheReadFactor  = 0.1
	cacheWriteFactor = 1.25
)

// billTokens applies cache-aware pricing. Anthropic reports cache tokens
// outside input_tokens; OpenAI/xAI report cached_tokens as a subset of
// input_tokens. Returned in/out are the totals to record for the usage chart.
func billTokens(model, provider string, in, out, cacheRead, cacheWrite, cached int) (totalIn, totalOut int, cost float64) {
	p := priceFor(model, provider)
	switch provider {
	case "anthropic":
		cost = float64(in)*p.inPer + float64(cacheRead)*p.inPer*cacheReadFactor + float64(cacheWrite)*p.inPer*cacheWriteFactor + float64(out)*p.outPer
		return in + cacheRead + cacheWrite, out, cost
	default:
		if cached > in {
			cached = in
		}
		cost = float64(in-cached)*p.inPer + float64(cached)*p.inPer*cacheReadFactor + float64(out)*p.outPer
		return in, out, cost
	}
}
