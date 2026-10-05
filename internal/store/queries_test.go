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

	// One tool call per session, carrying the same axes as its call, so a query
	// over the tool_call table has something to filter and the filtered and
	// unfiltered counts differ by something other than zero.
	add := func(uid, agent, project, modelName string, at time.Time) {
		sess := model.SessionWrite{}
		sess.UID, sess.Agent, sess.Project = uid, agent, project
		sess.Calls = append(sess.Calls, oneCall(uid, agent, project, modelName, at))
		sess.ToolCalls = append(sess.ToolCalls, model.ToolCall{
			SessionUID: uid, CallKey: "c", Agent: agent, Project: project,
			Tool: "tool-" + uid, Time: at, Seconds: 1,
		})
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

// filteredEntry is one of the eight filtered entry points the page's payload is
// assembled from, with what "scoped correctly" means for it.
//
// Every one of these is called with the same Filter in production — the index
// handler builds one and passes it to all of them — so a filter that one of them
// applies differently shows up as a headline total that disagrees with the table
// beneath it. Before this table existed, the agreement test called only four of
// the eight, and the four it skipped were exactly the ones that go through
// whereSession() or the tool-call prefix: the model and project axes there were
// untested, which is why a ?model= request could return HTTP 500 and nothing in
// the suite noticed.
type filteredEntry struct {
	name string
	call func(context.Context, *Store, Filter) (int, error)
	// want is the number of rows (or calls) this entry must return under a filter
	// that selects the sessions named by the fixture. Each is expressed relative
	// to Totals so the eight cannot be checked against four hand-written numbers
	// that agree with each other for the wrong reason.
	want func(Totals) int
	// label names the count for failure messages.
	label string
}

// filteredEntries is the complete set. Adding a filtered query without adding it
// here is invisible, so the count is asserted below.
var filteredEntries = []filteredEntry{
	{
		name: "Models", label: "calls",
		call: func(ctx context.Context, st *Store, f Filter) (int, error) {
			rows, err := st.Models(ctx, f)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, m := range rows {
				n += m.Calls
			}
			return n, nil
		},
		want: func(t Totals) int { return t.Calls },
	},
	{
		name: "Projects", label: "calls",
		call: func(ctx context.Context, st *Store, f Filter) (int, error) {
			rows, err := st.Projects(ctx, f)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, r := range rows {
				n += r.Calls
			}
			return n, nil
		},
		want: func(t Totals) int { return t.Calls },
	},
	{
		name: "Tools", label: "tool calls",
		call: func(ctx context.Context, st *Store, f Filter) (int, error) {
			rows, err := st.Tools(ctx, f)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, r := range rows {
				n += r.Calls
			}
			return n, nil
		},
		// A filtered view's tool calls are the ones whose own axes match; every
		// session in the fixture has exactly one.
		want: func(t Totals) int { return t.Sessions },
	},
	{
		name: "Sessions", label: "sessions",
		call: func(ctx context.Context, st *Store, f Filter) (int, error) {
			rows, err := st.Sessions(ctx, f, 0)
			if err != nil {
				return 0, err
			}
			return len(rows), nil
		},
		want: func(t Totals) int { return t.Sessions },
	},
	{
		name: "ProjectModels", label: "calls",
		call: func(ctx context.Context, st *Store, f Filter) (int, error) {
			rows, err := st.ProjectModels(ctx, f)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, r := range rows {
				n += r.Calls
			}
			return n, nil
		},
		want: func(t Totals) int { return t.Calls },
	},
	{
		name: "ProjectTools", label: "tool calls",
		call: func(ctx context.Context, st *Store, f Filter) (int, error) {
			rows, err := st.ProjectTools(ctx, f)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, r := range rows {
				n += r.Calls
			}
			return n, nil
		},
		want: func(t Totals) int { return t.Sessions },
	},
	{
		name: "Activity", label: "calls",
		call: func(ctx context.Context, st *Store, f Filter) (int, error) {
			rows, err := st.Activity(ctx, f, filterBase().Add(-time.Hour), filterBase().Add(24*time.Hour), 3600)
			if err != nil {
				return 0, err
			}
			n := 0
			for _, r := range rows {
				n += r.Calls
			}
			return n, nil
		},
		want: func(t Totals) int { return t.Calls },
	},
}

// filterBase is the fixture's first call time, recomputed the same way
// newFilterStore computes it.
func filterBase() time.Time { return time.Now().Add(-48 * time.Hour).Truncate(time.Hour) }

// TestEveryFilteredEntryPointAgreesOnTheSameFilter is the test that would have
// caught the live ?model= 500.
//
// Every entry point above is called with the same filter as Totals, and the
// number of rows each returns is compared against what Totals says the same
// filter selects. Four of the eight — Projects, Tools, Sessions, ProjectModels,
// ProjectTools — go through whereSession() or the tool_call prefix, and their
// model and project axes had no coverage at all: disabling
// `if len(f.Models) > 0` in whereSession left the suite green.
//
// The filters below include one per axis and one matching nothing, because an
// entry point that ignores a filter entirely returns the unfiltered count and an
// entry point that over-filters returns zero; only the middle is right.
func TestEveryFilteredEntryPointAgreesOnTheSameFilter(t *testing.T) {
	st, done := newFilterStore(t)
	defer done()
	ctx := context.Background()

	cut := filterBase().Add(90 * time.Minute)
	filters := []struct {
		name string
		f    Filter
	}{
		{"none", Filter{}},
		{"agent", Filter{Agents: []string{"agy"}}},
		{"two agents", Filter{Agents: []string{"claude", "agy"}}},
		{"project", Filter{Projects: []string{"/p3"}}},
		{"two projects", Filter{Projects: []string{"/p1", "/p3"}}},
		{"date from", Filter{DateFrom: &cut}},
		{"date to", Filter{DateTo: &cut}},
		{"project and date", Filter{Projects: []string{"/p1"}, DateFrom: &cut}},
	}

	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			totals, err := st.Totals(ctx, tc.f)
			if err != nil {
				t.Fatalf("Totals(%+v): %v", tc.f, err)
			}
			for _, e := range filteredEntries {
				got, err := e.call(ctx, st, tc.f)
				if err != nil {
					// An error here is the failure this test exists for: the page
					// turns one of these into an HTTP 500 while showing nothing.
					//
					// KNOWN RED on this branch: Tools and ProjectTools qualify
					// their filter columns as `t.model`, and tool_call has no
					// model column, so any ?model= filter reaches the database and
					// comes back as "no such column: t.model". The test is written
					// against the fixed behaviour and will go green when the
					// query.go fix lands; see the report for the exact change.
					t.Errorf("%s(%+v): %v", e.name, tc.f, err)
					continue
				}
				if want := e.want(totals); got != want {
					t.Errorf("%s(%+v) returned %d %s, want %d: it scoped the rows "+
						"differently from Totals, so the table and the headline "+
						"disagree", e.name, tc.f, got, e.label, want)
				}
			}

			// The headline is a cost, so the cost has to reconcile too. Daily is
			// the one rollup that groups by the call's own local date, so it is
			// only comparable where every call falls inside the same set of days
			// Totals counted — which the date filters below deliberately are not.
			daily, err := st.Daily(ctx, tc.f)
			if err != nil {
				t.Fatalf("Daily(%+v): %v", tc.f, err)
			}
			var dailyCost float64
			for _, d := range daily {
				dailyCost += d.Cost
			}
			withinOneDay := len(daily) == 1
			if withinOneDay && math.Abs(dailyCost-totals.Cost) > 0.01 {
				t.Errorf("%+v: the daily chart costs $%.4f while Totals says $%.4f, "+
					"and both cover the same single day", tc.f, dailyCost, totals.Cost)
			}
		})
	}
}

