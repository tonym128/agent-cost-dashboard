package source

import (
	"os"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// The Claude fixtures are the shapes a real Claude Code transcript contains, with
// every identifier and path replaced. They exist so a change to the log format
// fails a test rather than quietly changing what the dashboard bills.

// openAppend opens a fixture for appending, so a test can simulate an agent
// extending a live transcript rather than rewriting it.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
}

// sessionTotals sums one session's calls.
type sessionTotals struct {
	input, output, cacheRead, cacheWrite, reasoning int
	cost                                            float64
	priced                                          int
}

func totalsOf(sw model.SessionWrite) sessionTotals {
	var t sessionTotals
	for _, c := range sw.Calls {
		t.input += c.InputTokens
		t.output += c.OutputTokens
		t.cacheRead += c.CacheReadTokens
		t.cacheWrite += c.CacheWriteTokens
		t.reasoning += c.ReasoningTokens
		t.cost += c.CostUSD
		if c.Priced {
			t.priced++
		}
	}
	return t
}

// checkInvariants asserts the properties that must hold for every call of every
// parser, whatever the log said. It is the assertion the whole file is built
// around: a wrong number is almost always a number that breaks one of these.
func checkInvariants(t *testing.T, sw model.SessionWrite) {
	t.Helper()
	// The generated-count partition is *not* asserted here, because it cannot be:
	// a call's OutputTokens and ReasoningTokens are two halves of one number the
	// log states, and that number is not on the call. It used to be asserted here
	// as `ReasoningTokens > OutputTokens+ReasoningTokens`, which reduces to
	// `0 > OutputTokens` and so only ever fired on a negative output the loop above
	// had already caught — removing the Codex reasoning clamp while neutering that
	// line left the whole package green.
	//
	// The real invariant is checked by checkGenerated, at each site where the
	// fixture's own generated count is known.
	if sw.Session.UID == "" {
		t.Error("session has no uid: every stored row hangs off one")
	}
	keys := map[string]bool{}
	for _, c := range sw.Calls {
		switch {
		case c.CallKey == "":
			t.Errorf("call from %s has no call key", c.Time)
		case keys[c.CallKey]:
			t.Errorf("duplicate call key %q within one session", c.CallKey)
		}
		keys[c.CallKey] = true
		if c.SessionUID != sw.Session.UID {
			t.Errorf("call %q has session %q, want %q", c.CallKey, c.SessionUID, sw.Session.UID)
		}
		if c.Agent != sw.Session.Agent {
			t.Errorf("call %q has agent %q, want %q", c.CallKey, c.Agent, sw.Session.Agent)
		}
		for name, v := range map[string]int{
			"input": c.InputTokens, "output": c.OutputTokens,
			"cache read": c.CacheReadTokens, "cache write": c.CacheWriteTokens,
			"reasoning": c.ReasoningTokens, "total": c.TotalTokens,
		} {
			if v < 0 {
				t.Errorf("call %q has %d %s tokens", c.CallKey, v, name)
			}
		}
		if c.CostUSD < 0 {
			t.Errorf("call %q has a negative cost of %v", c.CallKey, c.CostUSD)
		}
		// A priced call that costs nothing is a confident wrong number, and it
		// defeats the unpriced safety net.
		if c.Priced && c.CostUSD == 0 && c.TotalTokens > 0 {
			t.Errorf("call %q is marked priced and costs $0.00", c.CallKey)
		}
	}
	for _, tc := range sw.ToolCalls {
		if tc.SessionUID != sw.Session.UID {
			t.Errorf("tool call %q has session %q, want %q", tc.CallKey, tc.SessionUID, sw.Session.UID)
		}
		if tc.Seconds < 0 {
			t.Errorf("tool call %q has a negative duration", tc.CallKey)
		}
	}
}

