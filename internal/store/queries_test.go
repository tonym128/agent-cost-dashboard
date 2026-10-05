package store

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tonym/agent-cost-dashboard/internal/model"
)

func newFilterStore(t *testing.T) (*Store, func()) {
	t.Helper()
	st, err := Open(t.TempDir() + "/f.db")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)

	add := func(uid, agent, project, modelName string, at time.Time) {
		sess := model.SessionWrite{}
		sess.UID, sess.Agent, sess.Project = uid, agent, project
		sess.Calls = append(sess.Calls, oneCall(uid, agent, project, modelName, at))
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}
	// Four calls on one axis each, so a filter that reaches only some of them
	// gives a wrong answer rather than an obviously empty one.
	add("a", "pi", "/p1", "m1", base)
	add("b", "claude", "/p2", "m2", base.Add(time.Hour))
	add("c", "agy", "/p3", "m3", base.Add(2*time.Hour))
	add("d", "opencode", "/p4", "m4", base.Add(3*time.Hour))
	return st, func() { st.Close() }
}

func oneCall(uid, agent, project, modelName string, at time.Time) model.Call {
	return model.Call{
		SessionUID: uid, CallKey: "c", Agent: agent, Project: project,
		Model: modelName, Time: at, InputTokens: 10, OutputTokens: 5,
		TotalTokens: 15, CostUSD: 1, Priced: true,
	}
}

// TestActivityAcceptsEveryFilterAxis guards the one query that used to build its
// WHERE clause by splicing a finished string.
//
// Trimming the " WHERE " prefix off the filter clause also trimmed the space
// after it, so the query read "ts <= ?AND ts >= ?". It worked with no filter —
// which is what the page loads by default — and failed the moment a date,
// model, agent or project filter was applied.
func TestActivityAcceptsEveryFilterAxis(t *testing.T) {
	st, done := newFilterStore(t)
	defer done()

	ctx := context.Background()
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
	from, to := base.Add(-time.Hour), base.Add(24*time.Hour)
	cut := base.Add(90 * time.Minute)

	cases := []struct {
		name   string
		filter Filter
		want   int
	}{
		{"none", Filter{}, 4},
		{"model", Filter{Models: []string{"m2"}}, 1},
		{"agent", Filter{Agents: []string{"pi"}}, 1},
		{"project", Filter{Projects: []string{"/p3"}}, 1},
		{"date from", Filter{DateFrom: &cut}, 2},
		{"date to", Filter{DateTo: &cut}, 2},
		{"both dates", Filter{DateFrom: &base, DateTo: &cut}, 2},
		{"several axes", Filter{Agents: []string{"claude", "agy"}, DateFrom: &cut}, 1},
		{"no match", Filter{Models: []string{"nothing"}}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buckets, err := st.Activity(ctx, tc.filter, from, to, 3600)
			if err != nil {
				t.Fatalf("query failed: %v", err)
			}
			var calls int
			for _, b := range buckets {
				calls += b.Calls
			}
			if calls != tc.want {
				t.Errorf("calls = %d, want %d", calls, tc.want)
			}
		})
	}
}

// TestActivityWindowAndFilterAgree checks the two halves narrow together: the
// window bounds and the filter must intersect, not override one another.
func TestActivityWindowAndFilterAgree(t *testing.T) {
	st, done := newFilterStore(t)
	defer done()

	ctx := context.Background()
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)

	// A window that excludes everything, with a filter that would match.
	buckets, err := st.Activity(ctx, Filter{Agents: []string{"pi"}},
		base.Add(-time.Hour), base.Add(-30*time.Minute), 3600)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range buckets {
		if b.Calls != 0 {
			t.Errorf("window excludes the call but %d came back", b.Calls)
		}
	}
}

// TestEveryFilteredQueryAgreesOnTheSameFilter: the risk with a shared filter is
// that one query applies it differently from another, and the page then shows
// a total that disagrees with the table under it.
func TestEveryFilteredQueryAgreesOnTheSameFilter(t *testing.T) {
	st, done := newFilterStore(t)
	defer done()

	ctx := context.Background()
	base := time.Now().Add(-48 * time.Hour).Truncate(time.Hour)
	from, to := base.Add(-time.Hour), base.Add(24*time.Hour)
	cut := base.Add(90 * time.Minute)

	for _, f := range []Filter{
		{},
		{Agents: []string{"agy"}},
		{Models: []string{"m1", "m4"}},
		{DateFrom: &cut},
		{DateTo: &cut},
		{Agents: []string{"claude", "agy"}, DateFrom: &cut},
	} {
		totals, err := st.Totals(ctx, f)
		if err != nil {
			t.Fatalf("Totals(%+v): %v", f, err)
		}
		models, err := st.Models(ctx, f)
		if err != nil {
			t.Fatalf("Models(%+v): %v", f, err)
		}
		daily, err := st.Daily(ctx, f)
		if err != nil {
			t.Fatalf("Daily(%+v): %v", f, err)
		}
		buckets, err := st.Activity(ctx, f, from, to, 3600)
		if err != nil {
			t.Fatalf("Activity(%+v): %v", f, err)
		}

		var fromModels, fromActivity int
		var fromDailyCost float64
		for _, m := range models {
			fromModels += m.Calls
		}
		for _, d := range daily {
			fromDailyCost += d.Cost
		}
		for _, b := range buckets {
			fromActivity += b.Calls
		}
		if fromModels != totals.Calls {
			t.Errorf("%+v: models total %d calls but Totals says %d", f, fromModels, totals.Calls)
		}
		// The daily grouping buckets by the call's own local date, so its span
		// can differ from the activity window's; compare the cost it does cover
		// only where every call falls inside the window.
		if totals.LastTS > 0 && totals.FirstTS > 0 &&
			totals.FirstTS >= from.Unix() && totals.LastTS <= to.Unix() &&
			math.Abs(fromDailyCost-totals.Cost) > 0.01 {
			t.Errorf("%+v: daily cost %.4f but Totals says %.4f", f, fromDailyCost, totals.Cost)
		}
		if fromActivity != totals.Calls {
			t.Errorf("%+v: activity window %d calls but Totals says %d "+
				"(the window and the filter must intersect, not override)",
				f, fromActivity, totals.Calls)
		}
	}
}

// TestFilterClauseKeepsItsSeparators is a direct guard on the defect itself: a
// clause list must never be able to produce two tokens glued together.
func TestFilterClauseKeepsItsSeparators(t *testing.T) {
	cut := time.Now()
	f := Filter{Agents: []string{"pi"}, DateFrom: &cut}
	clause, args := f.whereExtra("", "ts", "ts >= ?")
	if clause == "" {
		t.Fatal("no clause produced")
	}
	if !strings.HasPrefix(clause, " WHERE ") {
		t.Errorf("clause does not start with the keyword and a space: %q", clause)
	}
	for _, bad := range []string{"?AND", "?OR", "?AND ", "?model"} {
		if strings.Contains(clause, bad) {
			t.Errorf("clause has %q glued to a token: %q", bad, clause)
		}
	}
	if len(args) != 2 {
		t.Errorf("clause has %d args for %d placeholders", len(args), strings.Count(clause, "?"))
	}
}
