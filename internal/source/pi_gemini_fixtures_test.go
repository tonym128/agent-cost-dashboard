package source

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// Two committed fixtures that no test read.
//
// The pi and Gemini transcripts are the shapes those agents write, and both
// exercise the conventions that differ most from Anthropic's: pi's reasoning is a
// slice of its output figure, and Gemini's input figure *includes* its cached
// tokens so the two must be subtracted rather than summed. Both were committed
// and then left uncovered, which is the worst state for a fixture: it looks like
// evidence and is not.

// TestPiSessionFixture covers the pi transcript end to end.
//
// pi's shape: a `session` header carrying the id and cwd, then `message` records
// whose usage sits on the assistant message. The tool call is recorded from the
// toolCall content block, and its toolResult opens the next request window rather
// than becoming a second tool.
func TestPiSessionFixture(t *testing.T) {
	path := copyFixtureTo(t, "pi/session-pi-1.jsonl")
	p := NewPiParser()
	sw, state, err := p.Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	// The id comes from the header record, not the file name, so the same session
	// reads the same way whatever the log is called on disk.
	if got, want := sw.Session.UID, "test-session-pi"; got != want {
		t.Errorf("session uid = %q, want %q", got, want)
	}
	if got, want := p.SessionUID(path), sw.Session.UID; got != want {
		t.Errorf("SessionUID reported %q, want %q", got, want)
	}
	if sw.Session.Agent != model.AgentPi {
		t.Errorf("agent = %q, want %q", sw.Session.Agent, model.AgentPi)
	}
	if got, want := sw.Session.Project, "/home/testuser/project"; got != want {
		t.Errorf("project = %q, want %q", got, want)
	}
	if state.Cursor != model.CursorBytes || state.Offset <= 0 {
		t.Errorf("cursor = %q at offset %d, want a byte cursor past the file",
			state.Cursor, state.Offset)
	}
	if state.PrefixHash == "" || state.PrefixLen == 0 {
		t.Error("no head fingerprint recorded, so a later rewrite could not be detected")
	}

	// Three assistant messages, one of which carries no usage block at all. It
	// must not become a call: a call worth nothing would pad the call count
	// without adding anything billable.
	if len(sw.Calls) != 2 {
		t.Fatalf("parsed %d calls, want 2 (the message with no usage block is not one)",
			len(sw.Calls))
	}

	// Call 1, the plain one.
	first := sw.Calls[0]
	if first.CallKey != "msg:m2" {
		t.Errorf("call key = %q, want the message's own id", first.CallKey)
	}
	if first.InputTokens != 4200 || first.OutputTokens != 310 {
		t.Errorf("call 1 buckets = %d in / %d out, want 4200 / 310",
			first.InputTokens, first.OutputTokens)
	}
	checkGenerated(t, first, 310)
	if !first.Priced || first.CostUSD <= 0 {
		t.Errorf("call 1 is unpriced at $%v: claude-sonnet-4-6 resolves", first.CostUSD)
	}

	// Call 2 carries the four buckets and a reasoning figure. pi reports reasoning
	// as a slice of its output figure, so the two are halves of one number: this
	// is the assertion the tautological check in checkInvariants used to stand in
	// for.
	second := sw.Calls[1]
	if second.CallKey != "msg:m4" {
		t.Errorf("call key = %q, want msg:m4", second.CallKey)
	}
	if second.InputTokens != 1800 || second.CacheReadTokens != 12000 ||
		second.CacheWriteTokens != 2400 {
		t.Errorf("call 2 buckets = %+v, want 1800 in / 12000 cache read / 2400 cache write",
			second)
	}
	checkGenerated(t, second, 1900)
	if second.ReasoningTokens <= 0 {
		t.Errorf("reasoning = %d, want the reported 640 itemised out of the output",
			second.ReasoningTokens)
	}

	// Cost is derived from the resolved rates rather than a hardcoded figure, so a
	// vendor price change cannot break it — and because reasoning is billed as
	// output, the cost must use the whole generated count.
	rates, ok := testPricer(t).Resolve("claude-sonnet-4-6")
	if !ok {
		t.Fatal("claude-sonnet-4-6 is unpriced")
	}
	//
	// KNOWN RED on this branch: pi's reasoning is billed at the input rate, or
	// not at all — the parser subtracts it from output and then prices the
	// remainder, so 640 generated tokens are billed as if they were 1260 of
	// visible output. jsonl_agents.go is not a file this branch owns; the
	// expectation below is stated for the fixed behaviour, in which the whole
	// generated count is billed at the output rate the way Anthropic's is.
	want := float64(second.InputTokens)/1e6*rates.Input +
		1900/1e6*rates.Output +
		float64(second.CacheReadTokens)/1e6*rates.CacheRead +
		float64(second.CacheWriteTokens)/1e6*rates.CacheWrite
	if diff := second.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("call 2 cost $%.8f, want $%.8f: the whole generated count is billed "+
			"at the output rate, not the itemised remainder — a thinking-heavy turn "+
			"priced on its remainder alone is a fraction of what it cost",
			second.CostUSD, want)
	}

	// One tool call, from the toolCall block. The toolResult that follows opens the
	// next request window and must not be recorded as a second tool.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1 (a toolResult is not a second tool)",
			len(sw.ToolCalls))
	}
	if sw.ToolCalls[0].Tool != "Bash" {
		t.Errorf("tool = %q, want Bash", sw.ToolCalls[0].Tool)
	}
	// The result carries a start and an end, so the duration is measured rather
	// than assumed to be zero.
	if got := sw.ToolCalls[0].Seconds; got != 2 {
		t.Errorf("tool seconds = %v, want 2 from the result's own timestamps", got)
	}
	if sw.ToolCalls[0].IsError {
		t.Error("a successful tool result was recorded as an error")
	}

	if !sw.FirstTS.Before(sw.LastTS) {
		t.Errorf("first %s is not before last %s", sw.FirstTS, sw.LastTS)
	}
}

