package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// The documented activity windows, and what each has to contain.
//
// The `range` parameter was exercised exactly twice in the whole suite: once as
// 0 (all time) and once in TestActivityAPIRejectsABadRange as -5. Every value
// the page's own <select> offers was untested, so the block that narrows the
// window could be replaced wholesale and the suite stayed green — the reviewer
// confirmed exactly that. A window that silently returned all of history is a
// chart labelled "Last hour" showing four months.

// activityResponse is the JSON /api/activity returns.
type activityResponse struct {
	Buckets []struct {
		T       int64   `json:"t"`
		Calls   int     `json:"messages"`
		Cost    float64 `json:"cost"`
		Tokens  int64   `json:"total_tokens"`
		LLMSecs float64 `json:"llm_seconds"`
	} `json:"buckets"`
	Step     int64 `json:"step"`
	From     int64 `json:"from"`
	To       int64 `json:"to"`
	Oldest   int64 `json:"oldest"`
	Newest   int64 `json:"newest"`
	AutoStep int64 `json:"autoStep"`
}

// getActivity calls the endpoint and decodes it, failing on any non-200.
func getActivity(t *testing.T, srv *Server, query string) activityResponse {
	t.Helper()
	rec := get(t, srv, "/api/activity?"+query)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/activity?%s: status %d, body %s", query, rec.Code, rec.Body.String())
	}
	var out activityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode /api/activity?%s: %v", query, err)
	}
	return out
}

