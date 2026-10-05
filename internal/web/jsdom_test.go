package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A DOM for dashboard.js, and a way to run the real script in it.
//
// The layout tests used to grep dashboard.js's source text, which cannot tell a
// working comparator from one whose numeric branch was short-circuited to
// `false`, and which only counted `<td>`s — so swapping two cells' contents, or
// rendering `s.cost` under a Tokens heading, left the suite green. Four such
// mutations at once still passed.
//
// So the script is executed. node is the engine: it is present on all three CI
// platforms, and `vm` supplies a sandbox with the globals dashboard.js needs
// (URLSearchParams, Intl, Date). The stub DOM implements exactly what the script
// touches — getElementById, a handful of element properties, innerHTML as a
// settable string, and a fake fetch — so the code under test is the shipped
// file, unmodified. The functions under test are called directly by the body
// each test supplies; they are not stubbed.

// jsHarness loads dashboard.js into a stub DOM and then runs a body against it.
//
// Contract with the body: an async JavaScript function expression, invoked with
// no arguments, whose return value must be JSON-serialisable. In scope: the
// renderers and sort helpers the script declares, `document`, `elements` (id →
// stub element), and `fetch`. The script's own top-level bindings — projects,
// models, tools, and each table's sort state — are readable and mutable from the
// body, which is what lets a test change one payload field and re-render.
//
// A throw inside the script or the body exits non-zero with the stack on
// stderr, which runDashboardJS surfaces as the failure.
const jsHarness = `
const fs = require('fs');
const vm = require('vm');

const scriptPath = process.argv[2];
const bodyPath = process.argv[3];
const cfg = JSON.parse(fs.readFileSync(process.argv[4], 'utf8'));

// The one piece of real DOM behaviour the script depends on: assigning
// textContent and reading innerHTML back is how escapeHtml() escapes.
function escapeHTML(s) {
  return String(s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

// A listener registry, so a delegated document-level click handler can actually
// be registered and removed rather than being a no-op that hides wiring.
function listeners(target) {
  if (!target.__listeners) target.__listeners = new Map();
  return target.__listeners;
}

function addListener(target, type, fn) {
  const byType = listeners(target);
  if (!byType.has(type)) byType.set(type, []);
  byType.get(type).push(fn);
}

function removeListener(target, type, fn) {
  const byType = listeners(target);
  const fns = byType.get(type);
  if (!fns) return;
  const i = fns.indexOf(fn);
  if (i >= 0) fns.splice(i, 1);
}

// matches answers the selector forms the shipped script actually uses:
// attribute presence ([data-copy-resume]), class (.sort-icon) and tag. Anything
// else returns false rather than throwing, so a selector the stub cannot model
// behaves like a selector that matched nothing.
function matches(el, selector) {
  const sel = String(selector).trim();
  const attr = /^\[([a-zA-Z0-9_-]+)\]$/.exec(sel);
  if (attr) {
    const name = attr[1];
    if (name.startsWith('data-')) {
      const key = name.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase());
      return Object.prototype.hasOwnProperty.call(el.dataset, key) && el.dataset[key] !== undefined;
    }
    return el.getAttribute(name) !== null;
  }
  if (sel.startsWith('.')) return (el.className || '').split(/\s+/).includes(sel.slice(1));
  if (sel.startsWith('#')) return el.id === sel.slice(1);
  return el.tagName === sel.toUpperCase();
}

function makeElement(id) {
  const el = {
    id, tagName: 'DIV', innerHTML: '', textContent: '', value: '', title: '',
    className: '', disabled: false, style: {}, dataset: {}, options: [],
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    setAttribute() {}, getAttribute() { return null; }, removeAttribute() {},
    querySelector() { return null; }, querySelectorAll() { return []; },
    appendChild() {}, removeChild() {}, select() {}, focus() {},
  };
  el.addEventListener = (type, fn) => addListener(el, type, fn);
  el.removeEventListener = (type, fn) => removeListener(el, type, fn);
  // closest walks up from the event target. The stub has no tree, so an element
  // matches only itself, which is what the delegation in setupDelegatedActions
  // needs: the target it is handed is the button itself.
  el.closest = selector => (matches(el, selector) ? el : null);
  return el;
}

const elements = new Map();
for (const id of cfg.ids || []) elements.set(id, makeElement(id));

const document = {
  getElementById(id) {
    // Unknown ids answer null, as a browser would. A renderer targeting an
    // element the template lost therefore bails out exactly as it would on the
    // page, and the test sees an empty tbody rather than a false pass.
    return elements.has(id) ? elements.get(id) : null;
  },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  // The script registers its clickable-cell delegation on the document. The stub
  // records the handler so the wiring is exercised at load; a test body that
  // wants the behaviour can reach it through sandbox.dispatch('click', target).
  addEventListener(type, fn) { addListener(document, type, fn); },
  removeEventListener(type, fn) { removeListener(document, type, fn); },
  createElement(tag) {
    let text = '';
    return {
      tagName: String(tag).toUpperCase(), value: '', style: {},
      set textContent(v) { text = String(v); },
      get textContent() { return text; },
      get innerHTML() { return escapeHTML(text); },
      set innerHTML(v) { text = String(v); },
      setAttribute() {}, getAttribute() { return null; },
      appendChild() {}, removeChild() {}, select() {},
    };
  },
  body: makeElement('body'),
  execCommand() { return true; },
};

// dispatch fires the document-level handlers of one type at target, as a click
// on target would. Exposed so a test body can drive the delegation without
// reimplementing it.
function dispatch(type, target) {
  const fns = listeners(document).get(type) || [];
  return fns.map(fn => fn({type, target, preventDefault() {}, stopPropagation() {}}));
}

const sandbox = {
  console,
  document,
  window: {
    dashboardData: cfg.dashboardData || {},
    location: { search: cfg.search || '', pathname: '/', href: '/' },
    history: { replaceState() {} },
  },
  navigator: {},
  // The activity IIFE fetches its series on load. The response comes from the
  // test, so the activity table renderer runs against data the test chose.
  fetch: async () => {
    if (!cfg.activity) throw new Error('this test supplied no activity response');
    return { ok: true, status: 200, json: async () => cfg.activity };
  },
  setTimeout, clearTimeout, setInterval, clearInterval,
  URLSearchParams, URL, Intl, Date, Math, JSON, Object, Array, String, Number,
  Boolean, Map, Set, Promise, Error, RegExp, isNaN, parseInt, parseFloat,
};
sandbox.globalThis = sandbox;
sandbox.window.document = document;
// Exposed so a test body can seed an element's options before invoking a script
// it did not write — the prefill script marks options, and there is nothing to
// mark on an element with none.
sandbox.elements = elements;
sandbox.dispatch = dispatch;
vm.createContext(sandbox);

// A browser exposes location on the global object as well as on window, and the
// page's inline filter-prefill script reads the bare name.
sandbox.location = sandbox.window.location;

// Loading the script is itself an assertion: a top-level throw — a reference to
// an element that no longer exists, a syntax error from a bad edit — fails here
// rather than quietly skipping every test below.
vm.runInContext(fs.readFileSync(scriptPath, 'utf8'), sandbox, {
  filename: 'dashboard.js',
});

(async () => {
  // The activity IIFE loads its series asynchronously at startup; let that settle
  // before the body runs, so a test can inspect activity-tbody without racing.
  await new Promise(r => setTimeout(r, 25));
  const body = fs.readFileSync(bodyPath, 'utf8');
  const out = await vm.runInContext('(' + body + ')()', sandbox, { filename: 'body.js' });
  process.stdout.write(JSON.stringify(out === undefined ? null : out));
})().catch(err => {
  console.error(err && err.stack ? err.stack : String(err));
  process.exit(1);
});
`

