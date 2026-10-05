package web

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The layout tests below execute dashboard.js in the stub DOM (see jsdom_test.go)
// rather than reading its source. That is the whole point of this file: a grep
// cannot tell a working comparator from a short-circuited one, and counting
// `<td>`s cannot tell a Tokens cell from a Cost cell that happen to have swapped
// contents.
//
// Everything is driven from one declaration — columnLayout — so the header row,
// the rendered cell, and the sort hook cannot be checked against three different
// notions of the same table.

// column is one column of one table, declared once.
type column struct {
	// header is the exact text of the <th> in the template.
	header string
	// sort is the value of its data-sort attribute, or "" for a column with no
	// sort hook (an Actions cell).
	sort string
	// sentinel is a substring the rendered cell must contain when the payload row
	// carries the sentinel values below. Empty for a column that renders no
	// payload field — an actions cell, a bar, a locale-formatted label.
	sentinel string
}

// tableSpec is one table: its element id, the tbody its renderer fills, the
// renderer that fills it, the _COLUMNS constant its empty state uses, the
// top-level payload key its rows come from, and its columns in order.
type tableSpec struct {
	table    string
	tbody    string
	renderer string
	columns  string
	payload  string
	cols     []column
}

var columnLayout = []tableSpec{
	{
		table: "sessions-table", tbody: "sessions-tbody", renderer: "renderSessions",
		columns: "SESSIONS_COLUMNS", payload: "projects[0].sessions_list",
		cols: []column{
			{"Project / Session", "project", "sesssentinel"},
			{"Date", "start", "DATESENTINEL"},
			{"Duration", "duration", "DURSENTINEL"},
			{"LLM Time", "llm_time", "LLMSENTINEL"},
			{"Tool Time", "tool_time", "TOOLSENTINEL"},
			{"Tokens/s", "avg_tps", "91.2"},
			{"Messages", "messages", "901"},
			{"Tokens", "tokens", "902"},
			{"Cost", "cost", "$9.09"},
			// An actions cell renders buttons and a link, not a payload field.
			{"Actions", "", ""},
		},
	},
	{
		table: "models-table", tbody: "models-tbody", renderer: "renderModels",
		columns: "MODELS_COLUMNS", payload: "models",
		cols: []column{
			{"Model", "name", "MODELNAME"},
			{"Messages", "messages", "501"},
			{"Total Tokens", "tokens", "502"},
			{"Input", "input_tokens", "503"},
			{"Output", "output_tokens", "504"},
			{"Cache Read", "cache_read_tokens", "505"},
			{"Cache Write", "cache_write_tokens", "506"},
			{"Reasoning", "reasoning_tokens", "507"},
			{"Tokens/s", "avg_tps", "50.5"},
			{"Cost", "cost", "$5.05"},
			{"Share", "pct", "55.5%"},
		},
	},
	{
		table: "tools-table", tbody: "tools-tbody", renderer: "renderTools",
		columns: "TOOLS_COLUMNS", payload: "tools",
		cols: []column{
			{"Tool", "name", "TOOLNAME"},
			{"Calls", "calls", "601"},
			{"Total Time", "time", "TIMESENTINEL"},
			{"Avg Time", "avg_seconds", "AVGSENTINEL"},
			{"Errors", "errors", "602"},
			{"Associated Cost", "cost", "$6.06"},
		},
	},
	{
		table: "projects-table", tbody: "projects-tbody", renderer: "renderProjects",
		columns: "PROJECTS_COLUMNS", payload: "projects",
		cols: []column{
			{"Project", "name", "projsentinel"},
			{"Sessions", "sessions", "701"},
			{"Messages", "messages", "702"},
			{"Tokens", "tokens", "703"},
			{"LLM Time", "llm_time", "LLMSENTINEL"},
			{"Tool Time", "tool_time", "TOOLSENTINEL"},
			{"Tokens/s", "avg_tps", "70.4"},
			{"Cost", "cost", "$7.07"},
			{"Last Activity", "last_activity", "ACTSENTINEL"},
		},
	},
	{
		table: "activity-table", tbody: "activity-tbody", renderer: "renderActivityTable",
		columns: "ACTIVITY_COLUMNS", payload: "/api/activity",
		cols: []column{
			// The window label is a locale-formatted date, so only its presence
			// is checkable; every other column carries a sentinel.
			{"Window", "", ""},
			{"Messages", "", "811"},
			{"Output", "", "802"},
			{"Input", "", "803"},
			{"Total", "", "804"},
			{"Throughput", "", "6.6"},
			{"Avg response", "", "0s"},
			{"LLM time", "", "2m01s"},
			{"Cost", "", "$8.0800"},
		},
	},
}

