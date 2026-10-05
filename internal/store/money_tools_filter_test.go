package store

import (
	"context"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// Every dashboard view takes the same filter axes, and a model is the axis a
// user is most likely to reach for. On the two views built over tool_call it did
// not work at all: they called the generic where("t", "ts"), which emits
// `t.model IN (...)`, and tool_call has no model column. Every request with
// ?model= set returned
//
//	500 tools: SQL logic error: no such column: t.model
//
// since the original commit. The filter did not fail quietly — it took the whole
// view down, so a page that had worked since before the first release could not
// be loaded at all.

// toolFilterStore holds two sessions, each with one call and one tool call, on
// different models, so a model filter either selects the right one or selects
// nothing.
func toolFilterStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir() + "/tools.db")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Hour)
	for _, spec := range []struct{ uid, agent, project, modelName string }{
		{"s-claude", model.AgentClaude, "/p1", "claude-opus-4-8"},
		{"s-gemini", model.AgentGemini, "/p2", "gemini-2.5-pro"},
	} {
		sess := model.SessionWrite{}
		sess.UID, sess.Agent, sess.Project = spec.uid, spec.agent, spec.project
		sess.Calls = []model.Call{{
			SessionUID: spec.uid, CallKey: "c1", Agent: spec.agent, Project: spec.project,
			Model: spec.modelName, Time: base, InputTokens: 100, OutputTokens: 50,
			TotalTokens: 150, CostUSD: 1, Priced: true,
		}}
		sess.ToolCalls = []model.ToolCall{{
			SessionUID: spec.uid, CallKey: "t1", Agent: spec.agent, Project: spec.project,
			Tool: "Read", Time: base, Seconds: 2,
		}}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(spec.uid); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

// TestToolsAcceptAModelFilter is the regression: it must not error, and it must
// select the tools of the sessions that ran the model rather than all of them.
func TestToolsAcceptAModelFilter(t *testing.T) {
	st := toolFilterStore(t)
	defer st.Close()
	ctx := context.Background()

	got, err := st.Tools(ctx, Filter{Models: []string{"claude-opus-4-8"}})
	if err != nil {
		t.Fatalf("Tools with a model filter: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tool rows, want 1", len(got))
	}
	if got[0].Calls != 1 {
		t.Errorf("tool calls = %d, want 1: the filter selected the wrong session's tools", got[0].Calls)
	}

	// The other model must select the other session, not everything: a filter
	// that matched all rows would answer the page correctly while being wrong.
	other, err := st.Tools(ctx, Filter{Models: []string{"gemini-2.5-pro"}})
	if err != nil {
		t.Fatalf("Tools with the other model filter: %v", err)
	}
	if len(other) != 1 || other[0].Calls != 1 {
		t.Errorf("got %+v, want the one Gemini tool call", other)
	}

	// Every axis at once, since a page can carry them all.
	if _, err := st.Tools(ctx, Filter{
		Models: []string{"claude-opus-4-8"}, Agents: []string{model.AgentClaude},
		Projects: []string{"/p1"},
	}); err != nil {
		t.Errorf("Tools with every axis: %v", err)
	}
	// A date window on top of a model, which is a different ts column handling.
	from, to := time.Now().Add(-48*time.Hour), time.Now()
	if _, err := st.Tools(ctx, Filter{Models: []string{"claude-opus-4-8"}, DateFrom: &from, DateTo: &to}); err != nil {
		t.Errorf("Tools with a model and a date window: %v", err)
	}
}

// TestProjectToolsAcceptAModelFilter is the same regression on the per-project
// breakdown, which had the identical clause.
func TestProjectToolsAcceptAModelFilter(t *testing.T) {
	st := toolFilterStore(t)
	defer st.Close()
	ctx := context.Background()

	got, err := st.ProjectTools(ctx, Filter{Models: []string{"gemini-2.5-pro"}})
	if err != nil {
		t.Fatalf("ProjectTools with a model filter: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d project rows, want 1", len(got))
	}
	if got[0].Project != "/p2" {
		t.Errorf("project = %q, want /p2", got[0].Project)
	}
	if _, err := st.ProjectTools(ctx, Filter{
		Models:   []string{"claude-opus-4-8"},
		Agents:   []string{model.AgentClaude},
		Projects: []string{"/p1"},
	}); err != nil {
		t.Errorf("ProjectTools with every axis: %v", err)
	}
}

// TestToolModelFilterExcludesOtherSessions is the negative half: filtering by a
// model no session used returns nothing rather than everything.
func TestToolModelFilterExcludesOtherSessions(t *testing.T) {
	st := toolFilterStore(t)
	defer st.Close()
	ctx := context.Background()

	got, err := st.Tools(ctx, Filter{Models: []string{"no-such-model-at-all"}})
	if err != nil {
		t.Fatalf("Tools with a model nothing used: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want 0: the filter matched a model no session ran", len(got))
	}
}
