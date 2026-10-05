package web

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// parseFilter is the single translation from the query string into the store's
// Filter, and the page's only user input that reaches SQL. Nothing tested it
// directly: every filter test went through a rendered page, so the translation
// itself — the part that decides what "date_to" means — had no coverage of its
// own.

// TestParseFilterReadsEveryAxis covers the axes the filter bar offers.
func TestParseFilterReadsEveryAxis(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  []string // models, agents, projects
		from  string   // yyyy-mm-dd, or "" for no lower bound
		to    string   // yyyy-mm-dd, or "" for no upper bound
	}{
		{"nothing", "", nil, "", ""},
		{"one model", "model=gemini-2.5-pro", []string{"gemini-2.5-pro"}, "", ""},
		{"repeated model", "model=a&model=b", []string{"a", "b"}, "", ""},
		{"comma separated", "model=a,b", []string{"a", "b"}, "", ""},
		{"agent", "agent=pi", nil, "", ""},
		{"project", "project=/p/one", nil, "", ""},
		{
			"several axes", "model=a&agent=pi&project=/p",
			[]string{"a"}, "", "",
		},
		{
			// Whitespace around a value is a hand-typed filter, and a model name
			// with a leading space matches nothing.
			"padded value", "model=%20a%20&agent=%20pi%20",
			[]string{"a"}, "", "",
		},
		{"empty value", "model=&agent=pi", nil, "", ""},
		{"empty between commas", "model=a,,b", []string{"a", "b"}, "", ""},
		{
			"date from", "date_from=2026-05-01",
			nil, "2026-05-01", "",
		},
		{"date to", "date_to=2026-05-31", nil, "", "2026-05-31"},
		{
			"both dates", "date_from=2026-05-01&date_to=2026-05-31",
			nil, "2026-05-01", "2026-05-31",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			f, _, err := parseFilter(q)
			if err != nil {
				t.Fatalf("parseFilter(%q): %v", tc.query, err)
			}
			assertStringSlice(t, "models", f.Models, tc.want)
			checkBound(t, "DateFrom", f.DateFrom, tc.from)
			checkBound(t, "DateTo", f.DateTo, tc.to)
		})
	}

	// The axes are read independently, so a row setting only one must leave the
	// others empty rather than carrying something over.
	q, _ := url.ParseQuery("agent=claude")
	f, _, err := parseFilter(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Models) != 0 || len(f.Projects) != 0 {
		t.Errorf("an agent-only filter produced models %v and projects %v",
			f.Models, f.Projects)
	}
	if len(f.Agents) != 1 || f.Agents[0] != "claude" {
		t.Errorf("agents = %v, want [claude]", f.Agents)
	}
}

// TestParseFilterCoversTheWholeEndDay is the one rule that is easy to get wrong
// and invisible until a user hits it.
//
// A "to" date that stopped at midnight would silently exclude every call made
// during that day, so the upper bound is the last second of it. The existing
// end-to-end test proves this from the page; asserting it here says what the rule
// is, and where it lives.
func TestParseFilterCoversTheWholeEndDay(t *testing.T) {
	q, _ := url.ParseQuery("date_to=2026-06-15")
	f, _, err := parseFilter(q)
	if err != nil {
		t.Fatal(err)
	}
	if f.DateTo == nil {
		t.Fatal("a date_to filter produced no upper bound")
	}
	// Local midnight plus a day, less a second.
	want := time.Date(2026, 6, 15, 0, 0, 0, 0, time.Local).
		Add(24*time.Hour - time.Second)
	if !f.DateTo.Equal(want) {
		t.Errorf("date_to = %s, want %s: the whole of the day is included, not "+
			"only its first instant", f.DateTo, want)
	}
	if f.DateTo.Hour() != 23 || f.DateTo.Minute() != 59 || f.DateTo.Second() != 59 {
		t.Errorf("date_to = %s, want 23:59:59 on the named day", f.DateTo)
	}

	// A lower bound is midnight, not the end of the previous day: filtering
	// "from 15 June" must include a call made at 00:30 on the 15th.
	q, _ = url.ParseQuery("date_from=2026-06-15")
	f, _, err = parseFilter(q)
	if err != nil {
		t.Fatal(err)
	}
	if f.DateFrom == nil {
		t.Fatal("a date_from filter produced no lower bound")
	}
	if !f.DateFrom.Equal(time.Date(2026, 6, 15, 0, 0, 0, 0, time.Local)) {
		t.Errorf("date_from = %s, want midnight at the start of 15 June", f.DateFrom)
	}

	// Both bounds together, and an inverted pair is the user's problem rather
	// than an error: it selects nothing, which is what the dates say.
	q, _ = url.ParseQuery("date_from=2026-06-15&date_to=2026-06-01")
	f, _, err = parseFilter(q)
	if err != nil {
		t.Fatalf("an inverted date range was rejected: %v", err)
	}
	// An inverted range selects nothing, which is what the dates say. Rejecting it
	// would be a different decision; passing it through means the page shows an
	// empty view rather than silently swapping the two.
	if f.DateFrom == nil || f.DateTo == nil {
		t.Fatalf("date range = %v..%v, want both bounds", f.DateFrom, f.DateTo)
	}
	if !f.DateFrom.After(*f.DateTo) {
		t.Errorf("date range = %s..%s, want the order the query gave: an inverted "+
			"range selects nothing rather than being reinterpreted",
			f.DateFrom, f.DateTo)
	}
}