// TestClaudeSessionFixture covers the whole transcript: session identity, the
// project, per-message token attribution across all four Anthropic buckets,
// reasoning itemised out of the generated count, and the records that must
// contribute nothing.
func TestClaudeSessionFixture(t *testing.T) {
	path := copyFixtureTo(t, "claude/session-1.jsonl")
	pricer := testPricer(t)

	p := NewClaudeParser()
	sw, state, err := p.Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	// The file name is the session id for this agent: uidFrom is empty, so the
	// stem is used and stays stable across scans.
	if got, want := sw.Session.UID, "session-1"; got != want {
		t.Errorf("session uid = %q, want %q", got, want)
	}
	if state.SessionUID != sw.Session.UID {
		t.Errorf("cursor session uid = %q, want %q", state.SessionUID, sw.Session.UID)
	}
	if sw.Session.Agent != model.AgentClaude {
		t.Errorf("agent = %q, want %q", sw.Session.Agent, model.AgentClaude)
	}
	if got, want := sw.Session.Project, "/home/testuser/project"; got != want {
		t.Errorf("project = %q, want %q", got, want)
	}
	if got := p.SessionUID(path); got != sw.Session.UID {
		t.Errorf("SessionUID reported %q, want %q", got, sw.Session.UID)
	}
	if got := p.Agent(); got != model.AgentClaude {
		t.Errorf("Agent = %q, want %q", got, model.AgentClaude)
	}
	if state.Cursor != model.CursorBytes {
		t.Errorf("cursor = %q, want %q", state.Cursor, model.CursorBytes)
	}
	if state.Offset <= 0 {
		t.Error("cursor offset is zero after a full read")
	}
	if state.PrefixHash == "" || state.PrefixLen == 0 {
		t.Error("no head fingerprint recorded, so a later rewrite could not be detected")
	}

	// Four assistant records carry a usage block, but only two become calls:
	// msg_03 has no usage at all, and msg_04 is a "<synthetic>" record with a
	// block of zeroes, which the parser rejects by model name. Both exclusions
	// are deliberate — a zero-token synthetic row would otherwise pad the call
	// count without adding anything billable.
	if len(sw.Calls) != 2 {
		t.Fatalf("parsed %d calls, want 2 (the synthetic-model and no-usage records are not calls)", len(sw.Calls))
	}

	// The four buckets are disjoint per Anthropic, so they are summed rather
	// than subtracted — the opposite of the Gemini convention. A reasoning value
	// of -1 means "derived from the thinking block by a characters-per-token
	// estimate", so it is asserted as positive rather than as a magic number.
	want := []struct {
		key                                                string
		input, cacheRead, cacheWrite, generated, reasoning int
	}{
		{"msg:22222222-2222-4222-8222-222222222222", 120, 8000, 2000, 410, 0},
		{"msg:44444444-4444-4444-8444-444444444444", 340, 18000, 0, 1800, -1},
	}
	for i, w := range want {
		got := sw.Calls[i]
		if got.CallKey != w.key {
			t.Errorf("call %d key = %q, want %q", i, got.CallKey, w.key)
		}
		if got.InputTokens != w.input {
			t.Errorf("%s input = %d, want %d", w.key, got.InputTokens, w.input)
		}
		if got.CacheReadTokens != w.cacheRead {
			t.Errorf("%s cache read = %d, want %d", w.key, got.CacheReadTokens, w.cacheRead)
		}
		if got.CacheWriteTokens != w.cacheWrite {
			t.Errorf("%s cache write = %d, want %d", w.key, got.CacheWriteTokens, w.cacheWrite)
		}
		switch {
		case w.reasoning < 0 && got.ReasoningTokens <= 0:
			t.Errorf("%s reasoning = %d, want a positive estimate from its thinking block",
				w.key, got.ReasoningTokens)
		case w.reasoning >= 0 && got.ReasoningTokens != w.reasoning:
			t.Errorf("%s reasoning = %d, want %d", w.key, got.ReasoningTokens, w.reasoning)
		}
		// Output is the visible remainder: the generated count less reasoning.
		checkGenerated(t, got, w.generated)
		if got.Model != "claude-opus-4-8" {
			t.Errorf("%s model = %q", w.key, got.Model)
		}
	}

	// The per-window cache_creation breakdown underneath the aggregate must not
	// be added as well, or cache writes double count.
	first := sw.Calls[0]
	if first.CacheWriteTokens != 2000 {
		t.Errorf("cache write = %d, want the aggregate 2000, not the aggregate plus its 5m window",
			first.CacheWriteTokens)
	}

	// Totals reconcile: the session is the sum of its calls and nothing else.
	tot := totalsOf(sw)
	if tot.input != 460 || tot.cacheRead != 26000 || tot.cacheWrite != 2000 {
		t.Errorf("totals = %+v", tot)
	}

	// One tool call is recorded, from the tool_use block. The matching
	// tool_result arrives in a later *user* message and is deliberately not
	// recorded a second time: the two halves are one tool call, and recording
	// both would double the call count the Tool Usage table shows. Timing is
	// recovered separately, by attributing a tool to the first LLM call after
	// it in internal/store/queries.go.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1 (one tool_use; its tool_result is not counted again)",
			len(sw.ToolCalls))
	}
	if sw.ToolCalls[0].Tool != "Read" {
		t.Errorf("tool = %q, want Read", sw.ToolCalls[0].Tool)
	}
	if !sw.FirstTS.Before(sw.LastTS) {
		t.Errorf("first %s is not before last %s", sw.FirstTS, sw.LastTS)
	}
}