// sentinelPayload is the dashboardData the harness is fed. Every field a table
// renders holds a value unique to the column it belongs to, so a cell bound to
// the wrong field shows the wrong value rather than merely the wrong type.
//
// Zero-valued token detail fields are deliberate: the token cell's tooltip lists
// all of them, and a non-zero value there would make every token column's cell
// contain every other one's sentinel.
func sentinelPayload() map[string]any {
	return map[string]any{
		"dailyStats": []any{},
		"projects": []any{map[string]any{
			"name": "/w/projsentinel", "agent_cmd": "pi", "agents": []any{"pi"},
			"sessions": 701, "messages": 702, "tokens": 703,
			"input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0,
			"cache_write_tokens": 0, "reasoning_tokens": 0,
			"llm_time_display": "LLMSENTINEL", "tool_time_display": "TOOLSENTINEL",
			"avg_tps": 70.4, "cost": 7.07,
			"last_activity": 1, "last_activity_display": "ACTSENTINEL",
			"models": []any{}, "tools": []any{},
			"sessions_list": []any{map[string]any{
				"uid": "u1", "cwd": "/w/sesssentinel", "path": "/logs/u1.jsonl",
				"title": "T", "start": 1, "end": 2, "duration": 61,
				"start_display": "DATESENTINEL", "end_display": "DATESENTINEL",
				"duration_display": "DURSENTINEL",
				"llm_time_display": "LLMSENTINEL", "tool_time_display": "TOOLSENTINEL",
				"avg_tps":  91.2,
				"messages": 901, "tokens": 902, "cost": 9.09,
			}},
		}},
		"models": []any{map[string]any{
			"name": "MODELNAME", "messages": 501, "tokens": 502,
			"input_tokens": 503, "output_tokens": 504,
			"cache_read_tokens": 505, "cache_write_tokens": 506,
			"reasoning_tokens": 507, "avg_tps": 50.5, "cost": 5.05, "pct": 55.5,
		}},
		"tools": []any{map[string]any{
			"name": "TOOLNAME", "calls": 601,
			"time_display": "TIMESENTINEL", "avg_time_display": "AVGSENTINEL",
			"errors": 602, "cost": 6.06,
		}},
	}
}

// sentinelActivity is the /api/activity response the harness serves. The values
// are chosen so no cell's rendered text is a substring of another's: 121 seconds
// of LLM time over 811 messages is "0s" for the response-time column and
// "2m01s" for the LLM-time column, which a "0s" search would otherwise also hit
// inside "2m01s"... except it does not, because 1 precedes the s there.
func sentinelActivity() map[string]any {
	return map[string]any{
		"buckets": []any{map[string]any{
			"t": 1779976545, "messages": 811,
			"input_tokens": 803, "output_tokens": 802,
			"cache_read_tokens": 0, "cache_write_tokens": 0, "reasoning_tokens": 0,
			"total_tokens": 804, "llm_seconds": 121, "cost": 8.08, "unpriced": 0,
		}},
		"step": 86400, "from": 1779976545, "to": 1779976545 + 86400,
		"oldest": 1779976545, "newest": 1779976545, "autoStep": 86400,
	}
}

// renderAllBody re-renders every table from sentinelPayload and returns the HTML
// each tbody was left holding.
//
// One node process serves all five tables: the harness is the expensive part, and
// the activity table is already rendered by the script's own load() path, which is
// itself worth exercising.
const renderAllBody = `async function () {
    const read = id => {
        const el = document.getElementById(id);
        return el === null ? null : el.innerHTML;
    };
    renderProjects();
    renderSessions();
    renderModels();
    renderTools();
    return {
        projects: read('projects-tbody'),
        sessions: read('sessions-tbody'),
        models: read('models-tbody'),
        tools: read('tools-tbody'),
        activity: read('activity-tbody'),
    };
}`

// renderTables executes the renderers once and returns each tbody's HTML.
func renderTables(t *testing.T) map[string]string {
	t.Helper()
	var out map[string]string
	runDashboardJS(t, jsConfig{
		Ids:      templateIDs(t),
		Data:     sentinelPayload(),
		Activity: sentinelActivity(),
	}, renderAllBody, &out)
	for name, html := range out {
		if html == "" {
			t.Fatalf("%s rendered nothing at all: the renderer bailed out, so every "+
				"assertion about its cells would pass vacuously", name)
		}
	}
	return out
}

