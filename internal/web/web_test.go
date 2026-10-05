package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := New(st, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// A fixed clock keeps generated timestamps out of the assertions.
	fixed := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	srv.Generated = func() time.Time { return fixed }
	return srv, st
}

// seed inserts a small but representative dataset.
func seed(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	for i, spec := range []struct {
		agent, project, model string
		cost                  float64
	}{
		{"pi", "/p/one", "gemini-2.5-pro", 1.50},
		{"claude", "/p/one", "claude-opus-4-6", 3.00},
		{"pi", "/p/two", "mystery-model", 0},
	} {
		uid := "sess-" + string(rune('a'+i))
		sess := model.SessionWrite{
			Session: model.Session{UID: uid, Agent: spec.agent, Project: spec.project},
			Path:    "/logs/" + uid + ".jsonl",
			Calls: []model.Call{{
				SessionUID: uid, CallKey: "c1", Agent: spec.agent,
				Project: spec.project, Model: spec.model,
				Time:         now.Add(-time.Duration(i) * time.Hour),
				InputTokens:  1000,
				OutputTokens: 500,
				TotalTokens:  1500,
				LLMSeconds:   2,
				CostUSD:      spec.cost,
				// The third model has no rate, which must be reported rather
				// than presented as free.
				Priced: spec.cost > 0,
			}},
			// Tool calls are what the tools table is built from, and they
			// carry the attributed cost its "Associated Cost" column shows.
			ToolCalls: []model.ToolCall{{
				SessionUID: uid, CallKey: "c1", Agent: spec.agent,
				Project: spec.project, Tool: "read_file",
				Time:    now.Add(-time.Duration(i) * time.Hour),
				Seconds: 0.25, IsError: i == 2,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}
}

func get(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestIndexRendersWithData(t *testing.T) {
	srv, st := newTestServer(t)
	now := time.Now()
	seed(t, st, now)

	rec := get(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Agent Cost Dashboard", "Activity &amp; Throughput",
		`class="stats-grid"`, "Total Cost", "Unpriced Calls",
		"window.dashboardData", "/assets/dashboard.css", "/assets/dashboard.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestIndexWithNoDataStillRenders(t *testing.T) {
	// The scanner may not have run yet; an empty dashboard must be a page, not
	// an error page.
	srv, _ := newTestServer(t)
	rec := get(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d on an empty database", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "window.dashboardData") {
		t.Error("empty database produced no payload")
	}
}

func TestUnpricedCallsAreReportedNotHidden(t *testing.T) {
	// The seeded "mystery-model" call has no rate. It must still count, and the
	// page must say so, or a $0.00 reads as "free".
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	body := get(t, srv, "/").Body.String()
	if !strings.Contains(body, "Unpriced Calls") {
		t.Error("page does not surface unpriced calls")
	}
}

func TestFilterNarrowsThePayload(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())

	// The headline figures sit at the top level of the payload, which is where
	// the stat cards read them from.
	full := dashboardPayload(t, get(t, srv, "/").Body.String())
	if total, _ := full["totalCost"].(float64); total != 4.5 {
		t.Errorf("unfiltered cost = %v, want 4.5", total)
	}
	filtered := dashboardPayload(t, get(t, srv, "/?agent=claude").Body.String())
	if cost, _ := filtered["totalCost"].(float64); cost != 3.0 {
		t.Errorf("filtered cost = %v, want 3.0", cost)
	}
	models := filtered["models"].([]any)
	if len(models) != 1 {
		t.Errorf("filtered models = %d, want 1", len(models))
	}
}

func TestDateFilterCoversTheWholeEndDay(t *testing.T) {
	// A "to" date that stopped at midnight would silently drop every call made
	// during that day.
	srv, st := newTestServer(t)
	// Late in the *local* day. Dates are interpreted in the reader's timezone,
	// so the fixture has to be built from local time too — a wall-clock 23:30
	// UTC is already the next day in any timezone east of Greenwich.
	late := time.Date(2026, 6, 15, 22, 0, 0, 0, time.Local)
	seedAt(t, st, late)

	body := get(t, srv, "/?date_from=2026-06-15&date_to=2026-06-15").Body.String()
	payload := dashboardPayload(t, body)
	calls, _ := payload["totalMessages"].(float64)
	if calls != 1 {
		t.Errorf("calls on the end date = %v, want 1 (a midnight cutoff drops the day)", calls)
	}
}

func TestActivityAPIComputesAnyWindow(t *testing.T) {
	// The point of storing calls individually: a window over data whose logs are
	// long gone is still answerable.
	srv, st := newTestServer(t)
	old := time.Now().Add(-300 * 24 * time.Hour)
	sess := model.SessionWrite{
		Session: model.Session{UID: "ancient", Agent: "pi", Project: "/p"},
		Calls: []model.Call{{
			SessionUID: "ancient", CallKey: "c", Agent: "pi", Project: "/p",
			Model: "m", Time: old, InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			CostUSD: 1, Priced: true,
		}},
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("ancient"); err != nil {
		t.Fatal(err)
	}

	rec := get(t, srv, "/api/activity?range=0")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Buckets []map[string]any `json:"buckets"`
		Step    int64            `json:"step"`
		Oldest  int64            `json:"oldest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Buckets) != 1 {
		t.Fatalf("got %d buckets for an all-time window, want 1", len(out.Buckets))
	}
	if out.Buckets[0]["messages"].(float64) != 1 {
		t.Errorf("bucket messages = %v, want 1", out.Buckets[0]["messages"])
	}
	if out.Oldest == 0 {
		t.Error("response does not report the history extent")
	}
}

func TestActivityAPIRejectsABadRange(t *testing.T) {
	srv, _ := newTestServer(t)
	// A negative range is nonsense; it falls back to the default rather than
	// producing an empty or unbounded query.
	rec := get(t, srv, "/api/activity?range=-5")
	if rec.Code != http.StatusOK {
		t.Errorf("status %d for a negative range", rec.Code)
	}
}

func TestBadDateFilterIsAnErrorPage(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := get(t, srv, "/?date_from=not-a-date")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d for a malformed date, want 400", rec.Code)
	}
}

func TestSessionPageShowsStoredCalls(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())

	rec := get(t, srv, "/session?uid=sess-a")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Calls") || !strings.Contains(body, "gemini-2.5-pro") {
		t.Error("session page does not list the stored calls")
	}
	// The page must say the figures outlive the log.
	if !strings.Contains(body, "rotated away") {
		t.Error("session page does not explain that its figures outlive the log")
	}
}

func TestUnknownSessionIsANotFoundPage(t *testing.T) {
	srv, _ := newTestServer(t)
	if rec := get(t, srv, "/session?uid=nope"); rec.Code != http.StatusNotFound {
		t.Errorf("status %d for an unknown session, want 404", rec.Code)
	}
}

func TestHealthReportsScanState(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	srv.ScanInfo = func() (time.Time, bool) {
		return time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC), false
	}
	rec := get(t, srv, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Error("health does not report ok")
	}
	if out["calls"].(float64) != 3 {
		t.Errorf("health reports %v calls, want 3", out["calls"])
	}
	if out["last_scan"] != "2026-10-05T11:00:00Z" {
		t.Errorf("last_scan = %v", out["last_scan"])
	}
}

func TestPageEscapesUntrustedValues(t *testing.T) {
	// Project paths and model names come from session logs. They are rendered
	// into HTML and into a <script> block, so neither may be able to break out.
	srv, st := newTestServer(t)
	payload := `</script><img src=x onerror=alert(1)>`
	uid := "sess-x"
	sess := model.SessionWrite{
		Session: model.Session{UID: uid, Agent: "pi", Project: payload, Title: payload},
		Calls: []model.Call{{
			SessionUID: uid, CallKey: "c", Agent: "pi", Project: payload,
			Model: payload, Time: time.Now(), TotalTokens: 1, Priced: true,
		}},
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession(uid); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/").Body.String()
	if strings.Contains(body, "<img src=x onerror") {
		t.Error("a project path reached the page as live markup")
	}
	if strings.Contains(body, `</script><img`) {
		t.Error("a project path broke out of the script block")
	}

	// The filter state goes into a script block as JSON; a quote in a value must
	// not terminate the string.
	body = get(t, srv, "/?project="+url.QueryEscape(payload)).Body.String()
	if strings.Contains(body, `</script><img`) {
		t.Error("a project path in the query string broke out of the script block")
	}
}

func TestUnknownPathIsNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	if rec := get(t, srv, "/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", rec.Code)
	}
	if rec := get(t, srv, "/assets/nope.css"); rec.Code != http.StatusNotFound {
		t.Errorf("status %d for a missing asset, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------- helpers

// readAsset reads one of the embedded files the page is served from, so the
// test measures what is actually shipped rather than a copy on disk.
func readAsset(t *testing.T, fs embed.FS, name string) string {
	t.Helper()
	data, err := fs.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// dashboardPayload extracts and decodes the JSON the page hands to its script.
func dashboardPayload(t *testing.T, body string) map[string]any {
	t.Helper()
	const marker = "window.dashboardData = "
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatal("page has no dashboardData payload")
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, ";</script>")
	if j < 0 {
		t.Fatal("payload is not terminated")
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(rest[:j]), &out); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	return out
}

func seedAt(t *testing.T, st *store.Store, ts time.Time) {
	t.Helper()
	uid := "on-a-day"
	sess := model.SessionWrite{
		Session: model.Session{UID: uid, Agent: "pi", Project: "/p"},
		Calls: []model.Call{{
			SessionUID: uid, CallKey: "c", Agent: "pi", Project: "/p",
			Model: "m", Time: ts, TotalTokens: 10, CostUSD: 1, Priced: true,
		}},
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession(uid); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- step validation

func TestActivityAPIAcceptsAnOfferedStep(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	// 3600 is one of the resolutions the page offers.
	rec := get(t, srv, "/api/activity?range=86400&step=3600")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d for an offered step, want 200", rec.Code)
	}
	var out struct {
		Step int64 `json:"step"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Step != 3600 {
		t.Errorf("step = %d, want 3600", out.Step)
	}
}

func TestActivityAPIRejectsAnUnboundedStep(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	// One second over years of history is tens of millions of rows from a
	// local process; the page never asks for it, so it is refused rather than
	// served.
	for _, step := range []string{"1", "7", "86401", "999999999"} {
		rec := get(t, srv, "/api/activity?range=0&step="+step)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("status %d for step=%s, want 400", rec.Code, step)
		}
		if !strings.Contains(rec.Body.String(), "step must be one of") {
			t.Errorf("rejection for step=%s does not say what is allowed: %q",
				step, rec.Body.String())
		}
	}
}

// The sub-agent grouping UI was driven by a `subagent_sessions` key that no Go
// code emits: it survived a Python-to-Go rewrite whose data model dropped it.
// There is no sub-agent relationship in the session table to recover it from, so
// the code is gone rather than left waiting for data that will not arrive.
//
// This one stays a source-text check, and deliberately so: the property is an
// absence, and an absence cannot be observed by running the script. The filter-bar
// check that used to sit next to it *could* be observed that way, and now is —
// see TestFilterFormIsPrefilledFromTheQueryString.
func TestJSHasNoSubAgentGrouping(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")
	for _, dead := range []string{"subagent_sessions", "aggregateTokenCounts"} {
		if strings.Contains(script, dead) {
			t.Errorf("dashboard.js still contains %q, which no Go code emits", dead)
		}
	}
}

// ---------------------------------------------------------------- table data

func TestToolsTableShowsTheCostTheStoreComputed(t *testing.T) {
	// ToolStat.Cost has always been computed and shipped; the column header
	// promises it, so it has to be what is rendered under that heading.
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	payload := dashboardPayload(t, get(t, srv, "/").Body.String())
	tools, _ := payload["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("fixture produced no tool rows")
	}
	tool, _ := tools[0].(map[string]any)
	if _, ok := tool["cost"]; !ok {
		t.Error("tool rows do not carry cost")
	}
	if _, ok := tool["avg_seconds"]; !ok {
		t.Error("tool rows do not carry a numeric average, so the column cannot sort")
	}
}

func TestProjectsAreListedOncePerProjectNotOncePerAgent(t *testing.T) {
	// The same directory used by two agents arrives from store.Projects() as two
	// rows. The page shows one row per project, so the headline "Projects" card
	// and the table must agree.
	srv, st := newTestServer(t)
	now := time.Now()
	seed(t, st, now)
	for i, agent := range []string{"claude", "codex"} {
		uid := "shared-" + agent
		sess := model.SessionWrite{
			Session: model.Session{UID: uid, Agent: agent, Project: "/p/shared"},
			Path:    "/logs/" + uid + ".jsonl",
			Calls: []model.Call{{
				SessionUID: uid, CallKey: "c", Agent: agent, Project: "/p/shared",
				Model: "m", Time: now, TotalTokens: 10, CostUSD: float64(i + 1),
				Priced: true,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}

	payload := dashboardPayload(t, get(t, srv, "/").Body.String())
	projects, _ := payload["projects"].([]any)
	seen := map[string]int{}
	var merged *map[string]any
	for _, p := range projects {
		row, _ := p.(map[string]any)
		name, _ := row["name"].(string)
		if name == "/p/shared" && merged == nil {
			merged = &row
		}
		seen[name]++
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("project %s is listed %d times", name, n)
		}
	}
	if merged == nil {
		t.Fatal("the shared project is missing from the payload")
	}
	// Both agents' costs belong in the one row.
	if cost, _ := (*merged)["cost"].(float64); cost != 3 {
		t.Errorf("merged cost = %v, want 3 (1 from claude + 2 from codex)", cost)
	}
	if agents, _ := (*merged)["agents"].([]any); len(agents) != 2 {
		t.Errorf("merged row lists %d agents, want 2", len(agents))
	}
}

// ---------------------------------------------------------------- sorting

func TestSortFieldsExistInThePayload(t *testing.T) {
	// A data-sort naming a field the payload does not carry sorts an undefined
	// column: every row ties and the click appears to do nothing.
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	payload := dashboardPayload(t, get(t, srv, "/").Body.String())

	rows := map[string][]any{}
	for _, key := range []string{"projects", "models", "tools"} {
		list, _ := payload[key].([]any)
		if len(list) > 0 {
			rows[key] = list
		}
	}
	if len(rows) != 3 {
		t.Fatalf("fixture produced rows for %d of 3 tables", len(rows))
	}
	// Sorted session fields are checked against the sessions the page nests
	// under each project.
	var session map[string]any
	for _, p := range rows["projects"] {
		row, _ := p.(map[string]any)
		if list, _ := row["sessions_list"].([]any); len(list) > 0 {
			session, _ = list[0].(map[string]any)
			break
		}
	}
	if session == nil {
		t.Fatal("fixture produced no session rows")
	}

	cases := map[string][]string{
		"projects": {"name", "sessions", "messages", "tokens", "llm_time",
			"tool_time", "avg_tps", "cost", "last_activity"},
		"models": {"name", "messages", "tokens", "input_tokens", "output_tokens",
			"cache_read_tokens", "cache_write_tokens", "reasoning_tokens",
			"avg_tps", "cost", "pct"},
		"tools": {"name", "calls", "time", "avg_seconds", "errors", "cost"},
	}
	for table, fields := range cases {
		for _, f := range fields {
			if _, ok := rows[table][0].(map[string]any)[f]; !ok {
				t.Errorf("%s sorts on %q, which its rows do not carry", table, f)
			}
		}
	}
	for _, f := range []string{"cwd", "start", "duration", "llm_time", "tool_time",
		"avg_tps", "messages", "tokens", "cost"} {
		if _, ok := session[f]; !ok {
			t.Errorf("sessions-table sorts on %q, which its rows do not carry", f)
		}
	}
}

// ---------------------------------------------------------------- empty state

func TestEmptyDatabaseExplainsItselfRatherThanRenderingNothing(t *testing.T) {
	// A user whose agent logs dashd cannot read must not see the same page as a
	// user with no usage. Every table needs a message, and so does the page as a
	// whole.
	srv, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	for _, want := range []string{
		"Sources", // the source-detection panel
		"0 of 6 sources found",
		"No scan has run yet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty page is missing %q", want)
		}
	}
	// Each known agent gets a row whether or not anything was found, so a miss
	// is visible rather than inferred from an absent row.
	for _, agent := range []string{"pi", "claude", "codex", "gemini", "agy", "opencode"} {
		if !strings.Contains(body, "<td>"+agent+"</td>") {
			t.Errorf("source panel does not list %s", agent)
		}
	}

	// The daily chart's message is written by the script, so it is asserted
	// there rather than in the served markup.
	script := readAsset(t, assets, "assets/dashboard.js")
	if !strings.Contains(script, "No spending recorded yet") {
		t.Error("the daily chart renders nothing at all when there is no data")
	}
}

func TestSourcePanelIsHiddenOnceEverySourceIsFound(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	// A scan that found logs for every source it walked leaves nothing to act
	// on, so the panel would be noise. The agent not recorded here is
	// deliberately left out to prove the panel keys off scan_status, not off the
	// presence of data.
	for _, agent := range []string{"pi", "claude", "codex", "gemini", "agy", "opencode"} {
		if err := st.RecordScanStatus(agent, model.ScanStatus{
			Agent: agent, LastScanAt: time.Now(), FilesSeen: 4, CallsIngested: 9,
		}); err != nil {
			t.Fatal(err)
		}
	}
	body := get(t, srv, "/").Body.String()
	if strings.Contains(body, `class="section source-panel"`) {
		t.Error("source panel is shown even though every source has logs")
	}
	// The scanner table is still there; only the panel is suppressed.
	if !strings.Contains(body, `id="scan-table"`) {
		t.Error("the scanner table was removed along with the panel")
	}
}

func TestSourcePanelReportsAFailingSource(t *testing.T) {
	srv, st := newTestServer(t)
	if err := st.RecordScanStatus("claude", model.ScanStatus{
		Agent: "claude", LastScanAt: time.Now(), FilesSeen: 3, Error: "permission denied",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/").Body.String()
	if !strings.Contains(body, "source-error") || !strings.Contains(body, "permission denied") {
		t.Error("a failing source is not surfaced in the panel")
	}
	// A source that errored is not counted as found even though the walk saw
	// files: the pass did not complete.
	want := "0 of 6 sources found · 1 failing"
	if !strings.Contains(body, want) {
		t.Errorf("source summary = %q, want it to contain %q", sourceSummaryOf(body), want)
	}
}

// sourceSummaryOf pulls the summary badge text out of the rendered page.
func sourceSummaryOf(body string) string {
	i := strings.Index(body, `class="badge">`)
	if i < 0 {
		return ""
	}
	rest := body[i+len(`class="badge">`):]
	j := strings.Index(rest, "</span>")
	return rest[:j]
}

// ---------------------------------------------------------------- burn rate

func TestBurnWindowsCompareAgainstThePriorPeriod(t *testing.T) {
	// Local time deliberately: burnWindows anchors on the local calendar day and
	// store.Daily buckets by the call's own local date, so a UTC fixture would
	// measure a day boundary the production path never takes. Verified across
	// UTC, UTC+14 and a half-hour-DST zone.
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	daily := []store.DayBucket{
		{Day: "2026-10-05", Cost: 10}, // today
		{Day: "2026-10-04", Cost: 5},  // yesterday
		{Day: "2026-10-03", Cost: 2},
		{Day: "2026-10-02", Cost: 2},
		{Day: "2026-10-01", Cost: 1},
		{Day: "2026-09-30", Cost: 4},
		{Day: "2026-09-29", Cost: 6},
		{Day: "2026-09-28", Cost: 40}, // inside the prior 7 days
		{Day: "2026-09-03", Cost: 50}, // inside the prior month-to-date
	}
	ws := burnWindows(daily, now)
	if len(ws) != 4 {
		t.Fatalf("got %d windows, want 4", len(ws))
	}
	byLabel := map[string]burnWindow{}
	for _, w := range ws {
		byLabel[w.Label] = w
	}

	today := byLabel["Today"]
	if today.Cost != 10 || today.PriorCost != 5 {
		t.Errorf("Today = %.2f vs %.2f, want 10 vs 5", today.Cost, today.PriorCost)
	}
	// The 7-day window is 29 Sep through 5 Oct = 6+4+1+2+2+5+10 = 30; the seven
	// days before that hold only the 40.
	week := byLabel["Last 7 days"]
	if week.Cost != 30 {
		t.Errorf("7-day cost = %.2f, want 30", week.Cost)
	}
	if week.PriorCost != 40 {
		t.Errorf("prior 7-day cost = %.2f, want 40", week.PriorCost)
	}
	pct, ok := week.delta()
	if !ok || pct != -25 {
		t.Errorf("7-day delta = %.1f%% (ok=%v), want -25%%", pct, ok)
	}
	// October so far is the 5th, so the prior window is 1-5 September, which
	// holds the 50 but not the 40 from the 28th.
	mtd := byLabel["Month to date"]
	if mtd.Cost != 20 {
		t.Errorf("MTD cost = %.2f, want 20", mtd.Cost)
	}
	if mtd.PriorCost != 50 {
		t.Errorf("MTD prior cost = %.2f, want 50", mtd.PriorCost)
	}
}

func TestBurnWindowWithNoPriorSpendSaysSoRatherThanAPercentage(t *testing.T) {
	// "No prior spend" beats "+100%": there is no percentage to compute.
	// Local time for the same reason as the test above.
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.Local)
	ws := burnWindows([]store.DayBucket{{Day: "2026-10-05", Cost: 3}}, now)
	for _, c := range burnCards(ws) {
		if c.Label == "Today" {
			if c.Delta != "no prior spend" {
				t.Errorf("Today delta = %q, want %q", c.Delta, "no prior spend")
			}
			return
		}
	}
	t.Fatal("no Today card")
}

func TestBurnRowIsOnThePage(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())
	body := get(t, srv, "/").Body.String()
	for _, want := range []string{`id="burn-rate"`, "Today", "Last 7 days",
		"Last 30 days", "Month to date"} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing burn-rate figure %q", want)
		}
	}
}

func TestSourceStateIsUsableAsACSSClass(t *testing.T) {
	// The state slug goes straight into a class attribute, and every slug has a
	// style. "not scanned" produced class="source-not scanned", which is two
	// classes, one of which matched nothing.
	for _, v := range sourceViews(nil) {
		if strings.ContainsAny(v.State, " \t") {
			t.Errorf("source state %q is not a single class token", v.State)
		}
		if v.Status == "" {
			t.Errorf("source %s has no readable status", v.Agent)
		}
	}
	css := readAsset(t, assets, "assets/dashboard.css")
	for _, state := range []string{"found", "empty", "error", "not-scanned"} {
		if !strings.Contains(css, ".source-state.source-"+state) {
			t.Errorf("no style for source state %q", state)
		}
	}
}
