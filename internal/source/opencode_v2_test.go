package source

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// OpenCode 2 moved its storage. Sessions went from `session` to `session_v2`,
// messages from `message` to `session_message` — with the role taken out of the
// JSON blob and into a column — and a tool call stopped being a row in the part
// table and became an entry in the message's own content array. The migration
// copied rather than moved, so a real upgraded database holds both shapes at the
// same time.
//
// A reader that knows only the 1.x tables does not fail against that database:
// it reads the old tables, finds them full of old sessions, and reports a
// complete-looking answer that silently omits every session created since the
// upgrade. These tests pin the other behaviour — both shapes read, and each
// session counted exactly once.

// TestOpenCodeV2SessionBecomesCalls covers the 2.x shape end to end. Every field
// read here is one the 1.x reader had no way to reach: the session comes from
// `session_v2`, the role from a column, the model from an object, and the tool
// call from inside the message blob.
func TestOpenCodeV2SessionBecomesCalls(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	sw, state := parseOne(t, path, "ses_v2test000000001")
	checkInvariants(t, sw)

	if got, want := sw.Session.UID, "ses_v2test000000001"; got != want {
		t.Errorf("session uid = %q, want %q", got, want)
	}
	if sw.Session.Agent != model.AgentOpencode {
		t.Errorf("agent = %q, want %q", sw.Session.Agent, model.AgentOpencode)
	}
	if got, want := sw.Session.Project, "/home/testuser/project"; got != want {
		t.Errorf("project = %q, want %q", got, want)
	}
	if got, want := sw.Session.Title, "The 2.x tables are not read by a 1.x reader"; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}

	// The user message carries no tokens block, so only the assistant message
	// becomes a call.
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	c := sw.Calls[0]
	// The same key as 1.x, so a session that is read under both shapes — a
	// migrated one — never produces two rows for one message.
	if got, want := c.CallKey, "msg:msg_v2asst00000001"; got != want {
		t.Errorf("call key = %q, want %q", got, want)
	}
	// 2.x writes the model as an object with the provider beside it; the reader
	// has to reach inside it or every 2.x call is priced as "unknown".
	if got, want := c.Model, "claude-sonnet-4-6"; got != want {
		t.Errorf("model = %q, want %q", got, want)
	}
	if c.InputTokens != 12000 || c.OutputTokens != 900 || c.ReasoningTokens != 400 {
		t.Errorf("tokens = %d in / %d out / %d reasoning, want 12000 / 900 / 400",
			c.InputTokens, c.OutputTokens, c.ReasoningTokens)
	}
	// The cache buckets are nested exactly as they are in 1.x, so the same
	// lookup applies; reading them flat is what used to bill 30000 cached
	// tokens at the input rate.
	if c.CacheReadTokens != 30000 || c.CacheWriteTokens != 0 {
		t.Errorf("cache = %d read / %d write, want 30000 / 0",
			c.CacheReadTokens, c.CacheWriteTokens)
	}
	want := c.InputTokens + c.OutputTokens + c.ReasoningTokens +
		c.CacheReadTokens + c.CacheWriteTokens
	if c.TotalTokens != want {
		t.Errorf("total = %d, want the sum of the five buckets %d", c.TotalTokens, want)
	}
	if !c.Priced {
		t.Error("the call is unpriced: claude-sonnet-4-6 resolves")
	}
	if got, want := c.LLMSeconds, 3.9; got != want {
		t.Errorf("llm seconds = %v, want %v", got, want)
	}

	// A 2.x session has no part rows at all, so the tool call can only come from
	// the message's content array. Missing this is what made every 2.x session
	// report zero tool calls.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1", len(sw.ToolCalls))
	}
	tool := sw.ToolCalls[0]
	if tool.Tool != "bash" {
		t.Errorf("tool = %q, want bash", tool.Tool)
	}
	if got, want := tool.Seconds, 0.6; got != want {
		t.Errorf("tool seconds = %v, want %v", got, want)
	}
	if tool.IsError {
		t.Error("a completed tool was recorded as an error")
	}

	// No transcript file exists behind either shape, so the write still carries
	// no path.
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