// header is one <th> read out of the template.
type header struct {
	text string
	sort string
	// hasIcon reports whether the heading carries the <span class="sort-icon">
	// updateSortIcons looks for. A heading with one but not the other is either a
	// dead click or an arrow that never moves.
	hasIcon bool
}

// templateHeaders reads the header row of one table, in order.
//
// The extraction is per-table and tag-bounded rather than a whole-file regex, so
// a heading added to one table cannot be mistaken for one in another.
func templateHeaders(t *testing.T, page, table string) []header {
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
	thead := rest[:end]
	if k := strings.Index(thead, "<thead>"); k >= 0 {
		thead = thead[k:]
		if k2 := strings.Index(thead, "</thead>"); k2 >= 0 {
			thead = thead[:k2]
		}
	}
	var out []header
	rest = thead
	for {
		// "<th " or "<th>": a bare "<th" would also match "<thead>", and the
		// element's own tag would then be read as the first heading.
		open := strings.Index(rest, "<th ")
		if alt := strings.Index(rest, "<th>"); open < 0 || (alt >= 0 && alt < open) {
			open = alt
		}
		if open < 0 {
			break
		}
		// Step over the tag to just past its own ">", so the attribute text and
		// the heading text are separated rather than overlapping.
		rest = rest[open+len("<th"):]
		gt := strings.Index(rest, ">")
		if gt < 0 {
			t.Fatalf("table %q has an unterminated <th tag", table)
		}
		attrs := rest[:gt]
		rest = rest[gt+1:]
		close := strings.Index(rest, "</th>")
		if close < 0 {
			t.Fatalf("table %q has an unclosed <th>", table)
		}
		inner := rest[:close]
		rest = rest[close+len("</th>"):]
		hasIcon := strings.Contains(inner, `class="sort-icon"`)
		// The heading text is the label a reader sees; the arrow is decoration
		// that updateSortIcons replaces, so it is stripped before comparing.
		label := inner
		if k := strings.Index(label, "<span"); k >= 0 {
			label = label[:k]
		}
		out = append(out, header{
			text:    collapseSpace(label),
			sort:    attrValue(attrs, "data-sort"),
			hasIcon: hasIcon,
		})
	}
	return out
}

func attrValue(attrs, name string) string {
	key := " " + name + "=\""
	i := strings.Index(attrs, key)
	if i < 0 {
		return ""
	}
	rest := attrs[i+len(key):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func collapseSpace(s string) string {
	s = strings.NewReplacer("\n", " ", "\t", " ").Replace(s)
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	return strings.TrimSpace(s)
}

// stripAttrValues removes attribute values from a cell's HTML, so a check against
// the cell's visible text is not satisfied by a number that only appears inside a
// title tooltip.
//
// This matters for the token cells specifically: their tooltip itemises the input,
// output, cache and reasoning figures as well as the total, so leaving attributes
// in place would let a Tokens cell satisfy a check meant for Input.
func stripAttrValues(html string) string {
	for {
		i := strings.Index(html, `="`)
		if i < 0 {
			return html
		}
		rest := html[i+2:]
		j := strings.Index(rest, `"`)
		if j < 0 {
			return html
		}
		html = html[:i] + "=" + rest[j+1:]
	}
}

// TestEveryColumnRendersTheFieldItsHeaderPromises is the layout parity check.
//
// It runs the real renderers against a payload whose every field carries a
// sentinel unique to its column, and then asserts both directions: cell i
// contains the sentinel of the field header i promises, and contains no other
// column's sentinel. The second half is what a count could never do — swapping
// the contents of the Tool Time and Cost cells leaves every count in this file
// satisfied, and fails here.
func TestEveryColumnRendersTheFieldItsHeaderPromises(t *testing.T) {
	page := readAsset(t, templateFS, "templates/index.html")
	rendered := renderTables(t)

	for _, spec := range columnLayout {
		t.Run(spec.table, func(t *testing.T) {
			headers := templateHeaders(t, page, spec.table)
			cells := renderedCells(t, rendered[spec.payloadKey()])
			if len(headers) != len(cells) {
				t.Fatalf("%s has %d headers but its renderer emits %d data cells\nheaders: %v\ncells: %v",
					spec.table, len(headers), len(cells), headerTexts(headers), cells)
			}
			for i, want := range spec.cols {
				if headers[i].text != want.header {
					t.Errorf("%s column %d header is %q, want %q",
						spec.table, i, headers[i].text, want.header)
				}
				if headers[i].sort != want.sort {
					t.Errorf("%s column %d (%s) has data-sort=%q, want %q",
						spec.table, i, want.header, headers[i].sort, want.sort)
				}
				if want.sentinel == "" {
					// Nothing to assert about the contents, but the cell must
					// still exist and say something.
					if strings.TrimSpace(cells[i]) == "" {
						t.Errorf("%s column %d (%s) rendered an empty cell",
							spec.table, i, want.header)
					}
					continue
				}
				text := stripAttrValues(cells[i])
				if !strings.Contains(text, want.sentinel) {
					t.Errorf("%s column %d is headed %q and so must render its field, "+
						"but the cell holds %q, which does not contain %q",
						spec.table, i, want.header, text, want.sentinel)
				}
				for j, other := range spec.cols {
					if j == i || other.sentinel == "" {
						continue
					}
					if strings.Contains(text, other.sentinel) {
						t.Errorf("%s column %d is headed %q but renders %q, which is "+
							"column %d's (%s) value: the cells have swapped",
							spec.table, i, want.header, other.sentinel, j, other.header)
					}
				}
			}
		})
	}
}

// payloadKey is the key renderTables returns this table's HTML under.
func (s tableSpec) payloadKey() string {
	switch s.table {
	case "sessions-table":
		return "sessions"
	case "models-table":
		return "models"
	case "tools-table":
		return "tools"
	case "projects-table":
		return "projects"
	case "activity-table":
		return "activity"
	}
	return s.table
}

func headerTexts(hs []header) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.text
	}
	return out
}