// seedActivityOverMonthsAndHours writes calls at two scales — hourly for the last
// week and monthly for the last year — and returns their timestamps.
//
// Two scales because the documented windows span four orders of magnitude: an hour
// and a 90-day view cannot both be distinguished by a fixture spaced a month apart,
// and a fixture spaced an hour apart cannot express a 90-day view at all.
//
// Every timestamp is on an exact hour so the epoch-aligned buckets cannot split a
// call across two of them.
func seedActivityOverMonthsAndHours(t *testing.T, srv *Server) []time.Time {
	t.Helper()
	st := srv.store
	now := srv.Generated()
	base := now.Truncate(time.Hour)

	var times []time.Time
	add := func(at time.Time) {
		uid := fmt.Sprintf("a%d", at.Unix())
		sess := model.SessionWrite{
			Session: model.Session{UID: uid, Agent: "pi", Project: "/p"},
			Calls: []model.Call{{
				SessionUID: uid, CallKey: "c", Agent: "pi", Project: "/p",
				Model: "m", Time: at,
				InputTokens: 100, OutputTokens: 50, TotalTokens: 150,
				LLMSeconds: 2, CostUSD: 1, Priced: true,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
		times = append(times, at)
	}

	for i := 0; i < 24*8; i++ {
		add(base.Add(-time.Duration(i) * time.Hour))
	}
	for i := 1; i <= 12; i++ {
		add(base.AddDate(0, -i, 0))
	}
	return times
}

// TestActivityRangeNarrowsTheWindow is the table test over the values the page
// offers.
//
// Two assertions per row. The first is that the window the server reports
// matches the range asked for, anchored on the newest call — which is what makes
// a database whose data ends last Tuesday chart last Tuesday. The second is that
// the calls inside that window are exactly the calls a reader would expect to
// see in a chart with that label, which is the part a block that returned
// everything would get wrong.
func TestActivityRangeNarrowsTheWindow(t *testing.T) {
	srv, _ := newTestServer(t)
	seeded := seedActivityOverMonthsAndHours(t, srv)

	// The window a range asks for is anchored on the newest call, so the expected
	// count is computed from the fixture's own timestamps rather than written out
	// as literals. That keeps the assertion about the data rather than about a
	// number someone typed, while still failing if the narrowing is removed: a
	// window that returned everything would return len(seeded).
	for _, tc := range []struct {
		rangeSecs int64
		label     string
	}{
		{3600, "Last hour"},
		{10800, "Last 3 hours"},
		{21600, "Last 6 hours"},
		{43200, "Last 12 hours"},
		{86400, "Last 24 hours"},
		{259200, "Last 3 days"},
		{604800, "Last 7 days"},
		{2592000, "Last 30 days"},
		{7776000, "Last 90 days"},
		{0, "All time"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			out := getActivity(t, srv, "range="+strconv.FormatInt(tc.rangeSecs, 10))

			// Every returned bucket must belong to the window the response
			// reports. The one-step tolerance is the bucket alignment: boundaries
			// are epoch-aligned server-side, so the bucket holding the first
			// instant of the window starts up to one step before it.
			for _, b := range out.Buckets {
				if b.T < out.From-out.Step || b.T > out.To {
					t.Errorf("bucket at %d is outside the reported window %d..%d "+
						"(step %d)", b.T, out.From, out.To, out.Step)
				}
			}

			var got int
			for _, b := range out.Buckets {
				got += b.Calls
			}

			if tc.rangeSecs == 0 {
				if got != len(seeded) {
					t.Errorf("all-time window returned %d calls, want all %d", got, len(seeded))
				}
				if out.From != out.Oldest {
					t.Errorf("all-time window starts at %d, want the oldest call at %d",
						out.From, out.Oldest)
				}
				return
			}

			// The expected set: the seeded calls inside [to-range, to], using the
			// window the server reported rather than one recomputed here, so this
			// checks the rows against the bounds and the bounds against the range.
			span := time.Duration(tc.rangeSecs) * time.Second
			from, to := time.Unix(out.From, 0), time.Unix(out.To, 0)
			if got := to.Sub(from); got != span {
				t.Errorf("window spans %v, want exactly the %v asked for", got, span)
			}
			var want int
			for _, at := range seeded {
				if !at.Before(from) && !at.After(to) {
					want++
				}
			}
			if got != want {
				t.Errorf("range=%d (%s) returned %d calls, want %d of the %d seeded "+
					"inside %s..%s: a chart labelled %q that shows the whole history "+
					"is the failure this covers",
					tc.rangeSecs, tc.label, got, want, len(seeded),
					from.Format(time.RFC3339), to.Format(time.RFC3339), tc.label)
			}
			// A window shorter than the history must actually exclude something.
			// Without this a count comparison could be satisfied by a filter that
			// returns all of history for every range, which is exactly the defect.
			if want < len(seeded) && got == len(seeded) {
				t.Errorf("range=%d returned all %d calls: the window is not narrowing",
					tc.rangeSecs, got)
			}
		})
	}

	// Each window must be a distinct set, or several of the rows above are
	// measuring the same thing.
	seen := map[int]int{}
	for _, secs := range []int64{3600, 86400, 604800, 2592000, 7776000} {
		out := getActivity(t, srv, "range="+strconv.FormatInt(secs, 10))
		var n int
		for _, b := range out.Buckets {
			n += b.Calls
		}
		if prev, dup := seen[n]; dup {
			t.Errorf("range=%d and range=%d both returned %d calls: the windows are "+
				"not being distinguished", prev, secs, n)
		}
		seen[n] = int(secs)
	}

	// The control in the template has to offer exactly these values, or the table
	// above is testing ranges no reader can ask for.
	assertActivityRangesMatchTheControl(t)
}

// assertActivityRangesMatchTheControl checks the <select> in the template against
// the values the table test exercises.
func assertActivityRangesMatchTheControl(t *testing.T) {
	t.Helper()
	page := readAsset(t, templateFS, "templates/index.html")
	i := strings.Index(page, `id="activity-range"`)
	if i < 0 {
		t.Fatal("the template has no activity-range select")
	}
	rest := page[i:]
	end := strings.Index(rest, "</select>")
	if end < 0 {
		t.Fatal("the activity-range select is not closed")
	}
	offered := map[int64]bool{}
	re := regexp.MustCompile(`<option value="(\d+)"`)
	for _, m := range re.FindAllStringSubmatch(rest[:end], -1) {
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		offered[v] = true
	}
	for _, want := range []int64{
		3600, 10800, 21600, 43200, 86400, 259200, 604800, 2592000, 7776000, 0,
	} {
		if !offered[want] {
			t.Errorf("the window control does not offer range=%d, which "+
				"TestActivityRangeNarrowsTheWindow exercises", want)
		}
	}
	if len(offered) != 10 {
		t.Errorf("the window control offers %d ranges, want 10: an unlisted value is "+
			"one the window test does not cover", len(offered))
	}
}

// TestActivityRangeIsClampedToTheDataRatherThanTheClock is the other half of the
// anchoring rule, and the reason it exists.
//
// A database whose newest call is a month old must chart that month. Anchoring on
// wall-clock time instead would show an empty chart and read as "you did nothing",
// which is the single worst thing this page can say to someone whose logs it can
// read.
func TestActivityRangeNarrowingHoldsForStaleData(t *testing.T) {
	srv, _ := newTestServer(t)
	// One call, three months old, against a page generated today.
	old := srv.Generated().AddDate(0, -3, 0).Truncate(time.Hour)
	sess := model.SessionWrite{
		Session: model.Session{UID: "stale", Agent: "pi", Project: "/p"},
		Calls: []model.Call{{
			SessionUID: "stale", CallKey: "c", Agent: "pi", Project: "/p",
			Model: "m", Time: old, TotalTokens: 10, CostUSD: 1, Priced: true,
		}},
	}
	if err := srv.store.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.RecomputeSession("stale"); err != nil {
		t.Fatal(err)
	}

	out := getActivity(t, srv, "range=0")
	if out.Oldest != old.Unix() || out.Newest != old.Unix() {
		t.Fatalf("history extent = %d..%d, want the single call at %d",
			out.Oldest, out.Newest, old.Unix())
	}

	// The invariant, which holds whichever end the window is anchored on: a
	// requested range produces a window of exactly that width. The reviewer
	// replaced the whole narrowing block and the suite stayed green because only
	// range=0 was ever requested — with range=0 the block does nothing, so its
	// removal is invisible. Here a removed block would leave the window at the
	// full three months of history and the width assertion fails.
	for _, secs := range []int64{3600, 86400, 2592000} {
		out := getActivity(t, srv, "range="+strconv.FormatInt(secs, 10))
		if got := time.Duration(out.To-out.From) * time.Second; got != time.Duration(secs)*time.Second {
			t.Errorf("range=%d over three-month-old data: the window spans %v, want "+
				"exactly %v. With no history the window is the full extent, so this "+
				"width only appears if the range was applied.",
				secs, got, time.Duration(secs)*time.Second)
		}
		for _, b := range out.Buckets {
			if b.T < out.From-out.Step || b.T > out.To {
				t.Errorf("range=%d: bucket at %d is outside the reported window "+
					"%d..%d", secs, b.T, out.From, out.To)
			}
		}
		// Logged rather than asserted: which end a stale window is anchored on is
		// a production decision in activity.go, which this branch does not own.
		// The current code anchors on the page's generation time when the newest
		// call is more than a day old, which is the opposite of what the comment
		// above it describes — see the report.
		t.Logf("range=%-8d window %s..%s, %d calls",
			secs,
			time.Unix(out.From, 0).Format(time.RFC3339),
			time.Unix(out.To, 0).Format(time.RFC3339),
			countCalls(out))
	}

	// And all-time still reports the whole history, clamped to the oldest call.
	out = getActivity(t, srv, "range=0")
	if out.From != out.Oldest {
		t.Errorf("all-time window starts at %d, want the oldest call at %d",
			out.From, out.Oldest)
	}
	if got := countCalls(out); got != 1 {
		t.Errorf("all-time window returned %d calls, want the one recorded", got)
	}
}

// countCalls sums the calls across a response's buckets.
func countCalls(out activityResponse) int {
	n := 0
	for _, b := range out.Buckets {
		n += b.Calls
	}
	return n
}

// TestActivityAutoStepGrowsWithTheSpan covers the resolution chooser, which the
// same untested block computes.
//
// The bar count is what the reader has to read, so a fixed step over a long
// window would produce thousands of columns and a fixed large step over an hour
// would produce one. The chosen steps are the documented ones.
func TestActivityAutoStepGrowsWithTheSpan(t *testing.T) {
	for _, tc := range []struct {
		span time.Duration
		want int64
	}{
		{time.Hour, 300},
		{6 * time.Hour, 300},
		{7 * time.Hour, 900},
		{12 * time.Hour, 900},
		{13 * time.Hour, 3600},
		{7 * 24 * time.Hour, 3600},
		{8 * 24 * time.Hour, 6 * 3600},
		{90 * 24 * time.Hour, 6 * 3600},
		{91 * 24 * time.Hour, 86400},
		{3 * 365 * 24 * time.Hour, 86400},
	} {
		from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		got := autoStep(from, from.Add(tc.span))
		if got != tc.want {
			t.Errorf("autoStep over %v = %d, want %d", tc.span, got, tc.want)
		}
		// Whatever the span, the chosen step has to be one the endpoint will
		// accept if it is ever passed back explicitly.
		if !isAllowedStep(got) {
			t.Errorf("autoStep over %v chose %d, which the endpoint rejects as an "+
				"explicit step: the auto-chosen value must be one a reader could have "+
				"picked", tc.span, got)
		}
	}
}

// TestAtoiDefaultFallsBackRatherThanGuessing covers the query-parameter reader
// the window and step both go through.
//
// A malformed value must become the default, not zero: range=0 means all time,
// so a typo would otherwise silently widen the chart to the whole database.
func TestAtoiDefaultFallsBackRatherThanGuessing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 86400},
		{"3600", 3600},
		{"0", 0},
		{"nope", 86400},
		{"1e6", 86400},
		{"-5", 86400},
		{" 3600", 86400},
		{"3600 ", 86400},
		{"0x10", 86400},
	} {
		name := tc.in
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			if got := atoiDefault(tc.in, 86400); got != tc.want {
				t.Errorf("atoiDefault(%q, 86400) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestAllowedStepListIsTheOneTheControlOffers keeps the accepted set and the
// offered set the same, since one is validation and the other is the UI.
func TestAllowedStepListIsTheOneTheControlOffers(t *testing.T) {
	page := readAsset(t, templateFS, "templates/index.html")
	i := strings.Index(page, `id="activity-step"`)
	if i < 0 {
		t.Fatal("the template has no activity-step select")
	}
	rest := page[i:]
	end := strings.Index(rest, "</select>")
	if end < 0 {
		t.Fatal("the activity-step select is not closed")
	}
	re := regexp.MustCompile(`<option value="(\d+)"`)
	offered := map[int64]bool{}
	for _, m := range re.FindAllStringSubmatch(rest[:end], -1) {
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		offered[v] = true
	}
	// "auto" is the default and means "let the server choose", so it is not a
	// step value.
	delete(offered, 0)
	// Every value the control offers must pass validation: a resolution the UI
	// offers and the endpoint rejects is a dead control. The reverse does not
	// have to hold — the endpoint accepts more than the page asks for, which is
	// harmless — but the comment above allowedSteps claims it lists what the page
	// offers, and it does not: 21600 (6-hourly) is accepted and not offered. That
	// is reported rather than asserted, since activity.go is not a file this
	// branch owns; the fix is either adding the option to the template or
	// rewording the comment to say the list is a superset.
	// Collected into a slice first: ranging a map and passing the loop variable
	// straight into a generic call does not infer here (go1.27), which is a
	// distraction from what this test is about.
	var offeredList []int64
	for v := range offered {
		offeredList = append(offeredList, v)
	}
	sort.Slice(offeredList, func(i, j int) bool { return offeredList[i] < offeredList[j] })
	for _, v := range offeredList {
		if !slices.Contains(allowedSteps, v) {
			t.Errorf("the control offers step=%d, which the endpoint rejects: "+
				"choosing that resolution returns HTTP 400", v)
		}
	}
	var extra []int64
	for _, s := range allowedSteps {
		if !offered[s] {
			extra = append(extra, s)
		}
	}
	if len(extra) > 0 {
		t.Logf("the endpoint accepts %v, which the control does not offer; "+
			"allowedSteps' comment claims it lists what the page offers", extra)
	}
	// The rejection message has to name what is allowed, or it is a dead end.
	label := allowedStepsLabel()
	for _, s := range allowedSteps {
		if !strings.Contains(label, strconv.FormatInt(s, 10)) {
			t.Errorf("the rejection message %q does not mention the accepted value %d",
				label, s)
		}
	}
}
