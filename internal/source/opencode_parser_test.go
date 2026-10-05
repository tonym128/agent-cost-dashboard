package source

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// OpenCode is the only parser whose input is a single third-party SQLite
// database holding every session in every project. Nothing here is under this
// project's control: the file is written by another program, it can be upgraded
// or replaced under us, and it can be truncated or corrupt. That makes two
// different things worth testing, and the second matters more than the first.
//
// The happy path — do the rows become the right calls — is worth pinning because
// a token silently landing in the wrong bucket is the failure this project
// exists to prevent. But a parser that reads a schema it no longer recognises
// has to fail loudly. A new OpenCode release that renames a column, or a
// truncated database, must produce an error a user can act on and not a
// plausible-looking total of zero.

// parseOne is a small helper for the many single-session reads below.
func parseOne(t *testing.T, dbPath, sessionID string) (model.SessionWrite, model.ScanState) {
	t.Helper()
	sw, state, err := NewOpenCodeParser(dbPath).Parse(sessionID, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatalf("parse %s: %v", sessionID, err)
	}
	return sw, state
}

// TestOpenCodeSessionRowsBecomeCalls covers the happy path end to end: the
// session row, the assistant message and its parts.
func TestOpenCodeSessionRowsBecomeCalls(t *testing.T) {
	path := buildOpenCodeDB(t)
	p := NewOpenCodeParser(path)
	sw, state := parseOne(t, path, "ses_test0000000001")
	checkInvariants(t, sw)

	if got, want := sw.Session.UID, "ses_test0000000001"; got != want {
		t.Errorf("session uid = %q, want %q", got, want)
	}
	if sw.Session.Agent != model.AgentOpencode {
		t.Errorf("agent = %q, want %q", sw.Session.Agent, model.AgentOpencode)
	}
	if got, want := sw.Session.Project, "/home/testuser/project"; got != want {
		t.Errorf("project = %q, want %q", got, want)
	}
	if got, want := sw.Session.Title, "Fix the token assertion"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if p.Agent() != model.AgentOpencode {
		t.Errorf("Agent = %q, want %q", p.Agent(), model.AgentOpencode)
	}
	if got := p.SessionUID(path); got == "" {
		t.Error("SessionUID is empty: every stored row hangs off one")
	}

	// The user message carries no tokens block and must not become a call. Only
	// the assistant message does.
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1 (the user message has no usage)", len(sw.Calls))
	}
	c := sw.Calls[0]
	// The call key is the message id, so re-scanning the same message replaces
	// its row rather than duplicating it.
	if got, want := c.CallKey, "msg:msg_assistant000001"; got != want {
		t.Errorf("call key = %q, want %q", got, want)
	}

	// OpenCode's four buckets are disjoint: unlike Anthropic's, output here
	// excludes reasoning, so the two are summed rather than carved out of one
	// figure, and the input figure does not include the cached part.
	if c.InputTokens != 24000 {
		t.Errorf("input = %d, want 24000", c.InputTokens)
	}
	if c.OutputTokens != 4210 {
		t.Errorf("output = %d, want 4210", c.OutputTokens)
	}
	if c.ReasoningTokens != 1895 {
		t.Errorf("reasoning = %d, want 1895", c.ReasoningTokens)
	}
	// These are nested under `cache` rather than stored flat. Reading them as
	// the flat keys "cache.read" and "cache.write" found nothing and reported
	// both as zero, billing every cached token at the input rate instead of the
	// cache rate.
	if c.CacheReadTokens != 18000 {
		t.Errorf("cache read = %d, want 18000", c.CacheReadTokens)
	}
	if c.CacheWriteTokens != 1200 {
		t.Errorf("cache write = %d, want 1200", c.CacheWriteTokens)
	}

	// Totals reconcile: the call is the sum of its own buckets, and nothing
	// else is folded in.
	if want := c.InputTokens + c.OutputTokens + c.CacheReadTokens + c.CacheWriteTokens; c.TotalTokens != want {
		t.Errorf("total = %d, want the sum of the buckets %d", c.TotalTokens, want)
	}

	// The cost comes from the pricer, and it must reflect that 18000 tokens were
	// read from cache and 1200 written to it rather than billed as input.
	if !c.Priced {
		t.Fatal("the call is unpriced: claude-sonnet-4-6 resolves")
	}
	rates, ok := testPricer(t).Resolve("claude-sonnet-4-6")
	if !ok {
		t.Fatal("claude-sonnet-4-6 is unpriced")
	}
	wantCost := float64(c.InputTokens)/1e6*rates.Input +
		float64(c.OutputTokens+c.ReasoningTokens)/1e6*rates.Output +
		float64(c.CacheReadTokens)/1e6*rates.CacheRead +
		float64(c.CacheWriteTokens)/1e6*rates.CacheWrite
	if diff := c.CostUSD - wantCost; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost $%.8f, want $%.8f (cache buckets priced as cache, not input)",
			c.CostUSD, wantCost)
	}
	// A cache read is cheaper than an input token. If the two came out equal the
	// nested lookup above is not being read and the cost is wrong in a way the
	// arithmetic above would not show on its own.
	//
	// This was a mid-test t.Skipf, which is worse than useless: if the
	// relationship ever stopped holding it truncated the rest of the test — the
	// wall-clock bound, the tool count, the "no transcript file" claim — so those
	// assertions would silently stop being checked. It is now a fatal, before the
	// assertions it used to skip, and states what to do about it.
	if rates.CacheRead >= rates.Input {
		t.Fatalf("resolved rates for claude-sonnet-4-6 price a cache read at $%.2f/M, "+
			"which is not below the $%.2f/M input rate: the nested cache lookup this "+
			"test checks cannot be demonstrated against these rates. If the vendor has "+
			"genuinely changed them, pick a model in the fixture whose rates do have "+
			"that shape rather than deleting the check.",
			rates.CacheRead, rates.Input)
	}

	// The wall-clock the provider reported is kept, bounded: an unfinished
	// message reports a completion time hours later and that is not a
	// response time.
	if got, want := c.LLMSeconds, 2.015; got != want {
		t.Errorf("llm seconds = %v, want %v", got, want)
	}

	// One tool part. The step-start, reasoning, text and step-finish parts are
	// not tools and must not be counted as such.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1", len(sw.ToolCalls))
	}
	tool := sw.ToolCalls[0]
	if tool.Tool != "bash" {
		t.Errorf("tool = %q, want bash", tool.Tool)
	}
	if got, want := tool.Seconds, 0.2; got != want {
		t.Errorf("tool seconds = %v, want %v", got, want)
	}
	if tool.IsError {
		t.Error("a completed tool was recorded as an error")
	}

	// There is no transcript file behind this session, so the write carries no
	// path: filling it with the database would render a link that cannot resolve.
	if sw.Path != "" {
		t.Errorf("path = %q, want empty: no transcript file exists", sw.Path)
	}
	if state.Cursor != model.CursorTimeMax {
		t.Errorf("cursor = %q, want %q", state.Cursor, model.CursorTimeMax)
	}
	if state.Offset <= 0 {
		t.Error("no time cursor recorded after a full read")
	}
}