// TestOpenCodeV2RowShapes covers the rows a 2.x session is allowed to contain
// without inventing anything: no title, a bare-string model column, an assistant
// turn that never reached the model, and a non-assistant message.
func TestOpenCodeV2RowShapes(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	sw, _ := parseOne(t, path, "ses_v2test000000002")
	checkInvariants(t, sw)

	// An empty title still falls back to the directory's base name.
	if got, want := sw.Session.Title, "v2-project"; got != want {
		t.Errorf("title = %q, want %q (the directory's base name)", got, want)
	}
	// Neither of the two assistant messages carries no tokens; only the one that
	// reached the model does. The system message is a different type entirely
	// and must not be mistaken for a call.
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	c := sw.Calls[0]
	if got, want := c.Model, "claude-opus-4-8"; got != want {
		t.Errorf("model = %q, want %q", got, want)
	}
	if c.CacheWriteTokens != 500 {
		t.Errorf("cache write = %d, want 500", c.CacheWriteTokens)
	}
	if !c.Priced || c.CostUSD <= 0 {
		t.Errorf("call is unpriced at $%v", c.CostUSD)
	}

	// The "invalid" name is what 2.x files unparseable input under, with the
	// real one beside the parse error, exactly as 1.x did.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1", len(sw.ToolCalls))
	}
	if got, want := sw.ToolCalls[0].Tool, "read"; got != want {
		t.Errorf("tool = %q, want read", got)
	}
	if !sw.ToolCalls[0].IsError {
		t.Error("a tool whose state reports an error was recorded as successful")
	}
}

// TestOpenCodeMigratedSessionIsCountedOnce is the transitional case, and the one
// that decides whether the two shapes can be read together at all: the same
// session, its messages and its tool calls are each present in the old tables
// and the new ones.
//
// Reading one set of tables loses whichever set it does not read; reading both
// without deduplicating counts every call and every tool twice.
func TestOpenCodeMigratedSessionIsCountedOnce(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	sw, _ := parseOne(t, path, "ses_migratedtest0001")
	checkInvariants(t, sw)

	// Two calls: the message the migration copied into `session_message`, and
	// the one it left behind in `message`. Reading only the new table drops the
	// second — which is 163 messages on one real database — and reading only the
	// old one drops every 2.x session.
	if len(sw.Calls) != 2 {
		t.Fatalf("parsed %d calls, want 2: the message in both tables is one, the one only in `message` is another", len(sw.Calls))
	}
	keys := map[string]bool{}
	for _, c := range sw.Calls {
		keys[c.CallKey] = true
		if !c.Priced {
			t.Errorf("call %s is unpriced", c.CallKey)
		}
	}
	for _, want := range []string{"msg:msg_migasst00000001", "msg:msg_migonly0000001"} {
		if !keys[want] {
			t.Errorf("no call keyed %q; got %v", want, keys)
		}
	}

	// One tool call, not two: the inline content array and the part table hold
	// the same call for a migrated message. Parts are preferred when present,
	// which is also what keeps an incremental pass from re-reading them.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1: a migrated tool call is stored twice and must count once", len(sw.ToolCalls))
	}
	if got, want := sw.ToolCalls[0].Tool, "read"; got != want {
		t.Errorf("tool = %q, want read", got)
	}
	if got, want := sw.ToolCalls[0].Seconds, 0.4; got != want {
		t.Errorf("tool seconds = %v, want %v", got, want)
	}
}

