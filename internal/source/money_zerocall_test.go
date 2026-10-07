package source

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// A record whose token counts are all zero is not a call that happened to be
// free — it is a record of a turn that never reached the model. Every agent
// writes one: Claude's interrupted-message `<synthetic>` turn is the committed
// example, and the committed fixture's own msg_04 has exactly that shape, kept
// out of the totals only because its model name happens to be `<synthetic>`.
//
// Storing these produced a row with Priced=true and CostUSD=0: a confident zero
// that reads as "this cost nothing" rather than "this is not known", and it
// inflated the call and message counts on every page. The empty-usage guard
// existed only for Antigravity; the rule is a property of the call, so it now
// holds for all six parsers.
//
// The rule is about tokens, not about price. A call that carries tokens under a
// model no rate resolves to is still stored, still counts, and is still marked
// unpriced — that is the safety net, and none of this may weaken it.

func writeJSONL(t *testing.T, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// zeroUsageCases is one all-zero-usage record per JSONL agent, written so a
// model that does resolve is named — the drop has to be about the tokens, not
// about the model being unpriceable.
func zeroUsageCases(t *testing.T) []struct {
	agent  string
	parser func() Parser
	path   string
} {
	t.Helper()
	return []struct {
		agent  string
		parser func() Parser
		path   string
	}{
		{
			agent:  "claude",
			parser: NewClaudeParser,
			path: writeJSONL(t, "session.jsonl",
				`{"type":"assistant","uuid":"u1","timestamp":"2026-05-01T09:00:00Z","cwd":"/p","message":{"id":"m1","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"Interrupted."}],"usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
				`{"type":"assistant","uuid":"u2","timestamp":"2026-05-01T09:00:05Z","cwd":"/p","message":{"id":"m2","role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":10,"output_tokens":20}}}`,
			),
		},
		{
			agent:  "gemini",
			parser: NewGeminiParser,
			path: writeJSONL(t, "session.jsonl",
				`{"sessionId":"g1","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-05-01T09:00:00Z","tokens":{"input":0,"output":0,"cached":0,"thoughtsTokenCount":0}}`,
				`{"sessionId":"g1","type":"gemini","model":"gemini-2.5-pro","timestamp":"2026-05-01T09:00:05Z","tokens":{"input":100,"output":50}}`,
			),
		},
		{
			agent:  "pi",
			parser: NewPiParser,
			path: writeJSONL(t, "session.jsonl",
				`{"type":"session","id":"p1","cwd":"/p","timestamp":"2026-05-01T09:00:00Z"}`,
				`{"type":"message","id":"m1","timestamp":"2026-05-01T09:00:01Z","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"reasoning":0}}}`,
				`{"type":"message","id":"m2","timestamp":"2026-05-01T09:00:05Z","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[],"usage":{"input":100,"output":50}}}`,
			),
		},
		{
			// A token_count whose reported per-call usage is entirely zero. It is
			// still a real event, so it is differenced and then dropped as a
			// non-call.
			agent:  "codex",
			parser: NewCodexParser,
			path: writeJSONL(t, "rollout.jsonl",
				`{"type":"session_meta","timestamp":"2026-05-01T09:00:00Z","payload":{"id":"c1","cwd":"/p"}}`,
				`{"type":"turn_context","timestamp":"2026-05-01T09:00:00.1Z","payload":{"model":"gpt-5.5"}}`,
				`{"type":"event_msg","timestamp":"2026-05-01T09:00:01Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0},"last_token_usage":{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0}}}}`,
				`{"type":"event_msg","timestamp":"2026-05-01T09:00:05Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":0},"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":0}}}}`,
			),
		},
	}
}

func TestZeroUsageRecordIsNotACall(t *testing.T) {
	for _, tc := range zeroUsageCases(t) {
		t.Run(tc.agent, func(t *testing.T) {
			sw, _, err := tc.parser().Parse(tc.path, model.ScanState{}, testPricer(t))
			if err != nil {
				t.Fatal(err)
			}
			checkInvariants(t, sw)
			if len(sw.Calls) != 1 {
				t.Fatalf("parsed %d calls, want 1 (the all-zero record is not a call)", len(sw.Calls))
			}
			c := sw.Calls[0]
			if c.TotalTokens == 0 {
				t.Error("the surviving call has no tokens, so the zero one was kept and the real one dropped")
			}
			// The confident zero itself: priced, worth nothing.
			if c.Priced && c.CostUSD == 0 {
				t.Error("a priced call costs $0.00")
			}
		})
	}
}

// TestAntigravityZeroUsageStepIsNotACall covers the fifth parser, which needs a
// conversation database rather than a JSONL file.
//
// The existing guard drops a usage sub-message that decodes to no fields at all.
// A usage sub-message that decodes to fields which are all zero is a different
// thing and went through as a call.
func TestAntigravityZeroUsageStepIsNotACall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (
		idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 0, has_subtrajectory numeric NOT NULL DEFAULT false,
		metadata blob, error_details blob, permissions blob, task_details blob,
		render_info blob, step_payload blob, step_format INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	// Step 1 reports a model and nothing else: every counter is zero. Step 2 is
	// a real call.
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, metadata) VALUES (1, 15, ?)`, agyStepMetadata(1_700_000_000, nil, 1037, 0, 0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, metadata) VALUES (2, 15, ?)`, agyStepMetadata(1_700_000_060, nil, 1037, 500, 120, 900, 40)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	sw, _, err := NewAntigravityParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1 (the all-zero usage block is not a call)", len(sw.Calls))
	}
	if got := sw.Calls[0].TotalTokens; got == 0 {
		t.Error("the surviving step has no tokens")
	}
}

