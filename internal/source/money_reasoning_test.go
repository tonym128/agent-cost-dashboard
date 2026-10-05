package source

import (
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// Both providers here bill thinking at the output rate and report the generated
// count whole, with reasoning either reported alongside it or carved out of it
// for display. In both cases the billable figure is the whole generated count.
//
// Pricing only the itemised OutputTokens — the non-reasoning remainder — dropped
// the reasoning spend entirely: the identical bug already fixed for Claude and
// OpenCode, still live in these two parsers. A pi record of input 1800 / output
// 1900 / reasoning 640 stored $0.0369 where $0.0465 is correct, 20.6% under.

// TestPiBillsReasoningAtTheOutputRate checks the split is a display change only:
// the cost of the call is unchanged by itemising reasoning out of it.
func TestPiBillsReasoningAtTheOutputRate(t *testing.T) {
	path := copyFixtureTo(t, "pi/session-pi-1.jsonl")
	sw, _, err := NewPiParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	pricer := testPricer(t)
	var thinking *model.Call
	for i := range sw.Calls {
		if sw.Calls[i].ReasoningTokens > 0 {
			thinking = &sw.Calls[i]
		}
	}
	if thinking == nil {
		t.Fatal("the pi fixture's thinking record no longer carries reasoning tokens")
	}
	if thinking.OutputTokens+thinking.ReasoningTokens != 1900 {
		t.Errorf("generated count = %d, want 1900 (the fixture's reported output)",
			thinking.OutputTokens+thinking.ReasoningTokens)
	}
	want, ok := pricer.Cost(thinking.Model, thinking.InputTokens,
		thinking.OutputTokens+thinking.ReasoningTokens,
		thinking.CacheReadTokens, thinking.CacheWriteTokens)
	if !ok {
		t.Fatalf("%s is unpriced; the test would compare against nothing", thinking.Model)
	}
	if thinking.CostUSD != want {
		t.Errorf("cost = $%.6f, want $%.6f: reasoning was not billed",
			thinking.CostUSD, want)
	}
}

// TestGeminiBillsReasoningAtTheOutputRate is the same assertion for Gemini's
// thoughtsTokenCount.
//
// The fixture settles whether thoughts are a slice of the reported output or an
// addition to it: its first record reports input 12000 (of which 8000 cached),
// output 900, thoughts 250 and total_tokens 12900. 12000 + 900 is 12900, so the
// thoughts are already inside the output figure and are not an extra 250 to be
// added — itemising them out and billing the remainder under-charges by exactly
// the thinking spend.
func TestGeminiBillsReasoningAtTheOutputRate(t *testing.T) {
	path := copyFixtureTo(t, "gemini/example-project/chats/session-2026-05-05T11-00-0.testgemin.jsonl")
	sw, _, err := NewGeminiParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	pricer := testPricer(t)
	var thinking *model.Call
	for i := range sw.Calls {
		if sw.Calls[i].ReasoningTokens > 0 {
			thinking = &sw.Calls[i]
		}
	}
	if thinking == nil {
		t.Fatal("the Gemini fixture's thinking record no longer carries thoughtsTokenCount")
	}
	if got := thinking.OutputTokens + thinking.ReasoningTokens; got != 900 {
		t.Errorf("generated count = %d, want 900 (the fixture's reported output)", got)
	}
	want, ok := pricer.Cost(thinking.Model, thinking.InputTokens,
		thinking.OutputTokens+thinking.ReasoningTokens,
		thinking.CacheReadTokens, thinking.CacheWriteTokens)
	if !ok {
		t.Skipf("%s is unpriced in this environment", thinking.Model)
	}
	if thinking.CostUSD != want {
		t.Errorf("cost = $%.6f, want $%.6f: thoughts were not billed at the output rate",
			thinking.CostUSD, want)
	}
}

// TestTotalTokensIncludesReasoning guards the column total against the split.
//
// Reasoning is carved out of OutputTokens so it can be itemised, but it is still
// a token the provider generated and billed. Excluding it understated every
// call's total: on the OpenCode fixture, 47410 stored against 49305 actual, a
// 3.8% shortfall, which propagates into the session rollups, the models table
// and the tokens/sec throughput figure.
func TestTotalTokensIncludesReasoning(t *testing.T) {
	// Claude is used here because its fixture is a JSONL file with a call that
	// carries reasoning; the arithmetic under test is in addCall and is the same
	// for all six parsers.
	claudePath := copyFixtureTo(t, "claude/session-1.jsonl")
	sw, _, err := NewClaudeParser().Parse(claudePath, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	var sawReasoning bool
	for _, c := range sw.Calls {
		want := c.InputTokens + c.OutputTokens + c.ReasoningTokens +
			c.CacheReadTokens + c.CacheWriteTokens
		if c.TotalTokens != want {
			t.Errorf("call %q total = %d, want %d (every bucket, reasoning included)",
				c.CallKey, c.TotalTokens, want)
		}
		if c.ReasoningTokens > 0 {
			sawReasoning = true
		}
	}
	if !sawReasoning {
		t.Fatal("the fixture no longer exercises a call with reasoning tokens")
	}
}