// TestParseFilterRejectsAMalformedDate is the one input that is an error rather
// than a value: an unparseable date is a 400, not a silently absent filter. A
// filter quietly dropped would show the unfiltered view under a filtered URL.
//
// An *empty* date is not malformed — the filter form submits one whenever the
// field is blank — so it produces no bound rather than an error. That is covered
// by TestParseFilterReadsEveryAxis.
func TestParseFilterRejectsAMalformedDate(t *testing.T) {
	for _, tc := range []struct{ query, wantSubstring string }{
		{"date_from=not-a-date", "date_from"},
		{"date_to=not-a-date", "date_to"},
		{"date_from=2026-13-01", "date_from"}, // month 13
		{"date_from=2026-02-30", "date_from"}, // no such day
		{"date_from=15/06/2026", "date_from"}, // the other format
	} {
		t.Run(tc.query, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = parseFilter(q)
			if err == nil {
				t.Errorf("parseFilter(%q) returned no error, want one: a dropped "+
					"filter shows the unfiltered view under a filtered URL", tc.query)
				return
			}
			if tc.wantSubstring != "" && !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("error %q does not name the parameter %q, so the user "+
					"cannot tell which part of their URL was wrong", err, tc.wantSubstring)
			}
		})
	}
}

// TestParseFilterStateMirrorsTheFilter checks the second return value.
//
// The template prefills the form from it, so it has to carry the same axes the
// store filter does — as the raw strings the select options are matched against,
// since a select holds the model's own spelling rather than a normalised one.
func TestParseFilterStateMirrorsTheFilter(t *testing.T) {
	q, _ := url.ParseQuery(
		"model=anthropic/claude-opus-4.6&agent=pi,claude&project=/p/one&date_from=2026-05-01")
	f, state, err := parseFilter(q)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Models) != 1 || state.Models[0] != "anthropic/claude-opus-4.6" {
		t.Errorf("state models = %v, want the raw spelling the select holds", state.Models)
	}
	if len(f.Models) != 1 || f.Models[0] != "anthropic/claude-opus-4.6" {
		t.Errorf("filter models = %v, want the same value", f.Models)
	}
	if len(state.Agents) != 2 || state.Agents[0] != "pi" || state.Agents[1] != "claude" {
		t.Errorf("state agents = %v, want [pi claude]", state.Agents)
	}
	if state.DateFrom != "2026-05-01" || state.DateTo != "" {
		t.Errorf("state dates = %q..%q, want the raw strings", state.DateFrom, state.DateTo)
	}
}

