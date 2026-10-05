package source

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// Codex is the one agent here whose usage arrives as a running session total
// rather than per call, so its two failure modes are both about differencing:
// the first total of a file is a baseline and must not be billed, and every
// later total is an increment that must be billed exactly once — including when
// the file grew since the last scan.

// codexEvent renders one token_count rollout event. Empty last means no
// last_token_usage block, which is the shape most rollouts carry.
func codexEvent(ts string, ordinal int, total, last map[string]int) string {
	info := map[string]any{"total_token_usage": total}
	if last != nil {
		info["last_token_usage"] = last
	}
	rec := map[string]any{
		"timestamp": ts,
		"ordinal":   ordinal,
		"type":      "event_msg",
		"payload":   map[string]any{"type": "token_count", "info": info},
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	return string(raw) + "\n"
}

func codexHeader(t *testing.T, path string) {
	t.Helper()
	lines := []string{
		`{"timestamp":"2026-05-01T10:00:00.000Z","ordinal":0,"type":"session_meta","payload":{"id":"s1","cwd":"/p"}}`,
		`{"timestamp":"2026-05-01T10:00:00.100Z","ordinal":1,"type":"turn_context","payload":{"model":"gpt-5.5"}}`,
	}
	appendTo(t, path, strings.Join(lines, "\n")+"\n")
}

func appendTo(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

type codexTotals struct{ input, cacheRead, output, reasoning int }

func sumCalls(calls []model.Call) codexTotals {
	var t codexTotals
	for _, c := range calls {
		t.input += c.InputTokens
		t.cacheRead += c.CacheReadTokens
		t.output += c.OutputTokens
		t.reasoning += c.ReasoningTokens
	}
	return t
}

// TestCodexResumeBillsEveryIncrementOnce is the money case: a rollout grows
// between scans, and the scanner polls it every 30s, so most passes are
// incremental.
//
// Before the fix the baseline a resumed read differenced against lived only in
// the sessionBuilder, which is built fresh for each Parse, so the first
// token_count of every incremental pass saw an empty baseline and — absent an
// explicit last_token_usage — was discarded as "a baseline". It was not one: its
// predecessor had been billed on the previous pass. One call's usage was
// silently dropped per scan, forever.
//
// TestCodexResumeDoesNotRecount does not catch this, because it only asserts
// that an *unchanged* rollout yields no calls.
func TestCodexResumeBillsEveryIncrementOnce(t *testing.T) {
	path := t.TempDir() + "/rollout.jsonl"
	codexHeader(t, path)
	appendTo(t, path, codexEvent("2026-05-01T10:00:10.000Z", 3,
		map[string]int{"input_tokens": 13601, "cached_input_tokens": 9984,
			"output_tokens": 72, "reasoning_output_tokens": 0}, nil))
	appendTo(t, path, codexEvent("2026-05-01T10:00:18.000Z", 4,
		map[string]int{"input_tokens": 33572, "cached_input_tokens": 23040,
			"output_tokens": 139, "reasoning_output_tokens": 0}, nil))

	p, pricer := NewCodexParser(), testPricer(t)
	first, state, err := p.Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Calls) != 1 {
		t.Fatalf("first pass parsed %d calls, want 1", len(first.Calls))
	}

	// The agent keeps working: two more token_count events, no last_token_usage,
	// so both must be differenced against their predecessor.
	appendTo(t, path, codexEvent("2026-05-01T10:00:26.000Z", 5,
		map[string]int{"input_tokens": 56954, "cached_input_tokens": 42240,
			"output_tokens": 274, "reasoning_output_tokens": 14}, nil))
	appendTo(t, path, codexEvent("2026-05-01T10:00:34.000Z", 6,
		map[string]int{"input_tokens": 61954, "cached_input_tokens": 44240,
			"output_tokens": 349, "reasoning_output_tokens": 14}, nil))

	inc, state, err := p.Parse(path, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, inc)
	if len(inc.Calls) != 2 {
		t.Fatalf("the increment parsed %d calls, want 2: one increment was dropped", len(inc.Calls))
	}

	// True increments, from the totals as written: input+cache 5000 then 5000,
	// output 135 then 75, of which cache reads are 19200 then 2000.
	want := []codexTotals{
		{input: 56954 - 33572 - (42240 - 23040), cacheRead: 42240 - 23040, output: 274 - 139, reasoning: 14},
		{input: 61954 - 56954 - (44240 - 42240), cacheRead: 44240 - 42240, output: 349 - 274, reasoning: 0},
	}
	for i, w := range want {
		got := inc.Calls[i]
		if got.InputTokens != w.input || got.CacheReadTokens != w.cacheRead ||
			got.OutputTokens != w.output || got.ReasoningTokens != w.reasoning {
			t.Errorf("call %d = %d/%d/%d/%d, want %d/%d/%d/%d", i,
				got.InputTokens, got.CacheReadTokens, got.OutputTokens, got.ReasoningTokens,
				w.input, w.cacheRead, w.output, w.reasoning)
		}
	}

	// The reconciliation that does not depend on any single figure: across every
	// pass the calls must sum to the last total less the first, because the
	// increments telescope. Before the fix this was short by the first
	// increment of every resumed pass.
	all := append(append([]model.Call{}, first.Calls...), inc.Calls...)
	got := sumCalls(all)
	// Codex's input_tokens includes the cached part and the parser stores the
	// two as disjoint buckets, so input+cache is exactly the input_tokens delta.
	wantInputPlusCache := 61954 - 13601
	if got.input+got.cacheRead != wantInputPlusCache {
		t.Errorf("input+cache across all passes = %d, want %d",
			got.input+got.cacheRead, wantInputPlusCache)
	}
	wantTotal := codexTotals{output: 349 - 72, reasoning: 14}
	if got.output != wantTotal.output || got.reasoning != wantTotal.reasoning {
		t.Errorf("output/reasoning across all passes = %d/%d, want %d/%d",
			got.output, got.reasoning, wantTotal.output, wantTotal.reasoning)
	}

	// A third pass over an unchanged rollout still finds nothing, so recovering
	// the baseline cost nothing when there is nothing to bill.
	again, _, err := p.Parse(path, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Calls) != 0 {
		t.Errorf("an unchanged rollout returned %d calls, want 0", len(again.Calls))
	}
}

// TestCodexReportedZeroIsNotTheRunningTotal covers the other way the
// differencing goes wrong: a zero reported for one call falling through to the
// cumulative figure.
//
// Codex writes cached_input_tokens: 0 and reasoning_output_tokens: 0 explicitly
// for a call that hit neither bucket. Testing the *value* rather than the
// presence of last_token_usage therefore sent those keys to
// total - prevTotals, billing a whole session's cache reads to a single call —
// 38,000 tokens on the shape below, which read no cache at all, while the real
// 200 input tokens were clamped away by the subtraction.
func TestCodexReportedZeroIsNotTheRunningTotal(t *testing.T) {
	path := t.TempDir() + "/rollout.jsonl"
	codexHeader(t, path)
	appendTo(t, path, codexEvent("2026-05-01T10:00:06.000Z", 2,
		map[string]int{
			"input_tokens": 40200, "cached_input_tokens": 38000,
			"output_tokens": 560, "reasoning_output_tokens": 1500,
		},
		map[string]int{
			"input_tokens": 200, "cached_input_tokens": 0,
			"output_tokens": 60, "reasoning_output_tokens": 0,
		}))

	sw, _, err := NewCodexParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	c := sw.Calls[0]
	if c.CacheReadTokens != 0 {
		t.Errorf("cache read = %d, want 0: a reported zero fell through to the running total (38000)", c.CacheReadTokens)
	}
	if c.InputTokens != 200 {
		t.Errorf("input = %d, want 200: the real input was clamped away by the cache subtraction", c.InputTokens)
	}
	if c.OutputTokens != 60 {
		t.Errorf("output = %d, want 60", c.OutputTokens)
	}
	if c.ReasoningTokens != 0 {
		t.Errorf("reasoning = %d, want 0: a reported zero fell through to the running total (60)", c.ReasoningTokens)
	}
}

// TestCodexExplicitZeroDeltasAreResumedToo pins the same shape on a resumed
// read, where the corrected presence test and the recovered baseline have to
// agree rather than cancel out.
func TestCodexExplicitZeroDeltasAreResumedToo(t *testing.T) {
	path := t.TempDir() + "/rollout.jsonl"
	codexHeader(t, path)
	appendTo(t, path, codexEvent("2026-05-01T10:00:10.000Z", 3,
		map[string]int{"input_tokens": 40200, "cached_input_tokens": 38000,
			"output_tokens": 560, "reasoning_output_tokens": 1500}, nil))

	p, pricer := NewCodexParser(), testPricer(t)
	_, state, err := p.Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	appendTo(t, path, codexEvent("2026-05-01T10:00:18.000Z", 4,
		map[string]int{"input_tokens": 40400, "cached_input_tokens": 38000,
			"output_tokens": 620, "reasoning_output_tokens": 1500},
		map[string]int{"input_tokens": 200, "cached_input_tokens": 0,
			"output_tokens": 60, "reasoning_output_tokens": 0}))

	inc, _, err := p.Parse(path, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(inc.Calls))
	}
	c := inc.Calls[0]
	if c.InputTokens != 200 || c.CacheReadTokens != 0 || c.OutputTokens != 60 || c.ReasoningTokens != 0 {
		t.Errorf("resumed call = %d/%d/%d/%d, want 200/0/60/0",
			c.InputTokens, c.CacheReadTokens, c.OutputTokens, c.ReasoningTokens)
	}
}