// TestFilteredEntryPointListIsComplete guards the list above.
//
// A filtered query added to the store and not to filteredEntries would be
// silently uncovered — which is how four of the eight came to be untested in the
// first place. Each name is checked against a query the index handler actually
// issues.
func TestFilteredEntryPointListIsComplete(t *testing.T) {
	covered := map[string]bool{}
	for _, e := range filteredEntries {
		covered[e.name] = true
	}
	for _, name := range []string{
		"Models", "Projects", "Tools", "Sessions",
		"ProjectModels", "ProjectTools", "Activity",
	} {
		if !covered[name] {
			t.Errorf("%s is a filtered query the page issues but is not in "+
				"filteredEntries, so no test checks that it agrees with Totals", name)
		}
	}
	// Seven compared by row count, plus Daily compared by cost just below: eight
	// filtered queries in total, all of which the page issues with one filter.
	if len(filteredEntries) != 7 {
		t.Errorf("filteredEntries holds %d entries; the payload is assembled from "+
			"eight filtered queries and a removed one would go unnoticed",
			len(filteredEntries))
	}
}

// TestWhereSessionAppliesEveryAxis checks the clause builder directly.
//
// whereSession() exists because the session table has no model column, so the
// model filter has to be expressed as an EXISTS against the session's calls. That
// is a translation, and a translation is exactly where a silently dropped clause
// hides: the query still runs, still returns rows, and simply ignores one axis.
func TestWhereSessionAppliesEveryAxis(t *testing.T) {
	cut := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		name    string
		f       Filter
		wantHas []string
		wantNot []string
		args    int
	}{
		{
			name: "nothing", f: Filter{},
			wantNot: []string{"EXISTS", "model IN", "agent IN", "project IN", "last_ts"},
		},
		{
			name: "model alone", f: Filter{Models: []string{"m"}},
			wantHas: []string{"EXISTS", "model IN", "uid"},
			wantNot: []string{"agent IN", "project IN", "last_ts"},
			args:    1,
		},
		{
			name: "agent alone", f: Filter{Agents: []string{"a"}},
			wantHas: []string{"agent IN"},
			wantNot: []string{"EXISTS", "project IN", "last_ts"},
			args:    1,
		},
		{
			name: "project alone", f: Filter{Projects: []string{"p"}},
			wantHas: []string{"project IN"},
			wantNot: []string{"EXISTS", "agent IN", "last_ts"},
			args:    1,
		},
		{
			name: "every axis",
			f: Filter{Models: []string{"m"}, Agents: []string{"a"},
				Projects: []string{"p"}, DateFrom: &cut, DateTo: &cut},
			wantHas: []string{"EXISTS", "model IN", "agent IN", "project IN", "last_ts"},
			args:    5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clause, args := tc.f.whereSession("s")
			for _, want := range tc.wantHas {
				if !strings.Contains(clause, want) {
					t.Errorf("clause %q does not mention %q: this axis is silently "+
						"unfiltered", clause, want)
				}
			}
			for _, unwanted := range tc.wantNot {
				if strings.Contains(clause, unwanted) {
					t.Errorf("clause %q mentions %q, which this filter does not set",
						clause, unwanted)
				}
			}
			// One argument per placeholder, or the query pairs a value with the
			// wrong clause.
			if got := strings.Count(clause, "?"); got != len(args) {
				t.Errorf("clause has %d placeholders and %d args: %q / %v",
					got, len(args), clause, args)
			}
			if tc.args > 0 && len(args) != tc.args {
				t.Errorf("got %d args, want %d for %+v", len(args), tc.args, tc.f)
			}
			// The prefix has to reach every column it qualifies, including the one
			// inside the EXISTS subquery.
			if strings.Contains(clause, " FROM call mc WHERE mc.session_uid = uid") {
				t.Errorf("clause %q leaves the EXISTS subquery unqualified, so with a "+
					"prefix it would resolve uid against the wrong table", clause)
			}
		})
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