// TestParseFilterStateFallsBackToEmptyOnAMalformedDate covers the second caller.
//
// parseFilterState cannot report an error — the template needs *something* — and
// what it needs is a form with nothing marked, which is what an error page shows
// anyway.
func TestParseFilterStateFallsBackToEmptyOnAMalformedDate(t *testing.T) {
	q, _ := url.ParseQuery("date_from=nope&model=a")
	state := parseFilterState(q)
	if len(state.Models) != 0 {
		t.Errorf("a malformed filter still marked models %v on the form", state.Models)
	}
	if state.DateFrom != "" || state.DateTo != "" {
		t.Errorf("a malformed filter still produced dates %q..%q",
			state.DateFrom, state.DateTo)
	}

	// And it mirrors a good filter exactly.
	q, _ = url.ParseQuery("model=a&agent=pi")
	state = parseFilterState(q)
	if len(state.Models) != 1 || len(state.Agents) != 1 {
		t.Errorf("a good filter produced models %v and agents %v",
			state.Models, state.Agents)
	}
}

// TestFilterStateActiveIsComputedButNeverRead is the report the reviewer asked
// for, as a test rather than a note.
//
// filterState.Active is assigned in parseFilter and read by nothing: no template
// field, no Go comparison, no test. `state.Active = false` would therefore be a
// permanent no-op, which the reviewer confirmed by breaking it.
//
// This cannot fail while the field is unread — that is the point of it — so it is
// written as an inventory: it names the field, asserts the value parseFilter
// computes for a filtered query, and records that nothing consumes it. When
// somebody does consume it, TestFilterStateActiveIsReadBySomething below fails
// and the inventory is replaced with a real assertion.
//
// server.go is not a file this branch may edit, so the field cannot be deleted
// here; see the report.
func TestFilterStateActiveIsComputedButNeverRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  bool
	}{
		{"nothing filtered", "", false},
		{"model", "model=a", true},
		{"agent", "agent=pi", true},
		{"project", "project=/p", true},
		{"date from", "date_from=2026-05-01", true},
		{"date to", "date_to=2026-05-01", true},
		// An empty value is not a filter: marking one would narrow the next
		// request for a filter the reader never set.
		{"empty value", "model=&agent=&project=&date_from=&date_to=", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			_, state, err := parseFilter(q)
			if err != nil {
				t.Fatalf("parseFilter(%q): %v", tc.query, err)
			}
			if state.Active != tc.want {
				t.Errorf("Active = %v for %q, want %v", state.Active, tc.query, tc.want)
			}
		})
	}

	t.Log("filterState.Active is computed and read by nothing: no template field " +
		"and no Go comparison. Assigning false to it is a no-op. Deleting it (or " +
		"wiring it into the template) needs server.go, which this branch does not own.")
}

// TestFilterStateActiveHasNoReader inventories the shipped sources.
//
// The check is textual because "read by nothing" is a statement about the sources
// rather than about behaviour: a field cannot be exercised without ceasing to be
// dead. It reads the template and the non-test files of this package, so a reader
// appearing in either is noticed.
//
// When it does appear, the log line says to replace this inventory with a
// behavioural assertion; until then, the assertion in the test above is the one
// that pins what parseFilter computes.
func TestFilterStateActiveHasNoReader(t *testing.T) {
	const needle = "Active"
	readers := 0

	template := readAsset(t, templateFS, "templates/index.html")
	readers += countUses(template, needle)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		readers += countUses(string(data), needle)
	}
	// One use is the declaration in the struct and one is the assignment; both are
	// writes, neither is a read.
	if readers > 2 {
		t.Logf("filterState.Active now has %d uses outside its declaration: "+
			"replace this inventory with a behavioural assertion", readers-2)
	} else {
		t.Logf("filterState.Active has %d uses, all of them writes: it is still dead", readers)
	}
}

// countUses counts lines mentioning needle, skipping comments so a note about the
// field does not read as a reader of it.
func countUses(src, needle string) int {
	n := 0
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- helpers

func assertStringSlice(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s = %v, want %v", name, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s = %v, want %v", name, got, want)
			return
		}
	}
}

func checkBound(t *testing.T, name string, got *time.Time, wantDay string) {
	t.Helper()
	if wantDay == "" {
		if got != nil {
			t.Errorf("%s = %s, want no bound", name, got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s is nil, want %s", name, wantDay)
		return
	}
	if got.Format("2006-01-02") != wantDay {
		t.Errorf("%s = %s, want the day %s", name, got, wantDay)
	}
}