// TestCodexCounterResetIsAnUnpricedCallNotAConfidentZero separates the two kinds
// of "no tokens", which look identical in the token columns.
//
// A token_count event whose reported per-call usage is all zero is a turn that
// consumed nothing and is not a call. An event whose running total went *backwards*
// is a call that demonstrably happened and whose size the log cannot reveal: the
// increment across the reset is unrecoverable. Dropping it loses a call from the
// count; pricing it at $0.00 reports the unknown as a fact. It is stored, unpriced.
func TestCodexCounterResetIsAnUnpricedCallNotAConfidentZero(t *testing.T) {
	path := copyFixtureTo(t, "codex/rollout-counter-reset.jsonl")
	sw, _, err := NewCodexParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	var reset *model.Call
	for i := range sw.Calls {
		if sw.Calls[i].TotalTokens == 0 {
			reset = &sw.Calls[i]
		}
	}
	if reset == nil {
		t.Fatal("the reset event was dropped: a call that happened must stay in the count")
	}
	if reset.Priced {
		t.Error("the reset event is marked priced: its cost is unknown, not zero")
	}
	if reset.CostUSD != 0 {
		t.Errorf("cost = $%v, want 0", reset.CostUSD)
	}
	// The rest of the session is priced normally, so the unpriced marker is not
	// a blanket loss of the rate table.
	var priced int
	for _, c := range sw.Calls {
		if c.Priced {
			priced++
		}
	}
	if priced != len(sw.Calls)-1 {
		t.Errorf("%d of %d calls are priced; only the reset should be unpriced", priced, len(sw.Calls))
	}
}
