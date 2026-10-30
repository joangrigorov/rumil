package config

import (
	"strings"
	"testing"
)

// usageBase is a minimal configuration whose first chat provider a test can
// give prices.
const usageBase = `
github:
  app_id: 1
  installation_id: 2
  private_key: "key"
models:
  providers:
    - id: "chat"
      provider: "openai-compatible"
      base_url: "https://chat.example.com/v1"
      model_name: "chat"
      api_key: "k"
%s`

func TestProviderPrices(t *testing.T) {
	cfg := mustParse(t, strings.Replace(usageBase, "%s", `
      price_input_per_mtok: 0.27
      price_output_per_mtok: 1.1
      price_cache_read_per_mtok: 0.07
`, 1))

	provider := cfg.Models.Providers[0]
	if !provider.HasPrice() {
		t.Error("HasPrice = false with both dominant rates set")
	}
	price := provider.TokenPrice()
	if price.Input != 0.27 || price.Output != 1.1 || price.CacheRead != 0.07 {
		t.Errorf("price = %+v, want the configured rates", price)
	}

	unpriced := mustParse(t, strings.Replace(usageBase, "%s", "\n", 1))
	if unpriced.Models.Providers[0].HasPrice() {
		t.Error("HasPrice = true without prices")
	}
	if price := unpriced.Models.Providers[0].TokenPrice(); price != (TokenPrice{}) {
		t.Errorf("price = %+v, want zero rates", price)
	}
}

func TestProviderPricesRejectNegativeValues(t *testing.T) {
	for name, field := range map[string]string{
		"price_input_per_mtok":      "price_input_per_mtok: -1\n",
		"price_output_per_mtok":     "price_output_per_mtok: -1\n",
		"price_cache_read_per_mtok": "price_cache_read_per_mtok: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(usageBase, "%s", "      "+field, 1)), "test.yaml")
			if err == nil {
				t.Fatalf("Parse accepted a negative %s", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("Parse = %v, want it to name %s", err, name)
			}
		})
	}
}

// A token budget needs no prices; a cost budget does, because otherwise it
// cannot be evaluated and would fail every session at zero dollars.
func TestReviewBudgets(t *testing.T) {
	t.Run("token budget without prices", func(t *testing.T) {
		doc := strings.Replace(usageBase, "%s", "\n", 1) + "review:\n  max_total_tokens: 100000\n"
		cfg := mustParse(t, doc)
		if cfg.Review.MaxTotalTokens != 100000 {
			t.Errorf("max_total_tokens = %d, want 100000", cfg.Review.MaxTotalTokens)
		}
	})

	t.Run("cost budget with prices", func(t *testing.T) {
		doc := strings.Replace(usageBase, "%s", `
      price_input_per_mtok: 1
      price_output_per_mtok: 2
`, 1) + "review:\n  max_cost_usd: 0.5\n"
		cfg := mustParse(t, doc)
		if cfg.Review.MaxCostUSD != 0.5 {
			t.Errorf("max_cost_usd = %v, want 0.5", cfg.Review.MaxCostUSD)
		}
	})

	t.Run("cost budget without prices", func(t *testing.T) {
		doc := strings.Replace(usageBase, "%s", "\n", 1) + "review:\n  max_cost_usd: 0.5\n"
		_, err := Parse([]byte(doc), "test.yaml")
		if err == nil {
			t.Fatal("Parse accepted a cost budget with no prices")
		}
		if !strings.Contains(err.Error(), "max_cost_usd") {
			t.Errorf("Parse = %v, want it to name the budget", err)
		}
	})

	t.Run("negative budgets", func(t *testing.T) {
		for name, block := range map[string]string{
			"max_total_tokens": "review:\n  max_total_tokens: -1\n",
			"max_cost_usd":     "review:\n  max_cost_usd: -0.5\n",
		} {
			if _, err := Parse([]byte(strings.Replace(usageBase, "%s", "\n", 1)+block), "test.yaml"); err == nil {
				t.Errorf("Parse accepted a negative %s", name)
			}
		}
	})
}