// TestClaudeThinkingBlockBecomesReasoning covers the itemisation the itemised
// reasoning tokens depend on: Anthropic bills thinking at the output rate and
// folds it into output_tokens, so the split has to be reconstructed from the
// content block or a thinking-heavy session shows one opaque output line.
func TestClaudeThinkingBlockBecomesReasoning(t *testing.T) {
	path := copyFixtureTo(t, "claude/session-1.jsonl")
	pricer := testPricer(t)
	sw, _, err := NewClaudeParser().Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	thinking := sw.Calls[1]
	if thinking.ReasoningTokens <= 0 {
		t.Fatalf("reasoning = %d, want the thinking block itemised", thinking.ReasoningTokens)
	}
	if thinking.OutputTokens == 1800 {
		t.Error("output still holds the whole generated count: reasoning was not split out")
	}

	// The cost is unchanged by the split, because the whole generated count is
	// billed at the output rate.
	rates, ok := pricer.Resolve("claude-opus-4-8")
	if !ok {
		t.Fatal("claude-opus-4-8 is unpriced")
	}
	generated := thinking.OutputTokens + thinking.ReasoningTokens
	want := float64(thinking.InputTokens)/1e6*rates.Input +
		float64(generated)/1e6*rates.Output +
		float64(thinking.CacheReadTokens)/1e6*rates.CacheRead +
		float64(thinking.CacheWriteTokens)/1e6*rates.CacheWrite
	if diff := thinking.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost $%.8f, want $%.8f", thinking.CostUSD, want)
	}
	if !thinking.Priced {
		t.Error("the call is not marked priced")
	}
}

// TestClaudeRecordsThatContributeNothing is the negative half: a record with no
// usage, and a synthetic-model record whose usage is all zeroes, must not
// inflate a total.
func TestClaudeRecordsThatContributeNothing(t *testing.T) {
	path := copyFixtureTo(t, "claude/session-1.jsonl")
	pricer := testPricer(t)
	sw, _, err := NewClaudeParser().Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range sw.Calls {
		if c.Model == "<synthetic>" {
			t.Error("a synthetic-model record became a priced call")
		}
	}
	tot := totalsOf(sw)
	if tot.input != 460 {
		t.Errorf("input total %d: a usage-less record contributed tokens", tot.input)
	}
	// 410 + 1800, the two generated counts the fixture's usage blocks state. The
	// synthetic record's zeroes must not appear here: contributing them would add
	// a call worth nothing to the call count without changing this sum, which is
	// why the call count is asserted separately above.
	if tot.output+tot.reasoning != 2210 {
		t.Errorf("generated total %d, want 2210 (the sum of the two usage blocks)", tot.output+tot.reasoning)
	}
	// Per call, so a parser that folded one record's reasoning into another's
	// output cannot hide behind an aggregate that happens to add up.
	checkGenerated(t, sw.Calls[0], 410)
	checkGenerated(t, sw.Calls[1], 1800)
	if tot.cost <= 0 {
		t.Errorf("cost %v, want a positive total", tot.cost)
	}
}