// jsConfig is the input a test hands the harness.
type jsConfig struct {
	// Ids are the element ids the stub DOM answers for, taken from the template
	// rather than hand-listed.
	Ids []string `json:"ids"`
	// Data becomes window.dashboardData.
	Data map[string]any `json:"dashboardData"`
	// Activity is what /api/activity answers with.
	Activity map[string]any `json:"activity"`
	// Search is window.location.search.
	Search string `json:"search"`
}

// runDashboardJS executes assets/dashboard.js in the stub DOM, runs body against
// it, and decodes the body's return value into out.
//
// A missing node is a hard failure, not a skip. These tests exist because a
// source-text check could not tell working code from broken code; skipping them
// when node is missing would reinstate exactly that.
func runDashboardJS(t *testing.T, cfg jsConfig, body string, out any) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node is required to execute dashboard.js and was not found on PATH: %v\n"+
			"Install node (https://nodejs.org): the JS behaviour tests cannot be met by "+
			"reading the script's source instead", err)
	}

	dir := t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("dashboard.js", readAsset(t, assets, "assets/dashboard.js"))
	write("body.js", body)
	write("js_harness.js", jsHarness)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	write("cfg.json", string(raw))

	cmd := exec.Command(node, "js_harness.js", "dashboard.js", "body.js", "cfg.json")
	cmd.Dir = dir
	cmdOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running dashboard.js failed: %v\n%s", err, cmdOut)
	}
	if err := json.Unmarshal(cmdOut, out); err != nil {
		t.Fatalf("harness produced %q, which is not JSON: %v", cmdOut, err)
	}
}

// templateIDRe matches every id the shipped template declares.
var templateIDRe = regexp.MustCompile(`\bid="([^"]+)"`)

// templateIDs is every element id the page declares.
//
// It is the harness's element list rather than a hand-written one: the point of
// the DOM stub is that getElementById answers for what the page actually
// contains, so an element the template lost makes the renderer under test
// return early and the test fail, rather than pass.
func templateIDs(t *testing.T) []string {
	t.Helper()
	page := readAsset(t, templateFS, "templates/index.html")
	var ids []string
	seen := map[string]bool{}
	for _, m := range templateIDRe.FindAllStringSubmatch(page, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			ids = append(ids, m[1])
		}
	}
	if len(ids) == 0 {
		t.Fatal("the template declares no element ids; the JS harness would stub nothing")
	}
	return ids
}

// tableTagsRe matches a cell opening tag, with or without a colspan.
var tdRe = regexp.MustCompile(`(?s)<td\b([^>]*)>(.*?)</td>`)

// renderedCells splits a rendered tbody into the data row's cell contents.
//
// Only the first row is read, and colspan cells are dropped: those are the
// empty-state and expandable rows, which are spans rather than columns. Without
// the colspan filter a table showing an expanded row beside its data row would
// report a cell count unrelated to its column count.
func renderedCells(t *testing.T, tbody string) []string {
	t.Helper()
	first := tbody
	if i := strings.Index(first, "</tr>"); i >= 0 {
		first = first[:i]
	}
	var cells []string
	for _, m := range tdRe.FindAllStringSubmatch(first, -1) {
		if strings.Contains(m[1], "colspan") {
			continue
		}
		cells = append(cells, m[2])
	}
	return cells
}
