package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// The v4 step drops Claude's cursors so every Claude log is re-read from the
// start, and touches nothing else: not another agent's cursors, and not a single
// stored call.
func TestV4ForgetsOnlyClaudeCursors(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "v4.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, s := range []model.ScanState{
		{Path: "/logs/claude.jsonl", Agent: model.AgentClaude, Offset: 100, Cursor: model.CursorBytes},
		{Path: "/logs/codex.jsonl", Agent: model.AgentCodex, Offset: 100, Cursor: model.CursorBytes},
	} {
		if err := st.SaveScanState(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.ReplaceSession(model.SessionWrite{
		Session: model.Session{UID: "c1", Agent: model.AgentClaude},
		Calls: []model.Call{{SessionUID: "c1", CallKey: "msg:x", Agent: model.AgentClaude,
			InputTokens: 1, TotalTokens: 1, Time: time.Unix(1773540000, 0)}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.applyMigration(v4ResetClaude); err != nil {
		t.Fatalf("v4 step: %v", err)
	}

	states, err := st.LoadScanStates()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states["/logs/claude.jsonl"]; ok {
		t.Error("the Claude cursor survived, so its log would resume under the old keys")
	}
	if _, ok := states["/logs/codex.jsonl"]; !ok {
		t.Error("another agent's cursor was dropped")
	}
	var calls int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM call`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("%d calls after the step, want 1: it must forget cursors, not data", calls)
	}
}

// A tool row written with no name is a tool_result completing a tool_use an
// earlier pass stored. It takes the outcome and keeps the stored name.
func TestToolRowWithoutNameKeepsStoredName(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "tool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	write := func(tc model.ToolCall) {
		t.Helper()
		tc.SessionUID, tc.Agent, tc.CallKey = "s", model.AgentClaude, "tool:toolu_1"
		if err := st.AppendSession(model.SessionWrite{
			Session:   model.Session{UID: "s", Agent: model.AgentClaude},
			ToolCalls: []model.ToolCall{tc},
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(model.ToolCall{Tool: "Bash", Time: time.Unix(1773540000, 0)})
	write(model.ToolCall{Time: time.Unix(1773540060, 0), IsError: true})

	var tool string
	var isErr, n int
	if err := st.db.QueryRow(
		`SELECT tool, is_error, (SELECT COUNT(*) FROM tool_call) FROM tool_call`).Scan(&tool, &isErr, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 || tool != "Bash" || isErr != 1 {
		t.Errorf("rows=%d tool=%q is_error=%d, want 1 row, Bash, failed", n, tool, isErr)
	}
}

// Reprice works from the stored columns, so the one-hour share of a call's
// cache writes has to be stored and handed back, or repricing would quietly
// bill every one-hour write at the cheaper five-minute rate.
func TestRepriceSeesOneHourCacheWrites(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "rp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.ReplaceSession(model.SessionWrite{
		Session: model.Session{UID: "c1", Agent: model.AgentClaude},
		Calls: []model.Call{{SessionUID: "c1", CallKey: "msg:x", Agent: model.AgentClaude,
			CacheWriteTokens: 1000, CacheWrite1hTokens: 400, TotalTokens: 1000,
			Time: time.Unix(1773540000, 0)}},
	}); err != nil {
		t.Fatal(err)
	}
	var gotCW, got1h int64
	if _, err := st.Reprice(context.Background(), func(_ string, _, _, _, cw, cw1h int64) (float64, bool) {
		gotCW, got1h = cw, cw1h
		return 1, true
	}); err != nil {
		t.Fatal(err)
	}
	if gotCW != 1000 || got1h != 400 {
		t.Errorf("reprice saw cache write %d / 1h %d, want 1000 / 400", gotCW, got1h)
	}
}
