package model

import (
	"sort"
	"strings"
)

// ModelStat is one model's aggregated metrics.
type ModelStat struct {
	Model            string
	Cost             float64
	Calls            int
	TotalTokens      int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	LLMSeconds       float64
	FirstTS          int64
	LastTS           int64
	Unpriced         int
}

// ProviderStat is the aggregated metrics for a single model provider.
type ProviderStat struct {
	Name             string   `json:"name"`
	Provider         string   `json:"provider"`
	Cost             float64  `json:"cost"`
	Calls            int      `json:"messages"`
	TotalTokens      int64    `json:"tokens"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	LLMSeconds       float64  `json:"llm_time"`
	AvgTPS           float64  `json:"avg_tps"`
	FirstTS          int64    `json:"first_ts"`
	LastTS           int64    `json:"last_ts"`
	Unpriced         int      `json:"unpriced"`
	Pct              float64  `json:"pct"`
	Models           []string `json:"models"`
}

// MaxNamedProviders is the maximum number of major providers to distinguish
// before grouping the remainder into "Other".
const MaxNamedProviders = 10

// Provider returns the canonical provider name for an LLM model identifier.
func Provider(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return "Other"
	}

	// 1. If prefixed with a vendor directory or organization (e.g. "openai/gpt-4")
	if i := strings.IndexByte(m, '/'); i >= 0 {
		prefix := m[:i]
		switch prefix {
		case "openai", "~openai":
			return "OpenAI"
		case "anthropic":
			return "Anthropic"
		case "google":
			return "Google"
		case "meta", "meta-llama":
			return "Meta"
		case "mistralai", "mistral":
			return "Mistral"
		case "deepseek":
			return "DeepSeek"
		case "qwen", "alibaba":
			return "Qwen"
		case "x-ai", "xai":
			return "xAI"
		case "nvidia":
			return "NVIDIA"
		case "cohere":
			return "Cohere"
		case "amazon", "bedrock":
			return "Amazon"
		case "microsoft":
			return "Microsoft"
		case "upstage":
			return "Upstage"
		case "cerebras":
			return "Cerebras"
		case "groq":
			return "Groq"
		case "together":
			return "Together"
		case "perplexity":
			return "Perplexity"
		}
	}

	// 2. If prefixed with a colon provider label (e.g. "OpenAI: GPT-4")
	if i := strings.IndexByte(m, ':'); i >= 0 {
		prefix := strings.TrimSpace(m[:i])
		switch prefix {
		case "openai":
			return "OpenAI"
		case "anthropic":
			return "Anthropic"
		case "google":
			return "Google"
		case "meta":
			return "Meta"
		case "mistral", "mistralai":
			return "Mistral"
		case "deepseek":
			return "DeepSeek"
		case "qwen", "alibaba":
			return "Qwen"
		case "xai", "x-ai":
			return "xAI"
		case "nvidia":
			return "NVIDIA"
		case "cohere":
			return "Cohere"
		}
	}

	// 3. Keyword / model family heuristic matching
	switch {
	case strings.Contains(m, "claude"):
		return "Anthropic"
	case strings.Contains(m, "gemini"), strings.Contains(m, "gemma"), strings.Contains(m, "antigravity"):
		return "Google"
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "chatgpt"),
		strings.HasPrefix(m, "o1-"), strings.HasPrefix(m, "o3-"), strings.HasPrefix(m, "o4-"),
		strings.Contains(m, "openai"), strings.HasPrefix(m, "dall-e"),
		strings.HasPrefix(m, "text-embedding"):
		return "OpenAI"
	case strings.Contains(m, "deepseek"):
		return "DeepSeek"
	case strings.Contains(m, "qwen"):
		return "Qwen"
	case strings.Contains(m, "llama"), strings.Contains(m, "muse"):
		return "Meta"
	case strings.Contains(m, "mistral"), strings.Contains(m, "mixtral"),
		strings.Contains(m, "codestral"), strings.Contains(m, "ministral"),
		strings.Contains(m, "pixtral"):
		return "Mistral"
	case strings.Contains(m, "nemotron"):
		return "NVIDIA"
	case strings.Contains(m, "grok"):
		return "xAI"
	case strings.Contains(m, "command"):
		return "Cohere"
	case strings.Contains(m, "phi-") || strings.HasPrefix(m, "phi"):
		return "Microsoft"
	case strings.Contains(m, "titan-") || strings.HasPrefix(m, "titan"):
		return "Amazon"
	default:
		return "Other"
	}
}

// RollupProviders groups per-model stats into at most MaxNamedProviders (10)
// major providers, combining any remaining providers (plus unknown models) into "Other".
func RollupProviders(models []ModelStat, totalCost float64) []ProviderStat {
	if len(models) == 0 {
		return nil
	}

	grouped := make(map[string]*ProviderStat)
	for _, m := range models {
		prov := Provider(m.Model)
		p, ok := grouped[prov]
		if !ok {
			p = &ProviderStat{
				Name:     prov,
				Provider: prov,
			}
			grouped[prov] = p
		}
		p.Cost += m.Cost
		p.Calls += m.Calls
		p.TotalTokens += m.TotalTokens
		p.InputTokens += m.InputTokens
		p.OutputTokens += m.OutputTokens
		p.CacheReadTokens += m.CacheReadTokens
		p.CacheWriteTokens += m.CacheWriteTokens
		p.ReasoningTokens += m.ReasoningTokens
		p.LLMSeconds += m.LLMSeconds
		p.Unpriced += m.Unpriced
		if p.FirstTS == 0 || (m.FirstTS > 0 && m.FirstTS < p.FirstTS) {
			p.FirstTS = m.FirstTS
		}
		if m.LastTS > p.LastTS {
			p.LastTS = m.LastTS
		}
		p.Models = append(p.Models, m.Model)
	}

	// Separate named providers from "Other".
	var named []*ProviderStat
	other := grouped["Other"]
	for name, p := range grouped {
		if name == "Other" {
			continue
		}
		named = append(named, p)
	}

	// Sort named providers by cost descending, then calls descending, then tokens descending.
	sort.Slice(named, func(i, j int) bool {
		if named[i].Cost != named[j].Cost {
			return named[i].Cost > named[j].Cost
		}
		if named[i].Calls != named[j].Calls {
			return named[i].Calls > named[j].Calls
		}
		if named[i].TotalTokens != named[j].TotalTokens {
			return named[i].TotalTokens > named[j].TotalTokens
		}
		return named[i].Name < named[j].Name
	})

	var top []*ProviderStat
	if len(named) > MaxNamedProviders {
		top = named[:MaxNamedProviders]
		if other == nil {
			other = &ProviderStat{Name: "Other", Provider: "Other"}
		}
		for _, rem := range named[MaxNamedProviders:] {
			other.Cost += rem.Cost
			other.Calls += rem.Calls
			other.TotalTokens += rem.TotalTokens
			other.InputTokens += rem.InputTokens
			other.OutputTokens += rem.OutputTokens
			other.CacheReadTokens += rem.CacheReadTokens
			other.CacheWriteTokens += rem.CacheWriteTokens
			other.ReasoningTokens += rem.ReasoningTokens
			other.LLMSeconds += rem.LLMSeconds
			other.Unpriced += rem.Unpriced
			if other.FirstTS == 0 || (rem.FirstTS > 0 && rem.FirstTS < other.FirstTS) {
				other.FirstTS = rem.FirstTS
			}
			if rem.LastTS > other.LastTS {
				other.LastTS = rem.LastTS
			}
			other.Models = append(other.Models, rem.Models...)
		}
	} else {
		top = named
	}

	if other != nil && other.Calls > 0 {
		top = append(top, other)
	}

	out := make([]ProviderStat, len(top))
	for i, p := range top {
		if totalCost > 0 {
			p.Pct = p.Cost / totalCost * 100
		}
		if p.LLMSeconds > 0 {
			p.AvgTPS = float64(p.OutputTokens) / p.LLMSeconds
		}
		sort.Strings(p.Models)
		out[i] = *p
	}

	return out
}