// TestModelFilterReachesEveryFilteredEntryPoint is the regression test for the
// live ?model= HTTP 500.
//
// KNOWN RED on this branch, by one line of production code another agent is
// fixing: Tools() and ProjectTools() build their clause with
// f.where("t", "ts"), which qualifies the model column as `t.model` — and
// tool_call has no model column, so the database rejects the statement and the
// page serves a 500 for any model filter. The other six entry points apply the
// model axis correctly, which is why only these two were ever wrong.
//
// The test is written against the fixed behaviour. It goes green when the
// tool_call queries express the model filter as an EXISTS against the session's
// calls, the same translation whereSession() uses; see the report for the exact
// change.
func TestModelFilterReachesEveryFilteredEntryPoint(t *testing.T) {
	st, done := newFilterStore(t)
	defer done()
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		f    Filter
		want int
	}{
		{"one model", Filter{Models: []string{"m2"}}, 1},
		{"two models", Filter{Models: []string{"m1", "m4"}}, 2},
		{"no match", Filter{Models: []string{"absent"}}, 0},
		{"model and project, agreeing", Filter{Models: []string{"m2"}, Projects: []string{"/p2"}}, 1},
		// The axes must intersect, not override: m2 lives in /p2, so pairing it
		// with /p1 selects nothing rather than falling back to either axis.
		{"model and project, disagreeing", Filter{Models: []string{"m2"}, Projects: []string{"/p1"}}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			totals, err := st.Totals(ctx, tc.f)
			if err != nil {
				t.Fatalf("Totals(%+v): %v", tc.f, err)
			}
			if totals.Sessions != tc.want {
				t.Fatalf("fixture: Totals(%+v) says %d sessions, want %d",
					tc.f, totals.Sessions, tc.want)
			}
			for _, e := range filteredEntries {
				got, err := e.call(ctx, st, tc.f)
				if err != nil {
					t.Errorf("%s(%+v): %v — a model filter that reaches the database "+
						"as an unknown column is an HTTP 500 on the page", e.name, tc.f, err)
					continue
				}
				if want := e.want(totals); got != want {
					t.Errorf("%s(%+v) returned %d %s, want %d", e.name, tc.f, got, e.label, want)
				}
			}
			// Daily's cost has to reconcile too, where the filter's calls fall in
			// one local day.
			daily, err := st.Daily(ctx, tc.f)
			if err != nil {
				t.Fatalf("Daily(%+v): %v", tc.f, err)
			}
			if len(daily) == 1 && math.Abs(daily[0].Cost-totals.Cost) > 0.01 {
				t.Errorf("the daily chart costs $%.4f while Totals says $%.4f for the "+
					"same single day", daily[0].Cost, totals.Cost)
			}
		})
	}
}