// TestOpenCodeModelColumnShapes covers the column that has been observed holding
// three different things. Resolving it wrongly does not error — it prices the
// whole session at zero, which is the quietest possible failure.
func TestOpenCodeModelColumnShapes(t *testing.T) {
	tests := []struct {
		name, column, want string
	}{
		{"json object", `{"id":"claude-sonnet-4-6","providerID":"anthropic"}`, "claude-sonnet-4-6"},
		// A bare string is not valid JSON, so the unmarshal fails and the column
		// is used as written.
		{"bare string", "claude-opus-4-8", "claude-opus-4-8"},
		// Valid JSON that is not an object used to abort the export.
		{"json array", `["claude-sonnet-4-6"]`, `["claude-sonnet-4-6"]`},
		{"json object without id", `{"providerID":"anthropic"}`, ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := opencodeModelName(sql.NullString{String: tc.column, Valid: true}); got != tc.want {
				t.Errorf("model column %s resolved to %q, want %q", tc.column, got, tc.want)
			}
		})
	}
	if got := opencodeModelName(sql.NullString{}); got != "" {
		t.Errorf("a NULL model column resolved to %q, want empty", got)
	}
}

// TestOpenCodeSessionWithoutTitleAndTokens covers the two rows that carry no
// money and must still be handled without inventing any.
func TestOpenCodeSessionWithoutTitleAndTokens(t *testing.T) {
	path := buildOpenCodeDB(t)
	sw, _ := parseOne(t, path, "ses_test0000000002")
	checkInvariants(t, sw)

	// An empty title falls back to the project directory's name, so the session
	// is still identifiable in the list.
	if got, want := sw.Session.Title, "other-project"; got != want {
		t.Errorf("title = %q, want %q (the directory's base name)", got, want)
	}
	if got, want := sw.Session.Project, "/home/testuser/other-project"; got != want {
		t.Errorf("project = %q, want %q", got, want)
	}

	// The second assistant message has no tokens block at all, so only the first
	// becomes a call.
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1 (one message carries no tokens block)", len(sw.Calls))
	}
	c := sw.Calls[0]
	// This session's model column is a bare string rather than a JSON object,
	// and both have to reach the pricer.
	if c.Model != "claude-opus-4-8" {
		t.Errorf("model = %q, want claude-opus-4-8", c.Model)
	}
	if !c.Priced || c.CostUSD <= 0 {
		t.Errorf("call is unpriced at $%v", c.CostUSD)
	}
	// Zero-valued cache buckets stay zero rather than going negative or being
	// invented from the reasoning figure.
	if c.CacheReadTokens != 0 || c.CacheWriteTokens != 0 {
		t.Errorf("cache = %d read / %d write, want 0 / 0", c.CacheReadTokens, c.CacheWriteTokens)
	}

	// OpenCode files input it could not parse under a synthetic "invalid" tool
	// and keeps the real name beside the parse error, so the real one wins.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1", len(sw.ToolCalls))
	}
	tool := sw.ToolCalls[0]
	if tool.Tool != "read" {
		t.Errorf("tool = %q, want read (the name beside the parse error, not \"invalid\")", tool.Tool)
	}
	if !tool.IsError {
		t.Error("a tool whose state reports an error was recorded as successful")
	}
}