// TestClaudeDatedModelNormalises is why NormalizeModel exists: a dated snapshot
// id has to reduce to the same key as the bare model or the whole session
// prices at zero and shows as unpriced.
func TestClaudeDatedModelNormalises(t *testing.T) {
	path := copyFixtureTo(t, "claude/session-2-dated-model.jsonl")
	pricer := testPricer(t)
	sw, _, err := NewClaudeParser().Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	c := sw.Calls[0]
	// The raw id is kept on the call — it is what the log said — but the pricer
	// has to find it under the undated key.
	if c.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("model = %q, want the id as written in the log", c.Model)
	}
	if got := NormalizeModel(c.Model); got != "claude-sonnet-4-5" {
		t.Errorf("NormalizeModel = %q, want claude-sonnet-4-5", got)
	}
	if !c.Priced {
		t.Fatal("a dated model id priced as unpriced")
	}
	undated, ok := pricer.Cost("claude-sonnet-4-5", c.InputTokens, c.OutputTokens, 0, c.CacheWriteTokens)
	if !ok {
		t.Fatal("the undated id does not resolve either")
	}
	if diff := c.CostUSD - undated; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("dated id cost $%.8f, undated $%.8f", c.CostUSD, undated)
	}
	if c.InputTokens != 512 || c.CacheWriteTokens != 1500 {
		t.Errorf("buckets = %d input / %d cache write, want 512 / 1500",
			c.InputTokens, c.CacheWriteTokens)
	}
}

// TestClaudeIncrementalScanAppendsRatherThanRecounts guards the property the
// whole incremental design rests on: a second pass over a grown log returns only
// what is new.
func TestClaudeIncrementalScanAppendsRatherThanRecounts(t *testing.T) {
	path := copyFixtureTo(t, "claude/session-1.jsonl")
	pricer := testPricer(t)
	p := NewClaudeParser()

	_, state, err := p.Parse(path, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := p.Parse(path, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Calls) != 0 {
		t.Errorf("an unchanged log returned %d calls, want 0", len(second.Calls))
	}

	// Append a new assistant turn and rescan from the stored cursor.
	f, err := openAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","sessionId":"test-session-1",` +
		`"cwd":"/home/testuser/project","uuid":"88888888-8888-4888-8888-888888888888",` +
		`"timestamp":"2026-05-01T09:05:00.000Z","message":{"model":"claude-opus-4-8",` +
		`"content":[{"type":"text","text":"One more."}],"usage":{"input_tokens":10,` +
		`"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	third, _, err := p.Parse(path, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, third)
	if len(third.Calls) != 1 {
		t.Fatalf("the increment returned %d calls, want 1", len(third.Calls))
	}
	c := third.Calls[0]
	if c.CallKey != "msg:88888888-8888-4888-8888-888888888888" {
		t.Errorf("call key = %q, want the appended message's uuid", c.CallKey)
	}
	if c.InputTokens != 10 || c.OutputTokens != 20 || c.CacheReadTokens != 30 || c.CacheWriteTokens != 40 {
		t.Errorf("appended buckets = %+v", c)
	}
	if c.SessionUID != "session-1" {
		t.Errorf("the increment lost the session uid: %q", c.SessionUID)
	}
}

// checkGenerated asserts the partition invariant the tautological check in
// checkInvariants was standing in for: for a record whose generated count the log
// states, the visible output and the reasoning are two halves of that one number,
// never an addition to it.
//
// Anthropic bills extended thinking inside output_tokens, so without this the
// reasoning figure is not verifiable at all: a parser that dropped the split
// entirely, or double-counted the thinking, both produce an OutputTokens +
// ReasoningTokens that differs from the log, which is exactly what a summed
// assertion catches.
//
// generated is the count as written in the log, not one derived from the parse.
func checkGenerated(t *testing.T, c model.Call, generated int) {
	t.Helper()
	if got := c.OutputTokens + c.ReasoningTokens; got != generated {
		t.Errorf("call %q: output %d + reasoning %d = %d, want the %d the log reports "+
			"as generated: reasoning is a slice of that count, not an addition to it",
			c.CallKey, c.OutputTokens, c.ReasoningTokens, got, generated)
	}
}