// TestEverySortableHeaderIsWiredUp counts against the layout, not a floor.
//
// The old assertion was `len(found) < 20` against 35 real sortable headers, so
// fourteen of them could be deleted — each one turning a working column sort into
// a dead click — and the suite stayed green. Both sides are now derived from
// columnLayout: every column declared sortable here must carry both a data-sort
// hook and a sort icon, and no column declared unsortable may carry either.
func TestEverySortableHeaderIsWiredUp(t *testing.T) {
	page := readAsset(t, templateFS, "templates/index.html")

	wantTotal, gotTotal := 0, 0
	for _, spec := range columnLayout {
		t.Run(spec.table, func(t *testing.T) {
			headers := templateHeaders(t, page, spec.table)
			if len(headers) != len(spec.cols) {
				t.Fatalf("%s has %d headers but the layout declares %d; the two must "+
					"be the same list, not two that happen to agree today",
					spec.table, len(headers), len(spec.cols))
			}
			for i, h := range headers {
				want := spec.cols[i]
				if want.sort != "" {
					wantTotal++
				}
				// Both halves of the wiring are checked: the data-sort hook the
				// click handler binds to, and the arrow updateSortIcons moves.
				// A column with only one of them is a dead click or a dead arrow.
				switch {
				case want.sort == "" && h.sort != "":
					gotTotal++
					t.Errorf("%s column %d (%s) has data-sort=%q but the layout declares "+
						"it unsortable; a hook with no arrow is a dead click",
						spec.table, i, h.text, h.sort)
				case want.sort == "" && h.hasIcon:
					gotTotal++
					t.Errorf("%s column %d (%s) has a sort icon but no sort hook; "+
						"the arrow can never move", spec.table, i, h.text)
				case want.sort != "" && h.sort == "":
					t.Errorf("%s column %d (%s) is declared sortable but has no data-sort "+
						"attribute, so clicking it does nothing", spec.table, i, h.text)
				case want.sort != "" && !h.hasIcon:
					t.Errorf("%s column %d (%s) has data-sort=%q but no sort icon, so the "+
						"sorted column is never indicated", spec.table, i, h.text, h.sort)
				case want.sort != "":
					gotTotal++
					if h.text == "" {
						t.Errorf("%s column %d has an empty heading", spec.table, i)
					}
				}
			}
		})
	}
	if gotTotal != wantTotal {
		t.Errorf("found %d sortable headers across the page, want exactly %d: a removed "+
			"data-sort hook turns a working column sort into a dead click, so the count "+
			"is pinned rather than given a tolerance band", gotTotal, wantTotal)
	}
}

