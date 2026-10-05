package store

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
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

// TestWhereExtraPreservesBoundOrder pins the contract the leading bounds have:
// they come first, in the order given, ahead of anything the filter adds.
//
// Activity() depends on this to build its argument list — it prepends the two
// bucket divisors and then appends these, so a bound that moved behind a filter
// clause would pair a filter value with the window's timestamp.
func TestWhereExtraPreservesBoundOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		f       Filter
		bounds  []string
		wantSeq []string
	}{
		{"bounds only", Filter{}, []string{"ts >= ?", "ts <= ?"},
			[]string{"ts >= ?", "ts <= ?"}},
		{"bounds keep their order", Filter{}, []string{"a", "b", "c"}, []string{"a", "b", "c"}},
		{"no bounds", Filter{Models: []string{"m"}}, nil, []string{"model IN (?)"}},
		{"bounds lead the filter", Filter{Models: []string{"m"}}, []string{"w"},
			[]string{"w", "model IN (?)"}},
		{"bounds lead every axis", Filter{Agents: []string{"a"}, Projects: []string{"p"}},
			[]string{"w1", "w2"}, []string{"w1", "w2", "agent IN (?)", "project IN (?)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clause, _ := tc.f.whereExtra("", "ts", tc.bounds...)
			if clause == "" {
				t.Fatal("no clause produced")
			}
			got := strings.Split(strings.TrimPrefix(clause, " WHERE "), " AND ")
			if len(got) != len(tc.wantSeq) {
				t.Fatalf("clause has %d clauses %q, want %d", len(got), clause, len(tc.wantSeq))
			}
			for i := range got {
				if got[i] != tc.wantSeq[i] {
					t.Errorf("clause %d = %q, want %q (whole clause: %q)",
						i, got[i], tc.wantSeq[i], clause)
				}
			}
		})
	}
}

// TestWhereExtraEmptyFilterProducesNoClause: no bounds and no filter must not
// leave a bare WHERE behind, which is a syntax error rather than a no-op.
func TestWhereExtraEmptyFilterProducesNoClause(t *testing.T) {
	for _, tc := range []struct {
		name   string
		f      Filter
		bounds []string
	}{
		{"nothing", Filter{}, nil},
		{"empty bounds", Filter{}, []string{}},
		{"empty axes", Filter{Models: nil, Agents: nil, Projects: nil}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clause, args := tc.f.whereExtra("", "ts", tc.bounds...)
			if clause != "" || args != nil {
				t.Errorf("got %q / %v, want no clause and no args", clause, args)
			}
			// A ts column on its own is not a filter either.
			if clause, _ := tc.f.where("", "ts"); clause != "" {
				t.Errorf("where produced %q for an empty filter", clause)
			}
		})
	}
}

// TestHistoryRangeReportsTheSpanTheCallsActuallyCover is the regression test for
// a transposition nobody would make on purpose.
//
// HistoryRange returned MIN(NULLIF(ts,0)) and MAX(ts) side by side, and swapping
// the two `time.Unix` assignments that receive them is invisible to every other
// test: the web layer anchors the activity chart on `newest`, so a swap inverts
// every activity window the page shows. The existing coverage seeded a single
// call, where oldest == newest and the swap is a no-op.
//
// Two calls months apart are seeded here, and both the extent and the window
// bounds derived from it are asserted — the latter because that is what the swap
// actually corrupts, and it is the observable consequence.
func TestHistoryRangeReportsTheSpanTheCallsActuallyCover(t *testing.T) {
	st, err := Open(t.TempDir() + "/range.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx := context.Background()
	// Month precision on purpose: a month is far enough apart that transposing
	// the two bounds cannot produce a plausible-looking window, and it is a span
	// the documented activity ranges have to narrow rather than clamp.
	oldest := time.Now().Add(-200 * 24 * time.Hour).Truncate(time.Hour)
	newest := oldest.Add(90 * 24 * time.Hour)
	for _, spec := range []struct {
		uid string
		at  time.Time
	}{
		{"ancient", oldest},
		{"recent", newest},
	} {
		sess := model.SessionWrite{
			Session: model.Session{UID: spec.uid, Agent: "pi", Project: "/p"},
			Calls: []model.Call{{
				SessionUID: spec.uid, CallKey: "c", Agent: "pi", Project: "/p",
				Model: "m", Time: spec.at, TotalTokens: 10, CostUSD: 1, Priced: true,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(spec.uid); err != nil {
			t.Fatal(err)
		}
	}

	gotOldest, gotNewest, err := st.HistoryRange(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !gotOldest.Equal(oldest) {
		t.Errorf("oldest = %s, want %s: MIN over the stored calls", gotOldest.UTC(), oldest.UTC())
	}
	if !gotNewest.Equal(newest) {
		t.Errorf("newest = %s, want %s: MAX over the stored calls", gotNewest.UTC(), newest.UTC())
	}
	// A transposed pair is still "a range", so the ordering itself is the
	// cheapest possible guard and the one a reader of the API would expect.
	if !gotOldest.Before(gotNewest) {
		t.Errorf("oldest %s is not before newest %s", gotOldest.UTC(), gotNewest.UTC())
	}

	// The consequence: the activity window runs from the older bound and ends at
	// the newer one. With the bounds transposed, this window covers the wrong
	// three months entirely and returns the other call.
	buckets, err := st.Activity(ctx, Filter{}, gotOldest, gotNewest, 86400)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	for _, b := range buckets {
		calls += b.Calls
	}
	if calls != 2 {
		t.Errorf("a window from the reported oldest to the reported newest covered "+
			"%d calls, want both: the chart anchors on newest, so a transposed pair "+
			"inverts every window it draws", calls)
	}

	// A filter narrows the extent by the same clause every other query uses, so a
	// filtered view's chart cannot reach outside the calls that view selected.
	// Here the filter drops the newer call, which must pull `newest` back to it.
	byAgent, filteredNewest, err := st.HistoryRange(ctx, Filter{Agents: []string{"pi"}})
	if err != nil {
		t.Fatal(err)
	}
	if !byAgent.Equal(oldest) || !filteredNewest.Equal(newest) {
		t.Errorf("a filter matching both agents reported %s..%s, want the full extent "+
			"%s..%s", byAgent.UTC(), filteredNewest.UTC(), oldest.UTC(), newest.UTC())
	}
	none, _, err := st.HistoryRange(ctx, Filter{Agents: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if !none.IsZero() {
		t.Errorf("a filter matching nothing reported oldest %s, want a zero time", none.UTC())
	}

	// An empty database reports nothing rather than the epoch.
	st2, err := Open(t.TempDir() + "/empty.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	eo, en, err := st2.HistoryRange(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if !eo.IsZero() || !en.IsZero() {
		t.Errorf("an empty database reported %s..%s, want two zero times", eo, en)
	}
}