// TestOpenCodeV2Resume covers the cursor against the 2.x tables, since that is
// where all new sessions now live: a rescan must find nothing, and a message
// appended afterwards must come back exactly once.
func TestOpenCodeV2Resume(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	pricer := testPricer(t)
	p := NewOpenCodeParser(path)

	_, state, err := p.Parse("ses_v2test000000001", model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := p.Parse("ses_v2test000000001", state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Calls) != 0 {
		t.Errorf("an unchanged session returned %d calls, want 0", len(again.Calls))
	}
	if len(again.ToolCalls) != 0 {
		t.Errorf("an unchanged session returned %d tool calls, want 0", len(again.ToolCalls))
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const added = 1779976600000
	if _, err := db.Exec(
		`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		 VALUES (?,?,?,?,?,?,?)`,
		"msg_v2appended0001", "ses_v2test000000001", "assistant", 3, added, added,
		`{"model":{"id":"claude-sonnet-4-6"},"tokens":{"input":11,"output":12,"reasoning":1,`+
			`"cache":{"read":0,"write":0}},"time":{"created":1779976600000,"completed":1779976600100},`+
			`"content":[{"type":"tool","id":"call_v2_0003","name":"read","state":{"status":"completed"},`+
			`"time":{"created":1779976600000,"completed":1779976600200}}]}`); err != nil {
		t.Fatal(err)
	}

	increment, next, err := p.Parse("ses_v2test000000001", state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, increment)
	if len(increment.Calls) != 1 {
		t.Fatalf("the increment returned %d calls, want 1", len(increment.Calls))
	}
	if got, want := increment.Calls[0].CallKey, "msg:msg_v2appended0001"; got != want {
		t.Errorf("call key = %q, want %q", got, want)
	}
	if got, want := increment.ToolCalls[0].Tool, "read"; got != want {
		t.Errorf("the increment's tool = %q, want read", got)
	}
	if next.Offset <= state.Offset {
		t.Errorf("cursor did not advance: %d then %d", state.Offset, next.Offset)
	}

	// The increment's tool key is derived from the message and the call id, not
	// from its position in the batch, so the next increment cannot overwrite it.
	// A positional key restarted at zero on every pass and silently replaced the
	// first tool row of the session with each new one.
	for _, key := range []string{"inline:msg_v2appended0001:call_v2_0003", "inline:msg_v2asst00000001:call_v2_0001"} {
		found := false
		for _, tc := range append(increment.ToolCalls, sw2ToolCalls(t, path, "ses_v2test000000001")...) {
			if tc.CallKey == key {
				found = true
			}
		}
		if !found {
			t.Errorf("no tool call keyed %q; a positional key would have collided here", key)
		}
	}
}

// sw2ToolCalls re-reads a session in full so the first pass's tool keys can be
// compared with the increment's.
func sw2ToolCalls(t *testing.T, path, sessionID string) []model.ToolCall {
	t.Helper()
	sw, _ := parseOne(t, path, sessionID)
	return sw.ToolCalls
}

// TestOpenCodeV2Fallbacks covers the three places the 2.x reader has to fall back
// to something else, and each of them is a real shape rather than a hypothetical
// one:
//
//   - the `type` column is empty and the role has to come out of the blob, the
//     way 1.x recorded it;
//   - two messages created in the same millisecond, ordered by `seq` rather than
//     left to whichever order the rows came back in;
//   - an inline tool call with no id of its own, keyed by its position so a later
//     pass cannot mint a second row for it.
func TestOpenCodeV2Fallbacks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT,
		                          title TEXT, agent TEXT, model TEXT,
		                          time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT,
		                              seq INTEGER, time_created INTEGER,
		                              time_updated INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO session_v2 VALUES ('ses_fb00000000001','','/p','t','build','',1,2)`); err != nil {
		t.Fatal(err)
	}
	// Two assistant messages, deliberately out of insertion order and sharing a
	// creation time, so only `seq` can put them in the right order. The second
	// one has an empty type column and names its role in the blob.
	for _, row := range []struct {
		id, typ, blob string
		seq           int64
	}{
		{"msg_fb00000000002", "assistant",
			`{"model":{"id":"claude-sonnet-4-6"},"tokens":{"input":2,"output":2},` +
				`"time":{"created":100,"completed":100}}`, 7},
		{"msg_fb00000000001", "",
			`{"role":"assistant","modelID":"claude-sonnet-4-6",` +
				`"tokens":{"input":1,"output":1},"time":{"created":100,"completed":100},` +
				`"content":[{"type":"tool","name":"grep","state":{"status":"completed"},` +
				`"time":{"created":100,"completed":100}}]}`, 3},
	} {
		if _, err := db.Exec(
			`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
			 VALUES (?,?,?,?,?,?,?)`,
			row.id, "ses_fb00000000001", row.typ, row.seq, 100, 100, row.blob); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	sw, _ := parseOne(t, path, "ses_fb00000000001")
	checkInvariants(t, sw)

	// The message whose type column is empty still becomes a call: 1.x recorded
	// the role in the blob, and both spellings are in a migrated database.
	if len(sw.Calls) != 2 {
		t.Fatalf("parsed %d calls, want 2", len(sw.Calls))
	}
	// Order is by seq, not by insertion: seq 3 is the tool-bearing message and
	// seq 7 the one without.
	if got, want := sw.Calls[0].CallKey, "msg:msg_fb00000000001"; got != want {
		t.Errorf("first call key = %q, want %q; messages sharing a creation time are "+
			"ordered by seq, not by the order the rows came back in", got, want)
	}
	// An inline tool with no id still gets a stable key, so the next pass updates
	// its row instead of adding another.
	if len(sw.ToolCalls) != 1 {
		t.Fatalf("recorded %d tool calls, want 1", len(sw.ToolCalls))
	}
	if got, want := sw.ToolCalls[0].CallKey, "inline:msg_fb00000000001:0"; got != want {
		t.Errorf("tool call key = %q, want %q", got, want)
	}
	if got, want := sw.ToolCalls[0].Tool, "grep"; got != want {
		t.Errorf("tool = %q, want %q", got, want)
	}
}

// TestOpenCodeSessionIDsCoverBothShapes pins the enumeration a scan pass drives
// itself from. A session missing from this list is a session the dashboard never
// shows, whatever the parser can do, and the omission is invisible: the pass
// reports the sessions it did find.
func TestOpenCodeSessionIDsCoverBothShapes(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	db, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ids, err := OpenCodeSessionIDs(db)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ses_v2test000000001",  // 2.x only
		"ses_v2test000000002",  // 2.x only
		"ses_migratedtest0001", // both, and so listed once
	}
	if len(ids) != len(want) {
		t.Fatalf("OpenCodeSessionIDs = %v, want %d ids %v", ids, len(want), want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Errorf("id %d = %q, want %q (order is by creation time)", i, ids[i], id)
		}
	}
	// A child session's messages belong to its parent's conversation. Listing
	// one as a session of its own ingests the same calls twice.
	for _, id := range ids {
		if id == "ses_v2child00000001" {
			t.Error("a child session was returned as a session of its own")
		}
	}
}

// TestOpenCodeV2DatabaseWithoutOldTables: a database that has finished the
// migration — or been vacuumed after it — need not keep the 1.x tables at all.
// Reading whichever tables are present is what lets it be read at all.
func TestOpenCodeV2DatabaseWithoutOldTables(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE session`,
		`DROP TABLE message`,
		// The part table goes too: a 2.x-only database has no parts, and a
		// parser that insisted on one would fail on exactly the install that
		// needs it most.
		`DROP TABLE part`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	sw, state := parseOne(t, path, "ses_v2test000000001")
	checkInvariants(t, sw)
	if len(sw.Calls) != 1 {
		t.Fatalf("a 2.x-only database produced %d calls, want 1", len(sw.Calls))
	}
	if len(sw.ToolCalls) != 1 {
		t.Errorf("a 2.x-only database produced %d tool calls, want 1", len(sw.ToolCalls))
	}
	if state.Offset <= 0 {
		t.Error("no cursor recorded from a 2.x-only database")
	}
}

// TestOpenCodeUnreadableV2Database is the loud-failure half for the new tables. A
// `session_v2` that is missing the columns the parser reads is the shape a
// half-applied migration leaves, and it must not come back as a session with no
// calls in it.
func TestOpenCodeUnreadableV2Database(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// A renamed column: the table is there, the field the parser reads is not.
	if _, err := db.Exec(`ALTER TABLE session_v2 RENAME COLUMN directory TO cwd`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, state, err := NewOpenCodeParser(path).Parse("ses_v2test000000001", model.ScanState{}, testPricer(t))
	if err == nil {
		t.Fatal("a session_v2 missing its directory column parsed without error")
	}
	if state.SessionUID != "ses_v2test000000001" || state.Cursor != model.CursorTimeMax {
		t.Errorf("cursor after the failure = %q/%q, want the session retried from the start",
			state.SessionUID, state.Cursor)
	}
	// The batched path must fail the same way rather than skipping the session.
	if _, _, err := NewOpenCodeParser(path).
		ParseAll([]string{"ses_v2test000000001"}, nil, testPricer(t)); err == nil {
		t.Error("ParseAll accepted a database whose session_v2 it cannot read")
	}
}

// TestOpenCodeDatabaseWithNoSessionTable: a database that has neither shape is
// not a database with no sessions, and saying so is the difference between a
// misconfiguration and a quiet zero.
func TestOpenCodeDatabaseWithNoSessionTable(t *testing.T) {
	path := buildOpenCodeV2DB(t)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE session`, `DROP TABLE session_v2`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	_, _, err = NewOpenCodeParser(path).Parse("ses_v2test000000001", model.ScanState{}, testPricer(t))
	if err == nil {
		t.Fatal("a database with no session table parsed without error")
	}
	if !strings.Contains(err.Error(), "session_v2") || !strings.Contains(err.Error(), "session") {
		t.Errorf("error %q does not name the tables it looked for", err)
	}
}