// TestSortComparesNumbersAsNumbers runs the comparator.
//
// The regression it guards is a hand-rolled comparator that lowercases and falls
// through to `<`, which sorts "1000" before "900" — wrong for every token column
// on the page. The previous version of this test asserted that the file *contains
// the string* "Number.isFinite(Number(a))", which survives that exact mutation
// because the expression is still there, merely short-circuited.
func TestSortComparesNumbersAsNumbers(t *testing.T) {
	const body = `async function () {
        const strings = sortData([{v: '1000'}, {v: '900'}, {v: '10000'}],
            {field: 'v', asc: true}).map(r => r.v);
        const descending = sortData([{v: '1000'}, {v: '900'}, {v: '10000'}],
            {field: 'v', asc: false}).map(r => r.v);
        const numbers = sortData([{v: 1000}, {v: 900}, {v: 10000}],
            {field: 'v', asc: true}).map(r => r.v);
        const stable = sortData([{v: 5, i: 0}, {v: 5, i: 1}, {v: 1, i: 2}],
            {field: 'v', asc: true}).map(r => r.i);
        const nullish = sortData([{v: null}, {v: 3}, {v: ''}, {v: 1}],
            {field: 'v', asc: true}).map(r => String(r.v));
        return {strings, descending, numbers, stable, nullish};
    }`

	var got struct {
		Strings    []string `json:"strings"`
		Descending []string `json:"descending"`
		Numbers    []int    `json:"numbers"`
		Stable     []int    `json:"stable"`
		Nullish    []string `json:"nullish"`
	}
	runDashboardJS(t, jsConfig{Ids: templateIDs(t), Data: sentinelPayload()}, body, &got)

	assertOrder(t, "numeric strings ascending", got.Strings, []string{"900", "1000", "10000"})
	assertOrder(t, "numeric strings descending", got.Descending, []string{"10000", "1000", "900"})
	assertOrder(t, "numbers ascending", got.Numbers, []int{900, 1000, 10000})

	// Equal keys must keep their original order, or every render reshuffles the
	// rows that tie and a reader chasing a value loses it between clicks.
	if len(got.Stable) != 3 || got.Stable[0] != 2 || got.Stable[1] != 0 || got.Stable[2] != 1 {
		t.Errorf("ties reshuffled: got %v, want the value 1 first and the two 5s in "+
			"their original order [2 0 1]", got.Stable)
	}
	// A missing value must sort as empty, not as the literal "null" or
	// "undefined" — a column where half the rows have no value would otherwise
	// sort those rows after every real one. null and "" tie, so they keep the
	// order they arrived in.
	assertOrder(t, "missing values ascending", got.Nullish, []string{"null", "", "1", "3"})
}