// TestOpenCodeUnusableDatabase is the important half of this file.
//
// A panic here takes down the whole scan, so each case below must return an
// error rather than crash; the test failing is the assertion, and a panic fails
// it louder than any comparison could. A *wrong schema* is the case worth
// caring about most: OpenCode ships its own migrations and may rename or drop a
// column under us at any release, and the correct behaviour is a loud failure
// rather than a scan that quietly reports nothing.
func TestOpenCodeUnusableDatabase(t *testing.T) {
	dir := t.TempDir()
	pricer := testPricer(t)

	tests := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"missing file", func(t *testing.T) string {
			return filepath.Join(dir, "does-not-exist.db")
		}},
		{"right filename, wrong schema", func(t *testing.T) string {
			// The file is a perfectly good SQLite database and it has exactly the
			// name the parser looks for. Nothing about it announces the problem.
			path := filepath.Join(dir, "wrong-schema.db")
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE unrelated (a TEXT, b INTEGER)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`INSERT INTO unrelated VALUES ('x', 1)`); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"right tables, wrong columns", func(t *testing.T) string {
			// A partial schema: the tables the parser selects are all present but
			// one has lost a column, which is the shape a botched migration leaves.
			path := filepath.Join(dir, "missing-column.db")
			db, err := sql.Open("sqlite", "file:"+path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, stmt := range []string{
				`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)`,
				`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT)`,
				`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT)`,
			} {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO session VALUES ('ses_x', '/tmp/p')`); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"not a sqlite file", func(t *testing.T) string {
			path := filepath.Join(dir, "not-a-database.db")
			if err := os.WriteFile(path, []byte("this is a text file, not a database\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"truncated sqlite header", func(t *testing.T) string {
			// A database that was being written when the process died: the magic
			// string is there and the file stops.
			path := filepath.Join(dir, "truncated.db")
			if err := os.WriteFile(path, []byte("SQLite format 3\x00"), 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"empty file", func(t *testing.T) string {
			path := filepath.Join(dir, "empty.db")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return path
		}},
		{"directory", func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "a-directory.db")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup(t)

			// A missing database must not be reported as an empty session. The
			// returned cursor is the only thing a caller would act on, so it has
			// to be one that will be re-read rather than one that says "complete".
			_, state, err := NewOpenCodeParser(path).Parse("ses_x", model.ScanState{}, pricer)
			if err == nil {
				t.Fatal("parsing an unusable database returned no error: a schema change would silently bill nothing")
			}
			if state.SessionUID != "ses_x" || state.Cursor != model.CursorTimeMax {
				t.Errorf("cursor after the failure = %q/%q, want the session retried from the start",
					state.SessionUID, state.Cursor)
			}

			// The batched path opens the same file and must fail the same way.
			if _, _, err := NewOpenCodeParser(path).ParseAll([]string{"ses_x"}, nil, pricer); err == nil {
				t.Error("ParseAll accepted an unusable database")
			}

			// And OpenReadOnly is exported for callers outside this package, so
			// it has to survive the same input and fail at query time rather than
			// handing back a handle that panics on use. The query is the one the
			// parser itself runs, so a schema it cannot read fails here too.
			db, err := OpenReadOnly(path)
			if err != nil {
				return // refusing to open at all is fine
			}
			defer db.Close()
			_, err = db.Query("SELECT directory, title, agent, model, time_created,"+
				" time_updated FROM session WHERE id = ?", "ses_x")
			if err == nil {
				t.Error("the parser's own query succeeded against a database it cannot read")
			}
		})
	}
}

// TestOpenCodeUnknownSessionIsAnError: a session id the database does not hold
// is a caller bug or a stale index, and reporting it as an empty session would
// delete the row the caller meant to update.
func TestOpenCodeUnknownSessionIsAnError(t *testing.T) {
	path := buildOpenCodeDB(t)
	_, _, err := NewOpenCodeParser(path).Parse("ses_not_in_the_database", model.ScanState{}, testPricer(t))
	if err == nil {
		t.Fatal("an unknown session id returned no error")
	}
}

// TestOpenCodeParseAllOverOneConnection covers the batched entry point, which
// exists because re-opening a database per session cost a whole pass of wall
// time for no benefit.
func TestOpenCodeParseAllOverOneConnection(t *testing.T) {
	path := buildOpenCodeDB(t)
	pricer := testPricer(t)
	p := NewOpenCodeParser(path)

	ids := []string{"ses_test0000000001", "ses_test0000000002"}
	writes, states, err := p.ParseAll(ids, nil, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != len(ids) || len(states) != len(ids) {
		t.Fatalf("got %d writes and %d states, want %d of each", len(writes), len(states), len(ids))
	}
	for _, id := range ids {
		if _, ok := writes[id]; !ok {
			t.Errorf("ParseAll returned no session for %s", id)
		}
		if got := writes[id].Session.UID; got != id {
			t.Errorf("session %s came back as %q", id, got)
		}
		if states[id].SessionUID != id {
			t.Errorf("state for %s carries uid %q", id, states[id].SessionUID)
		}
	}
	// The batch must agree with the single-session path: the same session parsed
	// two ways is the same session.
	single, _ := parseOne(t, path, ids[0])
	if got, want := len(writes[ids[0]].Calls), len(single.Calls); got != want {
		t.Errorf("ParseAll returned %d calls for %s, Parse returned %d", got, ids[0], want)
	}
	for i, c := range writes[ids[0]].Calls {
		if c.TotalTokens != single.Calls[i].TotalTokens || c.CallKey != single.Calls[i].CallKey {
			t.Errorf("call %d differs between ParseAll and Parse: %+v vs %+v", i, c, single.Calls[i])
		}
	}

	// An empty id list is a no-op, not an error: a pass with nothing to do is
	// the common case and must not open the database at all.
	writes, states, err = p.ParseAll(nil, nil, pricer)
	if err != nil || len(writes) != 0 || len(states) != 0 {
		t.Errorf("ParseAll(nil) = %d writes / %d states / %v, want empty and no error",
			len(writes), len(states), err)
	}

	// One bad id fails the batch rather than silently dropping that session:
	// returning a partial result would let a caller commit half a pass.
	if _, _, err := p.ParseAll([]string{ids[0], "ses_absent"}, nil, pricer); err == nil {
		t.Error("ParseAll reported success with an unknown session in the batch")
	}
}

// TestOpenCodeResumePicksUpNewMessages guards the per-session cursor. OpenCode
// appends to one shared database continuously, so a scan must return only what
// arrived since — and must return it.
func TestOpenCodeResumePicksUpNewMessages(t *testing.T) {
	path := buildOpenCodeDB(t)
	pricer := testPricer(t)
	p := NewOpenCodeParser(path)

	_, state, err := p.Parse("ses_test0000000001", model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	// A rescan of an unchanged database finds nothing.
	again, _, err := p.Parse("ses_test0000000001", state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Calls) != 0 {
		t.Errorf("an unchanged session returned %d calls, want 0", len(again.Calls))
	}

	// Append a message, as OpenCode would.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const added = 1779976550000
	if _, err := db.Exec(
		`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`,
		"msg_appended00001", "ses_test0000000001", added, added,
		`{"role":"assistant","tokens":{"input":5,"output":6,"reasoning":0},`+
			`"modelID":"claude-sonnet-4-6"}`); err != nil {
		t.Fatal(err)
	}

	increment, next, err := p.Parse("ses_test0000000001", state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, increment)
	if len(increment.Calls) != 1 {
		t.Fatalf("the increment returned %d calls, want 1", len(increment.Calls))
	}
	if got, want := increment.Calls[0].CallKey, "msg:msg_appended00001"; got != want {
		t.Errorf("call key = %q, want %q", got, want)
	}
	if increment.Calls[0].SessionUID != "ses_test0000000001" {
		t.Errorf("the increment lost the session uid: %q", increment.Calls[0].SessionUID)
	}
	if next.Offset <= state.Offset {
		t.Errorf("cursor did not advance: %d then %d", state.Offset, next.Offset)
	}
}
