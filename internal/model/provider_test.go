package model

import (
	"fmt"
	"testing"
)

func TestProviderDetection(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		// Anthropic
		{"claude-opus-5-5", "Anthropic"},
		{"claude-sonnet-4.6", "Anthropic"},
		{"anthropic/claude-3-haiku", "Anthropic"},
		{"Anthropic: Claude 3.5 Sonnet", "Anthropic"},

		// Google
		{"gemini-3.8-flash", "Google"},
		{"gemini-3.1-pro", "Google"},
		{"google/gemini-2.5-flash", "Google"},
		{"gemma4:12b", "Google"},
		{"google/gemma-4-12b", "Google"},
		{"antigravity-model-1401", "Google"},

		// OpenAI
		{"gpt-4o", "OpenAI"},
		{"gpt-5.5", "OpenAI"},
		{"gpt-oss-120b", "OpenAI"},
		{"o1-preview", "OpenAI"},
		{"o3-mini", "OpenAI"},
		{"o4-max", "OpenAI"},
		{"openai/gpt-4", "OpenAI"},
		{"~openai/gpt-4", "OpenAI"},
		{"OpenAI: GPT-4 Turbo", "OpenAI"},

		// Meta
		{"llama-3.1-70b", "Meta"},
		{"meta-llama/llama-3-8b", "Meta"},
		{"meta/muse-spark-1.3", "Meta"},
		{"muse-spark-1.2-contributor-free", "Meta"},

		// Mistral
		{"mistral-large-2407", "Mistral"},
		{"mixtral-8x7b", "Mistral"},
		{"codestral-2501", "Mistral"},
		{"mistralai/mistral-large", "Mistral"},

		// DeepSeek
		{"deepseek-v4.1-flash", "DeepSeek"},
		{"deepseek/deepseek-chat", "DeepSeek"},

		// Qwen
		{"qwen3-vl-8b", "Qwen"},
		{"qwen/qwen3-30b", "Qwen"},

		// NVIDIA
		{"nemotron-3-super-free", "NVIDIA"},
		{"nvidia/nemotron-3.5-lightning", "NVIDIA"},

		// xAI
		{"grok-2-1212", "xAI"},
		{"x-ai/grok-beta", "xAI"},

		// Cohere
		{"command-r-plus", "Cohere"},
		{"cohere/command-r", "Cohere"},

		// Other / Fallback
		{"space-bunny-free", "Other"},
		{"Bonsai-8B-gguf", "Other"},
		{"unknown", "Other"},
		{"", "Other"},
	}

	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got := Provider(tc.model)
			if got != tc.want {
				t.Errorf("Provider(%q) = %q, want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestRollupProvidersMaxTenAndOther(t *testing.T) {
	// 12 named providers + 1 unknown
	models := []ModelStat{
		{Model: "claude-opus-5", Cost: 50.0, Calls: 10, OutputTokens: 1000, LLMSeconds: 10},
		{Model: "gemini-3.8-flash", Cost: 100.0, Calls: 20, OutputTokens: 2000, LLMSeconds: 20},
		{Model: "gpt-4o", Cost: 30.0, Calls: 5, OutputTokens: 500, LLMSeconds: 5},
		{Model: "deepseek-v4", Cost: 25.0, Calls: 4},
		{Model: "qwen-3", Cost: 20.0, Calls: 4},
		{Model: "mistral-large", Cost: 18.0, Calls: 3},
		{Model: "llama-3", Cost: 15.0, Calls: 3},
		{Model: "nemotron-3", Cost: 12.0, Calls: 2},
		{Model: "grok-2", Cost: 10.0, Calls: 2},
		{Model: "command-r", Cost: 8.0, Calls: 2},
		// 11th named provider (by cost)
		{Model: "amazon/titan-text", Cost: 5.0, Calls: 1},
		// 12th named provider (by cost)
		{Model: "microsoft/phi-3", Cost: 2.0, Calls: 1},
		// Unknown model
		{Model: "custom-space-bunny", Cost: 1.0, Calls: 1},
	}

	totalCost := 0.0
	for _, m := range models {
		totalCost += m.Cost
	}

	res := RollupProviders(models, totalCost)

	// Should have exactly 10 named providers + 1 "Other" = 11 entries
	if len(res) != 11 {
		t.Fatalf("RollupProviders returned %d entries, want 11", len(res))
	}

	// First provider should be Google with $100
	if res[0].Name != "Google" || res[0].Cost != 100.0 {
		t.Errorf("res[0] = %+v, want Google with cost 100", res[0])
	}
	// Second should be Anthropic with $50
	if res[1].Name != "Anthropic" || res[1].Cost != 50.0 {
		t.Errorf("res[1] = %+v, want Anthropic with cost 50", res[1])
	}

	// Last should be "Other", aggregating Amazon ($5) + Microsoft ($2) + custom-space-bunny ($1) = $8.0
	other := res[10]
	if other.Name != "Other" {
		t.Errorf("res[10].Name = %q, want Other", other.Name)
	}
	if other.Cost != 8.0 {
		t.Errorf("Other.Cost = %f, want 8.0", other.Cost)
	}
	if other.Calls != 3 {
		t.Errorf("Other.Calls = %d, want 3", other.Calls)
	}

	// Verify percentages sum to 100%
	sumPct := 0.0
	for _, p := range res {
		sumPct += p.Pct
	}
	if fmt.Sprintf("%.2f", sumPct) != "100.00" {
		t.Errorf("Sum of percentages = %f, want 100.00", sumPct)
	}
}

func TestRollupProvidersUnderTen(t *testing.T) {
	models := []ModelStat{
		{Model: "gemini-3.8-flash", Cost: 100.0, Calls: 20},
		{Model: "claude-opus-5", Cost: 50.0, Calls: 10},
	}
	res := RollupProviders(models, 150.0)
	if len(res) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(res))
	}
	if res[0].Name != "Google" || res[1].Name != "Anthropic" {
		t.Errorf("unexpected providers: %v, %v", res[0].Name, res[1].Name)
	}
}
