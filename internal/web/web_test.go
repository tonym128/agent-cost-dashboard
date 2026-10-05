package web

import (
	"embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
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
func TestJSHasNoSubAgentGrouping(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")
	for _, dead := range []string{"subagent_sessions", "aggregateTokenCounts"} {
		if strings.Contains(script, dead) {
			t.Errorf("dashboard.js still contains %q, which no Go code emits", dead)
		}
	}
}

// The filter bar is a plain GET form in the template; the IIFE that drove
// filter-model/filter-agent/filter-apply exited at its first null check, so
// every header click was a no-op while looking wired.
func TestJSHasNoDeadFilterBar(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")
	for _, dead := range []string{"filter-model", "filter-agent", "filter-apply"} {
		if strings.Contains(script, dead) {
			t.Errorf("dashboard.js still targets %q, which is not in the template", dead)
		}
	}
}

// ---------------------------------------------------------------- layout parity

// tableLayout maps each table to the renderer that fills its tbody.
//
// The count is asserted two ways: the number of <th> in the template's thead,
// and the number of <td> the JS renderer emits for one row. They drifted apart
// once already — the models table showed tokens/sec under a "Cost" heading —
// and nothing caught it, so the check is mechanical from now on.
var tableLayout = []struct {
	table    string
	tbody    string
	renderer string
	// columns is the name of the constant dashboard.js uses for this table's
	// colspans, so the empty state and expanded rows cannot drift either.
	columns string
}{
	{"sessions-table", "sessions-tbody", "renderSessions", "SESSIONS_COLUMNS"},
	{"models-table", "models-tbody", "renderModels", "MODELS_COLUMNS"},
	{"tools-table", "tools-tbody", "renderTools", "TOOLS_COLUMNS"},
	{"projects-table", "projects-tbody", "renderProjects", "PROJECTS_COLUMNS"},
	{"activity-table", "activity-tbody", "renderActivityTable", "ACTIVITY_COLUMNS"},
}

func TestTableHeadersMatchRenderedCells(t *testing.T) {
	page := readAsset(t, templateFS, "templates/index.html")
	script := readAsset(t, assets, "assets/dashboard.js")

	for _, tbl := range tableLayout {
		t.Run(tbl.table, func(t *testing.T) {
			headers := countTag(t, page, tbl.table, "th")
			cells := countRendererCells(script, tbl.renderer)
			if headers != cells {
				t.Errorf("%s has %d headers but its renderer emits %d cells; "+
					"every column needs a heading and every heading needs a cell",
					tbl.table, headers, cells)
			}
			// The tbody must be the one the renderer targets, otherwise this
			// check is comparing two unrelated things that happen to match.
			if !strings.Contains(page, `id="`+tbl.tbody+`"`) {
				t.Errorf("template has no tbody %q for %s", tbl.tbody, tbl.table)
			}
			// The colspan an empty or expanded row uses has to match too, or
			// the empty state renders a short row.
			if got := declaredColumns(t, script, tbl.columns); got != headers {
				t.Errorf("%s: colspan constant %s is %d, headers are %d",
					tbl.table, tbl.columns, got, headers)
			}
		})
	}
}

// declaredColumns reads the `const <TABLE>_COLUMNS = n` the JS uses for the
// empty-state and expanded-row colspans, checking that the renderer actually
// declares one for this table.
func declaredColumns(t *testing.T, script, name string) int {
	t.Helper()
	i := strings.Index(script, name+" = ")
	if i < 0 {
		t.Errorf("dashboard.js declares no %s constant", name)
		return -1
	}
	digits := regexp.MustCompile(`\d+`).FindString(script[i+len(name)+3:])
	if digits == "" {
		t.Errorf("%s is not assigned a number", name)
		return -1
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		t.Errorf("%s is not a number: %v", name, err)
		return -1
	}
	return n
}

// countRendererCells counts the <td> elements in a renderer's data-row
// template.
//
// The function body is extracted by brace matching from `function NAME(`, so the
// count is of that one function's cells and not of the file's. Nested template
// literals and braces do not confuse it because only the outer braces of the
// function are tracked.
func countRendererCells(script, fn string) int {
	start := strings.Index(script, "function "+fn+"(")
	if start < 0 {
		return -1
	}
	body := script[start:]
	depth := 0
	seenBody := false
	end := len(body)
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '{':
			depth++
			seenBody = true
		case '}':
			depth--
			if seenBody && depth == 0 {
				end = i
				i = len(body)
			}
		}
	}
	// Cells belonging to an expandable detail row or an empty-state row are
	// not data cells; they use colspan and would double-count.
	text := body[:end]
	lines := strings.Split(text, "\n")
	cells := 0
	for _, line := range lines {
		if !strings.Contains(line, "<td") || strings.Contains(line, "colspan") {
			continue
		}
		cells += strings.Count(line, "<td")
	}
	return cells
}

// countTag counts <tag occurrences inside one table's thead in the template.
func countTag(t *testing.T, page, table, tag string) int {
	t.Helper()
	i := strings.Index(page, `<table id="`+table+`">`)
	if i < 0 {
		t.Fatalf("template has no table %q", table)
	}
	rest := page[i:]
	end := strings.Index(rest, "</table>")
	if end < 0 {
		t.Fatalf("table %q is not closed", table)
	}
	// `<th` would also match `<thead>`, so the tag must be followed by its own
	// delimiter.
	re := regexp.MustCompile(`<` + tag + `[ >]`)
	return len(re.FindAllString(rest[:end], -1))
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