func assertOrder[S ~[]E, E any](t *testing.T, what string, got S, want S) {
	t.Helper()
	if !slices.EqualFunc(got, want, func(a, b E) bool {
		return fmt.Sprint(a) == fmt.Sprint(b)
	}) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

// TestEveryEmptyTableSaysWhyItIsEmpty runs each renderer with no rows.
//
// The message is written by the script, so a server-rendered {{else}} would never
// appear on this page — and an empty tbody is indistinguishable from one that
// failed to render, which on a machine whose logs dashd cannot read is the
// difference between "you did nothing" and "I could not see your logs".
func TestEveryEmptyTableSaysWhyItIsEmpty(t *testing.T) {
	const body = `async function () {
        const read = id => {
            const el = document.getElementById(id);
            return el === null ? null : el.innerHTML;
        };
        const empty = {dailyStats: [], projects: [], models: [], tools: []};
        // The script's own bindings are the only way to empty them: the renderers
        // read these arrays directly rather than window.dashboardData.
        projects.length = 0;
        models.length = 0;
        tools.length = 0;
        renderProjects();
        renderSessions();
        renderModels();
        renderTools();
        return {
            projects: read('projects-tbody'),
            sessions: read('sessions-tbody'),
            models: read('models-tbody'),
            tools: read('tools-tbody'),
            activity: read('activity-tbody'),
            chart: read('daily-chart-content'),
        };
    }`

	var got map[string]string
	runDashboardJS(t, jsConfig{
		Ids: templateIDs(t),
		Data: map[string]any{
			"dailyStats": []any{}, "projects": []any{},
			"models": []any{}, "tools": []any{},
		},
		Activity: map[string]any{"buckets": []any{}, "oldest": 0},
	}, body, &got)

	for _, spec := range columnLayout {
		if spec.table == "activity-table" {
			continue // asserted separately: its renderer is driven by fetch
		}
		html := got[spec.payloadKey()]
		if html == "" {
			t.Errorf("%s rendered nothing at all when it had no rows", spec.table)
			continue
		}
		assertEmptyRow(t, spec.table, html, len(spec.cols))
	}
	// The activity table is filled from /api/activity, and an all-empty response
	// is the case a user with no calls in the window hits.
	assertEmptyRow(t, "activity-table", got["activity"], len(columnLayout[4].cols))
	if chart := got["chart"]; !strings.Contains(chart, "No spending recorded yet") {
		t.Errorf("the daily chart rendered %q when there was no data at all; a blank "+
			"space is not a message", chart)
	}
}

// assertEmptyRow checks one rendered empty state: a single row, spanning every
// column, carrying a message with some words in it.
func assertEmptyRow(t *testing.T, table, html string, columns int) {
	t.Helper()
	want := `colspan="` + itoa(columns) + `"`
	if !strings.Contains(html, want) {
		t.Errorf("%s empty state is %q, want a cell spanning all %d columns (%s)",
			table, html, columns, want)
	}
	text := stripAttrValues(html)
	text = strings.NewReplacer("<tr>", " ", "<td", " ", "</td>", " ", "</tr>", " ",
		`class="`, " ").Replace(text)
	if n := len(strings.Fields(text)); n < 3 {
		t.Errorf("%s empty state carries no message: %q", table, html)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestEveryElementTheScriptLooksUpExistsOnThePage is the dead-wiring check, done
// positively.
//
// The filter bar's dead IIFE exited at its first null check, so every header click
// was a no-op while looking wired — and the test that was supposed to catch it
// asserted the script did not mention three strings. This asserts the opposite
// direction: every element dashboard.js looks up by id is an element the template
// declares, so a lookup of something the page does not have cannot pass unnoticed.
//
// The harness already fails a render that targets a missing element; this makes
// the specific claim checkable on its own.
func TestEveryElementTheScriptLooksUpExistsOnThePage(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")
	ids := map[string]bool{}
	for _, id := range templateIDs(t) {
		ids[id] = true
	}

	const lookup = `getElementById('`
	rest := script
	found := 0
	for {
		i := strings.Index(rest, lookup)
		if i < 0 {
			break
		}
		rest = rest[i+len(lookup):]
		j := strings.Index(rest, "'")
		if j < 0 {
			t.Fatalf("unterminated getElementById call in dashboard.js")
		}
		found++
		id := rest[:j]
		if !ids[id] {
			t.Errorf("dashboard.js looks up #%s, which the template does not declare: "+
				"the lookup returns null and everything behind it silently does nothing", id)
		}
		rest = rest[j:]
	}
	if found == 0 {
		t.Fatal("dashboard.js looks up no elements at all; this assertion is vacuous")
	}
	t.Logf("dashboard.js looks up %d element ids, all declared by the template", found)
}

// TestBurnDeltaClassesAreDistinguishable is the burn-rate guard the reviewer
// could flatten without consequence.
//
// Each direction of change must get its own class, and each class must have a
// style: a card whose delta is painted the same colour whether spend rose or fell
// turns the headline into a number nobody can act on.
func TestBurnDeltaClassesAreDistinguishable(t *testing.T) {
	css := readAsset(t, assets, "assets/dashboard.css")

	for _, tc := range []struct {
		name      string
		cost      float64
		prior     float64
		wantDelta string
		wantClass string
	}{
		{"rising", 1.5, 1, "+50%", "burn-up"},
		{"falling", 0.5, 1, "-50%", "burn-down"},
		{"unchanged", 1, 1, "+0%", "burn-flat"},
		{"no prior spend", 3, 0, "no prior spend", "burn-flat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			card := burnCards([]burnWindow{{Label: "x", Cost: tc.cost, PriorCost: tc.prior}})[0]
			if card.Delta != tc.wantDelta {
				t.Errorf("delta = %q, want %q", card.Delta, tc.wantDelta)
			}
			if card.DeltaClass != tc.wantClass {
				t.Errorf("delta class = %q, want %q: the direction of the change is "+
					"painted by this class, so a wrong one is invisible rather than wrong",
					card.DeltaClass, tc.wantClass)
			}
			if !strings.Contains(css, ".burn-delta."+card.DeltaClass) {
				t.Errorf("no style for burn delta class %q, so the delta renders in the "+
					"page's default colour", card.DeltaClass)
			}
		})
	}

	// The three directions must not collapse onto one class: the classes are the
	// only thing distinguishing them once the percentage has been read.
	classes := map[string]bool{}
	for _, cost := range []float64{1.5, 0.5, 1.0} {
		classes[burnCards([]burnWindow{{Cost: cost, PriorCost: 1}})[0].DeltaClass] = true
	}
	if len(classes) != 3 {
		t.Errorf("rising, falling and unchanged spend share %d classes, want 3: %v",
			len(classes), classes)
	}
}

// filterPrefillScript extracts the inline prefill IIFE from the shipped template.
//
// It is the script that makes a filtered view survive a reload, and it is the
// thing that used to be asserted by grepping dashboard.js for three strings that
// were not in the template. Running it is both possible and worth doing: whether
// a select comes back marked is observable, whereas whether a string appears in a
// file is only a proxy for it.
func filterPrefillScript(t *testing.T) string {
	t.Helper()
	page := readAsset(t, templateFS, "templates/index.html")
	// The opening tag carries the CSP nonce, so matching a bare "<script>" no
	// longer finds anything. The nonce is not incidental: an inline script
	// without one is blocked by the policy, and security_headers_test.go
	// asserts that separately. So the tag is matched with its attributes and
	// required to have one.
	last := ""
	for _, m := range inlineScriptRe.FindAllStringSubmatch(page, -1) {
		if strings.Contains(m[1], "src=") {
			continue // the external dashboard.js, not an inline block
		}
		if !strings.Contains(m[1], "nonce=") {
			t.Fatalf("an inline script has no CSP nonce and will be blocked:\n%s", m[0])
		}
		last = m[2]
	}
	if last == "" {
		t.Fatal("the template has no inline script block")
	}
	if !strings.Contains(last, "filterState") {
		t.Fatalf("the last inline script is not the filter prefill:\n%s", last)
	}
	return last
}

// inlineScriptRe matches an opening script tag, its attributes, and its body.
var inlineScriptRe = regexp.MustCompile(`(?s)<script([^>]*)>(.*?)</script>`)

// TestFilterFormIsPrefilledFromTheQueryString replaces the old "dashboard.js does
// not mention filter-model/filter-agent/filter-apply" grep, which could not fail
// on the bug it was written for: the dead IIFE it documented exited at its first
// null check, so the form was never marked while looking wired.
//
// This runs the shipped prefill script against stubbed selects and asserts which
// options come back marked. Making the script exit early — the original defect —
// leaves every option unmarked and fails here.
func TestFilterFormIsPrefilledFromTheQueryString(t *testing.T) {
	script := filterPrefillScript(t)

	body := `async function () {
        const withOptions = values => {
            const el = elements.get(values.id);
            el.options = values.options.map(v => ({value: v, selected: false}));
            return el;
        };
        withOptions({id: 'model', options: ['gemini-2.5-pro', 'claude-opus-4-6']});
        withOptions({id: 'agent', options: ['pi', 'claude']});
        withOptions({id: 'project', options: ['/p/one', '/p/two']});
        elements.get('date_from').value = '';
        elements.get('date_to').value = '';
        const prefill = ` + jsStringLiteral(script) + `;
        window.filterState = {models: ['gemini-2.5-pro'], agents: ['claude'], projects: []};
        (0, eval)(prefill);
        return {
            models: elements.get('model').options.map(o => [o.value, o.selected]),
            agents: elements.get('agent').options.map(o => [o.value, o.selected]),
            projects: elements.get('project').options.map(o => [o.value, o.selected]),
            dateFrom: elements.get('date_from').value,
            dateTo: elements.get('date_to').value,
        };
    }`

	var got struct {
		Models   [][]any `json:"models"`
		Agents   [][]any `json:"agents"`
		Projects [][]any `json:"projects"`
		DateFrom string  `json:"dateFrom"`
		DateTo   string  `json:"dateTo"`
	}
	runDashboardJS(t, jsConfig{
		Ids:    templateIDs(t),
		Data:   map[string]any{"dailyStats": []any{}, "projects": []any{}, "models": []any{}, "tools": []any{}},
		Search: "?date_from=2026-01-02&date_to=2026-03-04",
	}, body, &got)

	marked := func(opts [][]any) map[string]bool {
		out := map[string]bool{}
		for _, o := range opts {
			out[o[0].(string)] = o[1].(bool)
		}
		return out
	}
	// A filtered view reloaded must show the same filter, or the reader sees an
	// unfiltered form over filtered data and cannot tell which view they are in.
	if m := marked(got.Models); !m["gemini-2.5-pro"] || m["claude-opus-4-6"] {
		t.Errorf("model options came back %v, want only gemini-2.5-pro marked", m)
	}
	if m := marked(got.Agents); !m["claude"] || m["pi"] {
		t.Errorf("agent options came back %v, want only claude marked", m)
	}
	// An empty axis marks nothing: marking every option of an unfiltered axis
	// would submit it as a filter and silently narrow the next request.
	if m := marked(got.Projects); len(m) != 2 || m["/p/one"] || m["/p/two"] {
		t.Errorf("project options came back %v, want neither marked: filterState carried "+
			"no projects, so prefill marks none", m)
	}
	if got.DateFrom != "2026-01-02" || got.DateTo != "2026-03-04" {
		t.Errorf("date inputs came back %q..%q, want the query's dates 2026-01-02..2026-03-04",
			got.DateFrom, got.DateTo)
	}
}

// jsStringLiteral renders a Go string as a JavaScript single-quoted literal.
func jsStringLiteral(s string) string {
	return "'" + strings.NewReplacer(
		`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", "", "<", `\u003c`,
	).Replace(s) + "'"
}

// TestSessionsSearchNarrowsTheTable runs the search box.
//
// The search is the reason the sessions list is not simply all rows: a few
// hundred sessions would be a multi-megabyte DOM. It is also the one piece of
// interactivity on the page with no server round trip, so a predicate that
// matches everything looks identical to a working one until someone types.
//
// Three rows are supplied and the query matches exactly one, so a predicate
// that returns true unconditionally fails here.
func TestSessionsSearchNarrowsTheTable(t *testing.T) {
	payload := sentinelPayload()
	row := payload["projects"].([]any)[0].(map[string]any)
	base := row["sessions_list"].([]any)[0].(map[string]any)

	sessions := []any{}
	for i, cwd := range []string{"/w/needle-match", "/w/other-one", "/w/other-two"} {
		s := map[string]any{}
		for k, v := range base {
			s[k] = v
		}
		s["uid"] = "u" + itoa(i)
		s["cwd"] = cwd
		s["title"] = "t" + itoa(i)
		sessions = append(sessions, s)
	}
	row["sessions_list"] = sessions

	const body = `async function () {
        const search = document.getElementById('sessions-search');
        const run = q => {
            search.value = q;
            renderSessions();
            return {
                html: document.getElementById('sessions-tbody').innerHTML,
                shown: document.getElementById('sessions-shown').textContent,
                count: document.getElementById('sessions-count').textContent,
            };
        };
        return {
            none: run(''),
            needle: run('NEEDLE'),
            absent: run('no-such-string'),
        };
    }`

	var got struct {
		None   renderState `json:"none"`
		Needle renderState `json:"needle"`
		Absent renderState `json:"absent"`
	}
	runDashboardJS(t, jsConfig{Ids: templateIDs(t), Data: payload}, body, &got)

	if n := strings.Count(got.None.HTML, "<tr>"); n != 3 {
		t.Errorf("an empty search rendered %d rows, want all 3", n)
	}
	if got.None.Count != "3 sessions" {
		t.Errorf("session count = %q, want %q", got.None.Count, "3 sessions")
	}
	// Case-insensitively matching one row, and saying so, is the whole point.
	if !strings.Contains(got.Needle.HTML, "needle-match") {
		t.Error("a search matching one session did not render it")
	}
	for _, absent := range []string{"other-one", "other-two"} {
		if strings.Contains(got.Needle.HTML, absent) {
			t.Errorf("a search for one session also rendered %s", absent)
		}
	}
	if !strings.Contains(got.Needle.Shown, "1 of 3") {
		t.Errorf("the shown counter reads %q, want it to report 1 of 3: without that "+
			"a search that silently matches nothing looks the same as one that matched "+
			"everything", got.Needle.Shown)
	}
	// A search matching nothing must say so rather than showing every row.
	if strings.Contains(got.Absent.HTML, "other-one") {
		t.Error("a search matching nothing rendered every session")
	}
	if !strings.Contains(got.Absent.HTML, "No sessions match") {
		t.Errorf("a search matching nothing rendered %q, want an explicit no-match row",
			got.Absent.HTML)
	}
}

// renderState is what one run of renderSessions left behind.
type renderState struct {
	HTML  string `json:"html"`
	Shown string `json:"shown"`
	Count string `json:"count"`
}
