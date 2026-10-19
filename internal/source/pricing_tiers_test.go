package source

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// tierCatalogue writes a one-model catalogue in OpenRouter's shape, with a
// long-prompt override that lists only some rates, the way the real dump does.
func tierCatalogue(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	doc := `{"data":[{"id":"vendor/tiered-1","pricing":{
		"prompt":"0.000002","completion":"0.000012",
		"input_cache_read":"0.0000002","input_cache_write":"0.000000375",
		"overrides":[{"min_prompt_tokens":200000,
			"prompt":"0.000004","completion":"0.000018","input_cache_read":"0.0000004"}]}}]}`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// TestPricerLongPromptTier: a call whose prompt reaches the threshold is billed
// entirely at the higher rates, and a rate the override leaves out carries over
// from the base price rather than falling to zero.
func TestPricerLongPromptTier(t *testing.T) {
	p, err := NewPricer(tierCatalogue(t), "")
	if err != nil {
		t.Fatal(err)
	}

	// 150K input + 49,999 cache read + 0 write: one token short of the tier.
	below, _ := p.Cost("tiered-1", 150_000, 1000, 49_999, 0)
	if want := 150_000*2e-6 + 1000*12e-6 + 49_999*0.2e-6; !near(below, want) {
		t.Errorf("below the tier: $%v, want $%v at base rates", below, want)
	}

	// The prompt is every input bucket together, cache included: 150K fresh
	// input with 50K read from cache is a 200K prompt.
	above, _ := p.Cost("tiered-1", 150_000, 1000, 50_000, 1000)
	want := 150_000*4e-6 + 1000*18e-6 + 50_000*0.4e-6 + 1000*0.375e-6
	if !near(above, want) {
		t.Errorf("at the tier: $%v, want $%v (cache write inherited from the base rate)", above, want)
	}
}

// TestPricerOneHourCacheWrite: a one-hour cache write is billed at twice the
// input rate, the rest of the writes at the listed five-minute rate.
func TestPricerOneHourCacheWrite(t *testing.T) {
	p := testPricer(t)
	r, ok := p.Resolve("claude-opus-4-8")
	if !ok {
		t.Fatal("claude-opus-4-8 is unpriced")
	}
	all5m, _ := p.CostCall("claude-opus-4-8", 0, 0, 0, 1000, 0)
	all1h, _ := p.CostCall("claude-opus-4-8", 0, 0, 0, 1000, 1000)
	mixed, _ := p.CostCall("claude-opus-4-8", 0, 0, 0, 1000, 400)
	if want := 1000 * r.CacheWrite / 1e6; !near(all5m, want) {
		t.Errorf("five-minute writes: $%v, want $%v", all5m, want)
	}
	if want := 1000 * r.Input * 2 / 1e6; !near(all1h, want) {
		t.Errorf("one-hour writes: $%v, want $%v (2x input)", all1h, want)
	}
	if want := (600*r.CacheWrite + 400*r.Input*2) / 1e6; !near(mixed, want) {
		t.Errorf("mixed writes: $%v, want $%v", mixed, want)
	}
	// More one-hour tokens than writes is a bad record; it is clamped rather
	// than billed beyond what was written.
	if over, _ := p.CostCall("claude-opus-4-8", 0, 0, 0, 1000, 5000); !near(over, all1h) {
		t.Errorf("1h > total: $%v, want it clamped to $%v", over, all1h)
	}
}

// TestPricerOverrideBeatsCatalogue: a price checked against the provider's own
// list wins over the catalogue's for that model, and only for that model.
func TestPricerOverrideBeatsCatalogue(t *testing.T) {
	p := testPricer(t)
	r, ok := p.Resolve("gemini-3.8-flash")
	if !ok {
		t.Fatal("gemini-3.8-flash is unpriced")
	}
	if r.Input != 0.75 || r.Output != 3.75 || r.CacheRead != 0.075 {
		t.Errorf("gemini-3.8-flash = %+v, want Google's $0.75/$3.75/$0.075", r)
	}
	// The vendor-prefixed spelling reaches the same override.
	if r2, _ := p.Resolve("google/gemini-3.8-flash"); r2.Input != r.Input {
		t.Errorf("google/gemini-3.8-flash input = %v, want %v", r2.Input, r.Input)
	}
}

// TestClaudeOneHourCacheWriteIsRead: the parser stores the one-hour share of a
// response's cache writes, and the call is priced with it.
func TestClaudeOneHourCacheWriteIsRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := `{"type":"assistant","sessionId":"s","uuid":"a","timestamp":"2026-05-01T09:00:00.000Z",` +
		`"message":{"id":"msg_1","model":"claude-opus-4-8","content":[{"type":"text","text":"x"}],` +
		`"usage":{"input_tokens":1,"output_tokens":1,"cache_creation_input_tokens":1000,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":1000}}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	pricer := testPricer(t)
	sw, _, err := NewClaudeParser().Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	c := sw.Calls[0]
	if c.CacheWriteTokens != 1000 || c.CacheWrite1hTokens != 1000 {
		t.Errorf("cache write = %d, 1h = %d; want 1000 and 1000", c.CacheWriteTokens, c.CacheWrite1hTokens)
	}
	want, _ := pricer.CostCall(c.Model, 1, 1, 0, 1000, 1000)
	if !near(c.CostUSD, want) {
		t.Errorf("cost $%v, want $%v with the writes at the one-hour rate", c.CostUSD, want)
	}
}