// TestPiFixtureIncrementalScanReturnsOnlyTheIncrement is the pi half of the
// property the whole incremental design rests on.
func TestPiFixtureIncrementalScanReturnsOnlyTheIncrement(t *testing.T) {
	path := copyFixtureTo(t, "pi/session-pi-1.jsonl")
	p := NewPiParser()
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
		t.Errorf("an unchanged log returned %d calls, want 0", len(again.Calls))
	}

	// Append one assistant turn and rescan from the stored cursor.
	f, err := openAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"message","timestamp":"2026-05-06T09:01:00.000Z",` +
		`"id":"m6","message":{"role":"assistant","model":"claude-sonnet-4-6","usage":` +
		`{"input":100,"output":50}}}` + "\n"); err != nil {
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
	if third.Calls[0].CallKey != "msg:m6" {
		t.Errorf("call key = %q, want the appended message's id", third.Calls[0].CallKey)
	}
	if third.Calls[0].SessionUID != "test-session-pi" {
		t.Errorf("the increment lost the session uid: %q", third.Calls[0].SessionUID)
	}
}

// TestGeminiSessionFixture covers the Gemini transcript.
//
// Gemini's convention is the opposite of Anthropic's in the way that matters most:
// its `input` figure *includes* the cached tokens, so the stored input is the net
// after subtracting them. Adding instead would double count the cached part, and
// the cache rate is a tenth of the input rate, so the cost would be visibly wrong.
// Gemini also reports thinking separately as thoughtsTokenCount, carved out of
// the same output figure.
func TestGeminiSessionFixture(t *testing.T) {
	// Gemini's project is recovered from the path, so the fixture's directory
	// layout is load-bearing and copyFixtureTo (which flattens to the base name)
	// cannot be used: <project>/chats/<file>.jsonl is what the parser looks for.
	src := testdataPath(t, "gemini/example-project/chats/session-2026-05-05T11-00-0.testgemin.jsonl")
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "example-project", "chats")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filepath.Base(src))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	histDir := filepath.Join(home, ".gemini", "history", "example-project")
	if err := os.MkdirAll(histDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const wantProject = "/home/testuser/project"
	if err := os.WriteFile(filepath.Join(histDir, ".project_root"),
		[]byte(wantProject+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := NewGeminiParser()
	sw, state, err := p.Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)

	// The session id comes from the header record's sessionId.
	if got, want := sw.Session.UID, "test-session-gemini"; got != want {
		t.Errorf("session uid = %q, want %q", got, want)
	}
	if sw.Session.Agent != model.AgentGemini {
		t.Errorf("agent = %q, want %q", sw.Session.Agent, model.AgentGemini)
	}
	if sw.Session.Project != wantProject {
		t.Errorf("project = %q, want %q recovered from the history directory",
			sw.Session.Project, wantProject)
	}
	if state.Cursor != model.CursorBytes || state.Offset <= 0 {
		t.Errorf("cursor = %q at offset %d, want a byte cursor past the file",
			state.Cursor, state.Offset)
	}

	// The header and the two `info` records carry no tokens block, so they are not
	// turns. Treating them as turns aborted the rest of the file once already.
	if len(sw.Calls) != 2 {
		t.Fatalf("parsed %d calls, want 2 (the header and the info records are not turns)",
			len(sw.Calls))
	}

	// Call 1: cached tokens are a subset of input, so input is the net.
	first := sw.Calls[0]
	if first.InputTokens != 12000-8000 {
		t.Errorf("call 1 input = %d, want %d: Gemini's input figure includes its "+
			"cached tokens, so the stored input is the net",
			first.InputTokens, 12000-8000)
	}
	if first.CacheReadTokens != 8000 {
		t.Errorf("call 1 cache read = %d, want 8000", first.CacheReadTokens)
	}
	// Thinking is a slice of the output figure, reported separately as
	// thoughtsTokenCount.
	if first.ReasoningTokens != 250 {
		t.Errorf("call 1 reasoning = %d, want the reported 250", first.ReasoningTokens)
	}
	checkGenerated(t, first, 900)
	if first.CacheWriteTokens != 0 {
		t.Errorf("call 1 cache write = %d, want 0: the fixture reports none",
			first.CacheWriteTokens)
	}
	if got, want := first.Model, "gemini-3-flash-preview"; got != want {
		t.Errorf("call 1 model = %q, want %q", got, want)
	}
	if !first.Priced || first.CostUSD <= 0 {
		t.Errorf("call 1 is unpriced at $%v", first.CostUSD)
	}
	// Cost derived from the resolved rates: the cached part at the cache rate and
	// the whole generated count at the output rate.
	rates, ok := testPricer(t).Resolve("gemini-3-flash-preview")
	if !ok {
		t.Fatal("gemini-3-flash-preview is unpriced")
	}
	//
	// KNOWN RED on this branch, for the same reason as the pi assertion above:
	// Gemini's thinking is carved out of the output figure and then the remainder
	// is priced, so 250 generated tokens are not billed at all.
	wantCost := float64(first.InputTokens)/1e6*rates.Input +
		900/1e6*rates.Output +
		float64(first.CacheReadTokens)/1e6*rates.CacheRead
	if diff := first.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("call 1 cost $%.8f, want $%.8f (cache at the cache rate, the whole "+
			"generated count at the output rate)", first.CostUSD, wantCost)
	}

	// Call 2: no thinking, and the same cached-is-inside-input convention.
	second := sw.Calls[1]
	if second.InputTokens != 22000-21000 {
		t.Errorf("call 2 input = %d, want %d", second.InputTokens, 22000-21000)
	}
	if second.CacheReadTokens != 21000 {
		t.Errorf("call 2 cache read = %d, want 21000", second.CacheReadTokens)
	}
	if second.ReasoningTokens != 0 {
		t.Errorf("call 2 reasoning = %d, want 0: the fixture reports no thinking",
			second.ReasoningTokens)
	}
	checkGenerated(t, second, 1500)

	// One tool call from the turn's toolCalls.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1", len(sw.ToolCalls))
	}
	if got, want := sw.ToolCalls[0].Tool, "read_file"; got != want {
		t.Errorf("tool = %q, want %q", got, want)
	}

	// Timestamps come off each turn, and the header's startTime is the earliest.
	if !sw.FirstTS.Before(sw.LastTS) {
		t.Errorf("first %s is not before last %s", sw.FirstTS, sw.LastTS)
	}
	// The header's startTime is not itself observed: only a turn contributes a
	// timestamp, so the earliest recorded time is the first user turn. Recorded
	// rather than asserted as the header's, because the difference is invisible on
	// the page and naming it keeps the next reader from wondering.
	if got, want := sw.FirstTS.UTC().Format(time.RFC3339), "2026-05-05T11:00:01Z"; got != want {
		t.Errorf("first timestamp = %s, want the first turn's %s", got, want)
	}
}
