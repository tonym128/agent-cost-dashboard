package source

import (
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// The Codex fixtures are rollout-file shapes with every identifier and path
// replaced. Codex is the one agent here that reports usage as a running total
// rather than per call, so the arithmetic below is the only thing standing
// between the log and the numbers: if the differencing silently degrades to
// zero the dashboard still renders, it just renders a lie.

// TestCodexDifferencesRunningTotals is the test the whole Codex parser rests on.
//
// A rollout does not record what a call cost; it records what the session has
// cost so far. So a token_count event is only meaningful as the difference from
// the one before it. Reading the total as if it were the call's own usage
// inflates every call after the first by the whole session to date — and
// differencing the total against itself instead of against the previous one
// reports every call as free.
func TestCodexDifferencesRunningTotals(t *testing.T) {
	path := copyFixtureTo(t, "codex/rollout-differencing.jsonl")
	sw, state, err := NewCodexParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	// Session identity comes from the header record, not the file name, so the
	// same session reads the same way whatever the rollout is called on disk.
	if got, want := sw.Session.UID, "test-session-codex"; got != want {
		t.Errorf("session uid = %q, want %q", got, want)
	}
	if sw.Session.Agent != model.AgentCodex {
		t.Errorf("agent = %q, want %q", sw.Session.Agent, model.AgentCodex)
	}
	if got, want := sw.Session.Project, "/home/testuser/project"; got != want {
		t.Errorf("project = %q, want %q", got, want)
	}
	if got := NewCodexParser().SessionUID(path); got != sw.Session.UID {
		t.Errorf("SessionUID reported %q, want %q", got, sw.Session.UID)
	}

	// The file carries three token_count events and no last_token_usage block,
	// so the first is a baseline and only the two increments are calls. Three
	// calls here would mean the baseline was billed as usage of its own; two
	// zero-token calls would mean the differencing collapsed.
	if len(sw.Calls) != 2 {
		t.Fatalf("parsed %d calls, want 2 (the first total is a baseline, not a call)", len(sw.Calls))
	}

	// The running totals, as written in the fixture:
	//
	//	event  input   cached  output  reasoning
	//	1       13601    9984      72          0   <- baseline
	//	2       33572   23040     139          0
	//	3       56954   42240     274         14
	//
	// Codex's input figure includes the cached part, so the stored input is the
	// net after cache reads and the two buckets stay disjoint — the opposite of
	// the Anthropic convention, where all four buckets are summed.
	want := []struct {
		input, cacheRead, output, reasoning int
	}{
		{33572 - 13601 - (23040 - 9984), 23040 - 9984, 139 - 72, 0},     // 6915 / 13056 / 67 / 0
		{56954 - 33572 - (42240 - 23040), 42240 - 23040, 274 - 139, 14}, // 4182 / 19200 / 135 / 14
	}
	for i, w := range want {
		got := sw.Calls[i]
		if got.InputTokens != w.input {
			t.Errorf("call %d input = %d, want %d (the increment net of cache reads)", i, got.InputTokens, w.input)
		}
		if got.CacheReadTokens != w.cacheRead {
			t.Errorf("call %d cache read = %d, want %d", i, got.CacheReadTokens, w.cacheRead)
		}
		if got.OutputTokens != w.output {
			t.Errorf("call %d output = %d, want %d", i, got.OutputTokens, w.output)
		}
		if got.ReasoningTokens != w.reasoning {
			t.Errorf("call %d reasoning = %d, want %d", i, got.ReasoningTokens, w.reasoning)
		}
		// Every field is the difference of two numbers in the log, so a call
		// carrying the session total anywhere in it is the bug this test exists
		// for. The totals are all five figures; no single call should reach them.
		if got.InputTokens > 56954 || got.CacheReadTokens > 42240 {
			t.Errorf("call %d looks like a running total, not an increment: %+v", i, got)
		}
	}

	// The reconciliation that makes the arithmetic checkable without trusting
	// any single number: the increments telescope, so the calls sum to the last
	// running total minus the first. If differencing is right this holds
	// exactly; if it is wrong the error grows with every event.
	var input, cacheRead int
	for _, c := range sw.Calls {
		input += c.InputTokens
		cacheRead += c.CacheReadTokens
	}
	if got, want := input+cacheRead, 56954-13601; got != want {
		t.Errorf("input+cache across calls = %d, want %d (the last total less the first)", got, want)
	}

	// Timestamps come off the event, so each call is dated when its usage
	// arrived rather than when the session started.
	if got, want := sw.Calls[0].Time.UTC().Format("2006-01-02T15:04:05Z"), "2026-05-01T10:00:18Z"; got != want {
		t.Errorf("call 0 time = %s, want %s", got, want)
	}
	if !sw.FirstTS.Before(sw.LastTS) {
		t.Errorf("first %s is not before last %s", sw.FirstTS, sw.LastTS)
	}
	if state.Cursor != model.CursorBytes || state.Offset <= 0 {
		t.Errorf("cursor = %q at offset %d, want a byte cursor past the file", state.Cursor, state.Offset)
	}

	// The model comes from turn_context, which is the only record carrying it.
	if sw.Session.Title != "gpt-5.5" {
		t.Errorf("title = %q, want the turn's model gpt-5.5", sw.Session.Title)
	}
	for i, c := range sw.Calls {
		if c.Model != "gpt-5.5" {
			t.Errorf("call %d model = %q, want gpt-5.5", i, c.Model)
		}
	}
}

// TestCodexSingleTokenCountIsOneCall covers the degenerate session: one
// token_count event carrying an explicit last_token_usage block.
//
// There is nothing to difference against, so this is the path where the parser
// has to trust the log's own per-call figure. Codex's first total is already
// this call's usage — the rollout begins with it — so it must be billed rather
// than discarded as a baseline with nothing to compare it to.
func TestCodexSingleTokenCountIsOneCall(t *testing.T) {
	path := copyFixtureTo(t, "codex/rollout-single-event.jsonl")
	sw, _, err := NewCodexParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	c := sw.Calls[0]
	if got, want := c.InputTokens, 45210-40000; got != want {
		t.Errorf("input = %d, want %d (the reported input net of its cached part)", got, want)
	}
	if c.CacheReadTokens != 40000 {
		t.Errorf("cache read = %d, want 40000", c.CacheReadTokens)
	}
	if c.OutputTokens != 918 {
		t.Errorf("output = %d, want 918", c.OutputTokens)
	}
	if c.ReasoningTokens != 640 {
		t.Errorf("reasoning = %d, want 640", c.ReasoningTokens)
	}
	if !c.Priced || c.CostUSD <= 0 {
		t.Errorf("call is unpriced at $%v; a real token count must resolve to a rate", c.CostUSD)
	}
	if got, want := sw.Session.UID, "test-session-codex-single"; got != want {
		t.Errorf("session uid = %q, want %q", got, want)
	}
}

// TestCodexCounterResetNeverGoesNegative covers the case the differencing
// cannot survive: a total that decreases.
//
// Codex restarts its counters when the session is compacted or resumed, so the
// running total can drop. The increment across such an event is genuinely
// unknown — it is not that a call was refunded, and it is certainly not
// negative — so the parser clamps the delta to zero and this event contributes
// no tokens. Reporting the new total in full instead would double count
// everything before the reset, which is the failure this pins shut.
func TestCodexCounterResetNeverGoesNegative(t *testing.T) {
	path := copyFixtureTo(t, "codex/rollout-counter-reset.jsonl")
	sw, _, err := NewCodexParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	// Four token_count events: the first is the baseline, the second an
	// increment, the third the reset, the fourth an increment measured against
	// the post-reset total.
	if len(sw.Calls) != 3 {
		t.Fatalf("parsed %d calls, want 3 (baseline, increment, reset, increment)", len(sw.Calls))
	}

	// The reset is the call whose total fell from 94000 to 3100. Whatever the
	// parser decides to do with it, no field may come out negative: a negative
	// token count would flow straight into the totals and render as a bill
	// smaller than zero.
	reset := sw.Calls[1]
	for name, v := range map[string]int{
		"input": reset.InputTokens, "output": reset.OutputTokens,
		"cache read": reset.CacheReadTokens, "reasoning": reset.ReasoningTokens,
		"total": reset.TotalTokens,
	} {
		if v < 0 {
			t.Errorf("the reset event produced %d %s tokens", v, name)
		}
	}
	if reset.TotalTokens != 0 || reset.CostUSD != 0 {
		t.Errorf("the reset event contributed %d tokens for $%v; an unknown increment must contribute none",
			reset.TotalTokens, reset.CostUSD)
	}

	// The session's tokens are the increments, so the reset must not have
	// charged the post-reset total on top of them. Both totals below come from
	// the log: the pre-reset increments are 6000/4000/300/200 and the
	// post-reset ones 4300/3400/190/120, net of cache reads.
	want := []struct{ input, cacheRead, output, reasoning int }{
		{6000 - 4000, 4000, 300, 200},
		{0, 0, 0, 0},
		{4300 - 3400, 3400, 190, 120},
	}
	var input, cacheRead, output int
	for i, w := range want {
		c := sw.Calls[i]
		if c.InputTokens != w.input || c.CacheReadTokens != w.cacheRead ||
			c.OutputTokens != w.output || c.ReasoningTokens != w.reasoning {
			t.Errorf("call %d = %d/%d/%d/%d, want %d/%d/%d/%d", i,
				c.InputTokens, c.CacheReadTokens, c.OutputTokens, c.ReasoningTokens,
				w.input, w.cacheRead, w.output, w.reasoning)
		}
		input += c.InputTokens
		cacheRead += c.CacheReadTokens
		output += c.OutputTokens
	}
	if got, want := input+cacheRead, 6000+4300; got != want {
		t.Errorf("input+cache = %d, want %d: the reset double counted the session", got, want)
	}
	if output != 300+190 {
		t.Errorf("output = %d, want 490", output)
	}
	if sw.Session.UID != "test-session-codex-reset" {
		t.Errorf("session uid = %q, want test-session-codex-reset", sw.Session.UID)
	}
}

// TestCodexResumeDoesNotRecount guards the incremental half of the same
// arithmetic. The baseline that makes the first increment meaningful lives only
// in the builder, so a resumed scan that lost it would difference the first
// event after the cursor against zero and bill the whole running total again.
func TestCodexResumeDoesNotRecount(t *testing.T) {
	path := copyFixtureTo(t, "codex/rollout-differencing.jsonl")
	p := NewCodexParser()
	pricer := testPricer(t)

	_, state, err := p.Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := p.Parse(path, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Calls) != 0 {
		t.Errorf("an unchanged rollout returned %d calls, want 0", len(again.Calls))
	}
}

// TestCodexRecordsWithoutUsageAreNotCalls covers the negative half: a rollout is
// mostly conversation, and only token_count events carry billing. Treating any
// event as a call would pad the call count with rows worth nothing.
func TestCodexRecordsWithoutUsageAreNotCalls(t *testing.T) {
	path := copyFixtureTo(t, "codex/rollout-differencing.jsonl")
	sw, _, err := NewCodexParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	// The fixture also holds a session_meta, a turn_context and a response_item.
	// Only the two incrementing token_count events may become calls.
	if len(sw.Calls) != 2 {
		t.Errorf("parsed %d calls, want 2: a header or a user message became billable", len(sw.Calls))
	}
	if len(sw.ToolCalls) != 0 {
		t.Errorf("recorded %d tool calls, want 0: this rollout carries no tool descriptors", len(sw.ToolCalls))
	}
}