// TestAntigravityStepWithoutStartTimestampStaysUndated makes sure a step whose
// metadata omits the start timestamp (field 1) is not stamped 1970-01-01. Such
// a call was landing in a real "day" bucket of 1970 and polluting the daily
// chart and month totals.
func TestAntigravityStepWithoutStartTimestampStaysUndated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conv.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (
		idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL DEFAULT 0,
		status INTEGER NOT NULL DEFAULT 0, has_subtrajectory numeric NOT NULL DEFAULT false,
		metadata blob, error_details blob, permissions blob, task_details blob,
		render_info blob, step_payload blob, step_format INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	// Usage block only, with no field-1 start timestamp.
	usage := protoVarint(1, 1037)
	usage = append(usage, protoVarint(2, 500)...)
	usage = append(usage, protoVarint(3, 120)...)
	usage = append(usage, protoVarint(5, 900)...)
	usage = append(usage, protoVarint(9, 40)...)
	meta := protoBytes(9, usage)
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, metadata) VALUES (1, 15, ?)`, meta); err != nil {
		t.Fatal(err)
	}
	db.Close()

	sv, _, err := NewAntigravityParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sv)
	if len(sv.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sv.Calls))
	}
	c := sv.Calls[0]
	if !c.Time.IsZero() {
		t.Errorf("call without a start timestamp is dated %v, want the zero time", c.Time)
	}
	if c.Day != "" {
		t.Errorf("call without a start timestamp has day %q, want empty", c.Day)
	}
}

// TestOpenCodeZeroUsageMessageIsNotACall covers the sixth.
func TestOpenCodeZeroUsageMessageIsNotACall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, title TEXT,
			agent TEXT, model TEXT, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO session VALUES ('s1','/p','t','opencode','{"id":"claude-sonnet-4-6"}',1,2)`); err != nil {
		t.Fatal(err)
	}
	empty, _ := json.Marshal(map[string]any{"role": "assistant", "tokens": map[string]any{
		"input": 0, "output": 0, "reasoning": 0,
	}})
	real, _ := json.Marshal(map[string]any{"role": "assistant", "tokens": map[string]any{
		"input": 300, "output": 90, "reasoning": 0,
	}})
	if _, err := db.Exec(`INSERT INTO message VALUES ('m1','s1',1,1,?)`, string(empty)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO message VALUES ('m2','s1',2,2,?)`, string(real)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	sw, _, err := NewOpenCodeParser(path).Parse("s1", model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1 (the all-zero tokens block is not a call)", len(sw.Calls))
	}
	if sw.Calls[0].TotalTokens == 0 {
		t.Error("the surviving message has no tokens")
	}
}

// TestUnknownModelWithTokensIsStillAnUnpricedCall is the half of the policy
// that must not move: dropping zero-token calls must not become dropping
// unpriceable ones. A call with tokens and no resolvable rate is still stored,
// still counts its tokens, and reports as unpriced rather than as free.
func TestUnknownModelWithTokensIsStillAnUnpricedCall(t *testing.T) {
	pricer := testPricer(t)
	if _, ok := pricer.Resolve("no-such-model-dashd-money-test"); ok {
		t.Skip("the probe model name resolves, so it cannot demonstrate the unpriced path")
	}
	cases := []struct {
		agent  string
		parser func() Parser
		path   string
	}{
		{
			agent:  "claude",
			parser: NewClaudeParser,
			path: writeJSONL(t, "session.jsonl",
				`{"type":"assistant","uuid":"u1","timestamp":"2026-05-01T09:00:00Z","cwd":"/p","message":{"id":"m1","role":"assistant","model":"no-such-model-dashd-money-test","content":[],"usage":{"input_tokens":100,"output_tokens":20}}}`,
			),
		},
		{
			agent:  "gemini",
			parser: NewGeminiParser,
			path: writeJSONL(t, "session.jsonl",
				`{"sessionId":"g1","type":"gemini","model":"no-such-model-dashd-money-test","timestamp":"2026-05-01T09:00:00Z","tokens":{"input":100,"output":20}}`,
			),
		},
		{
			agent:  "pi",
			parser: NewPiParser,
			path: writeJSONL(t, "session.jsonl",
				`{"type":"session","id":"p1","cwd":"/p","timestamp":"2026-05-01T09:00:00Z"}`,
				`{"type":"message","id":"m1","timestamp":"2026-05-01T09:00:01Z","message":{"role":"assistant","model":"no-such-model-dashd-money-test","content":[],"usage":{"input":100,"output":20}}}`,
			),
		},
		{
			agent:  "codex",
			parser: NewCodexParser,
			path: writeJSONL(t, "rollout.jsonl",
				`{"type":"session_meta","timestamp":"2026-05-01T09:00:00Z","payload":{"id":"c1","cwd":"/p"}}`,
				`{"type":"event_msg","timestamp":"2026-05-01T09:00:05Z","payload":{"type":"token_count","model":"no-such-model-dashd-money-test","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0},"last_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":20,"reasoning_output_tokens":0}}}}`,
			),
		},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			sw, _, err := tc.parser().Parse(tc.path, model.ScanState{}, pricer)
			if err != nil {
				t.Fatal(err)
			}
			checkInvariants(t, sw)
			if len(sw.Calls) != 1 {
				t.Fatalf("parsed %d calls, want 1: a call with tokens was dropped", len(sw.Calls))
			}
			c := sw.Calls[0]
			if c.Priced {
				t.Error("a model with no rate is marked priced")
			}
			if c.CostUSD != 0 {
				t.Errorf("cost = $%v, want 0: an unpriced call must not invent a figure", c.CostUSD)
			}
			if c.TotalTokens == 0 {
				t.Error("an unpriced call still has to count its tokens")
			}
		})
	}
}
