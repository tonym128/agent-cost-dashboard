package web

import (
	"strings"
	"testing"
)

// The escaper in dashboard.js used to round-trip a value through a detached
// element's innerHTML, which escapes & < > and neither quote. Every value it
// guards is read out of an agent log, so a project path of
//
//	/tmp/evil" onmouseover="window.__xss1=1
//
// closed its title= attribute and appended a live event handler. The rendered
// result was
//
//	<td class="project-name" title="/tmp/evil" onmouseover="window.__xss1=1">
//
// This asserts on the escaper itself rather than on the served page, because
// the served page's attributes are written by the script and only exist in a
// browser; asserting that the script's own output cannot contain an unescaped
// quote is the check that holds without one.
func TestJSEscaperNeutralisesQuotesAndBacktick(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")

	// The old implementation, which must not come back.
	if strings.Contains(script, "div.textContent = text") ||
		strings.Contains(script, "div.innerHTML;") {
		t.Error("escapeHtml is back to an innerHTML round-trip, which leaves " +
			`" and ' unescaped and lets a log close an attribute`)
	}

	// The replacement has to cover both quotes: a value can land in a
	// double-quoted attribute, and a handler written with single quotes would
	// still fire.
	for _, want := range []string{`'"': '&quot;'`, `"'": '&#39;'`} {
		if !strings.Contains(script, want) {
			t.Errorf("escapeHtml does not escape this pair: %s", want)
		}
	}
	if !strings.Contains(script, "`") || !strings.Contains(script, "&#96;") {
		t.Error("escapeHtml does not escape the backtick, so a value dropped " +
			"into a JS string or template literal is still live")
	}
}

// An attribute-context sink is only safe if every value reaching it goes through
// the escaper. The title= attributes are the ones a crafted log reaches: the
// project path, the session cwd and the model name all come out of the log.
func TestJSAttributeSinksEscapeTheirValues(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")

	for _, sink := range []string{
		`title="${escapeHtml(`,
		`data-resume-cmd="${escapeHtml(`,
	} {
		if !strings.Contains(script, sink) {
			t.Errorf("no %q sink: an unescaped value in an attribute is an injection", sink)
		}
	}

	// The two interpolations that are not escaped must not carry log-derived
	// text. Both are numbers the client formats itself.
	for _, raw := range []string{
		`title="${formatFullNumber(`,
		`title="${escapeHtml(tokenTitle(`,
	} {
		if !strings.Contains(script, raw) {
			t.Errorf("expected the formatted-number title %q to still be there", raw)
		}
	}
}

// The project and session names are the two title= sinks a log can reach. Both
// must be escaped rather than sliced into the attribute raw.
func TestJSProjectAndSessionNamesAreEscapedNotRaw(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")

	for _, raw := range []string{
		`title="${p.name}"`,
		`title="${s.cwd}"`,
		`>${p.name}<`,
		`>${s.cwd}<`,
	} {
		if strings.Contains(script, raw) {
			t.Errorf("dashboard.js interpolates log data raw: %s", raw)
		}
	}
	for _, escaped := range []string{
		`title="${escapeHtml(p.name)}"`,
		`title="${escapeHtml(s.cwd)}"`,
	} {
		if !strings.Contains(script, escaped) {
			t.Errorf("dashboard.js does not have %s", escaped)
		}
	}
}

// stripLineComments removes // comments so that a handler named in prose above
// the escaper — this file has to be able to explain the payload it guards
// against — does not count as a live handler.
func stripLineComments(script string) string {
	var out []string
	for _, line := range strings.Split(script, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Inline handlers are what a CSP has to allow 'unsafe-inline' for, and
// 'unsafe-inline' in script-src also permits a handler an injected attribute
// managed to add. Each one is therefore a data- attribute read by a delegated
// listener instead, which is what lets the CSP drop 'unsafe-inline'.
func TestJSHasNoInlineEventHandlers(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")
	page := readAsset(t, templateFS, "templates/index.html")

	code := stripLineComments(script)
	for _, on := range []string{"onclick=", "onmouseover=", "onerror="} {
		if strings.Contains(code, on) {
			t.Errorf("dashboard.js contains an inline handler %q", on)
		}
		if strings.Contains(page, on) {
			t.Errorf("templates/index.html contains an inline handler %q", on)
		}
	}

	// Every handler that was there has to have somewhere to live now, or the
	// button silently stops working.
	for _, dataAttr := range []string{
		"data-copy-resume",
		"data-toggle-project",
		"data-toggle-daily-chart",
		"data-reload",
	} {
		if !strings.Contains(script, dataAttr) {
			t.Errorf("dashboard.js wires no %s listener, so the control is dead", dataAttr)
		}
	}
	if !strings.Contains(script, "setupDelegatedActions();") {
		t.Error("the delegated listener is defined but never installed")
	}
}

// TestJSSortableColumnsKeepTheirColumnHeaderRole locks the accessibility fix in
// setupSorting. Applying role="button" to the <th> overwrote its implicit
// columnheader role, which cost every table on the page its column headers to a
// screen reader and made the aria-sort on the same element invalid. The sort
// control is now a real <button> inside the heading.
//
// This asserts on the script text because the DOM stub cannot model the
// "#id th[data-sort]" query setupSorting runs; the behavioural gap that leaves
// is called out in the review.
func TestJSSortableColumnsKeepTheirColumnHeaderRole(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")
	if strings.Contains(script, "setAttribute('role', 'button')") {
		t.Error("dashboard.js still sets role=button on a sortable heading, " +
			"which removes the columnheader role and invalidates aria-sort")
	}
	if !strings.Contains(script, "sort-btn") {
		t.Error("the sortable heading no longer builds a sort-btn button, so the " +
			"column is left with no keyboard-operable control")
	}
}

// TestJSProjectDisclosureIsKeyboardOperable locks the second blocker: the project
// drill-down row was click-only, so its entire model/tool breakdown was
// unreachable by keyboard (WCAG 2.1.1).
func TestJSProjectDisclosureIsKeyboardOperable(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")
	code := stripLineComments(script)
	if !strings.Contains(script, `data-toggle-project="${rowId}" tabindex="0"`) {
		t.Error("the project row carries no tabindex, so the drill-down cannot be " +
			"reached by keyboard")
	}
	if !strings.Contains(script, "aria-expanded") {
		t.Error("the project row never sets aria-expanded, so its open state is " +
			"not announced")
	}
	if !strings.Contains(code, "keydown") {
		t.Error("no keydown handler exists, so Enter/Space cannot open the row")
	}
}
