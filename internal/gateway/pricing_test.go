package gateway

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestPriceForByModel(t *testing.T) {
	cases := []struct {
		model, provider string
		want            modelPrice
	}{
		{"claude-opus-4-20250514", "anthropic", priceOpus},
		{"claude-3-5-sonnet-latest", "anthropic", priceSonnet},
		{"claude-haiku-4-5", "anthropic", priceHaiku},
		{"grok-4", "xai", priceGrok},
		{"gpt-5.6-sol", "openai", priceGPT5},
		{"gpt5-codex", "openai", priceGPT5},
		{"o3-mini", "openai", priceGPT5},
		// empty model → provider default
		{"", "anthropic", priceSonnet},
		{"", "openai", priceGPT5},
		{"", "xai", priceGrok},
		// unknown model, known provider → provider default
		{"llama-3", "anthropic", priceSonnet},
		// unknown model, unknown provider → fallback
		{"whatever", "unknownprov", priceFallback},
	}
	for _, c := range cases {
		got := priceFor(c.model, c.provider)
		if got != c.want {
			t.Errorf("priceFor(%q, %q) = %+v, want %+v", c.model, c.provider, got, c.want)
		}
	}
}

func TestCostUSD(t *testing.T) {
	// Sonnet: $3/M in, $15/M out. 1,000,000 in + 1,000,000 out = 3 + 15 = 18.
	if got := costUSD("claude-3-5-sonnet", "anthropic", 1_000_000, 1_000_000); !approx(got, 18) {
		t.Errorf("sonnet cost = %v, want 18", got)
	}
	// Opus: $15/M in, $75/M out. 200k in + 100k out = 3 + 7.5 = 10.5.
	if got := costUSD("claude-opus-4", "anthropic", 200_000, 100_000); !approx(got, 10.5) {
		t.Errorf("opus cost = %v, want 10.5", got)
	}
	// Zero tokens costs nothing.
	if got := costUSD("grok-4", "xai", 0, 0); got != 0 {
		t.Errorf("zero-token cost = %v, want 0", got)
	}
}
