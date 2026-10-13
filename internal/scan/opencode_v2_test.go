package scan

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

// The scanner half of the OpenCode 2 problem.
//
// The parser reading session_v2 is necessary and not sufficient: a pass is driven
// by the list of session ids it enumerates first, so a parser that reads the new
// tables while the enumeration only knows the old ones still shows a dashboard
// with every OpenCode 2 session missing and nothing anywhere saying so. These
// tests drive whole passes against a database holding both shapes, because that
// combination is what an upgraded install actually contains and it is the only
// arrangement in which the two sets of tables can disagree.

// openCodeV2FixtureDB writes a database in the shape an upgraded OpenCode leaves
// behind: `session_v2` and `session_message` alongside the 1.x tables, with one
// session in both and one message the migration left behind in the old table.
//
// The 1.x fixture in scan_test.go covers the other arrangement — old tables
// only — and must keep working unchanged.
func openCodeV2FixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sessionCols := `(id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT,
	                 agent TEXT, model TEXT, time_created INTEGER, time_updated INTEGER)`
	for _, stmt := range []string{
		`CREATE TABLE session ` + sessionCols,
		`CREATE TABLE session_v2 ` + sessionCols,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT)`,
		`CREATE TABLE session_message (
			id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INTEGER,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create %s: %v", stmt, err)
		}
	}

	insertSession := func(table, id, parent, directory string, created int64) {
		var parentArg any
		if parent != "" {
			parentArg = parent
		}
		if _, err := db.Exec(
			`INSERT INTO `+table+` (id, parent_id, directory, title, agent, model,
			                        time_created, time_updated)
			 VALUES (?,?,?,?,?,?,?,?)`,
			id, parentArg, directory, "a session", "opencode",
			"claude-sonnet-4-6", created, created); err != nil {
			t.Fatalf("insert into %s: %v", table, err)
		}
	}
	// The 2.x-only session gets its own directory so the per-tool rollup can be
	// narrowed to it without a new query on the store.
	insertSession("session_v2", "ses_v2only00000001", "", "/home/testuser/v2-only", 1779976545000)
	insertSession("session_v2", "ses_migrated000001", "", "/home/testuser/project", 1779976645000)
	insertSession("session", "ses_migrated000001", "", "/home/testuser/project", 1779976645000)
	insertSession("session_v2", "ses_v2child00000001", "ses_v2only00000001",
		"/home/testuser/project", 1779976745000)

	// A 2.x assistant message: the role is a column, the model an object, and
	// the tool call lives in the content array.
	if _, err := db.Exec(
		`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		 VALUES (?,?,?,?,?,?,?)`,
		"msg_v2asst00000001", "ses_v2only00000001", "assistant", 1, 1779976545000, 1779976545000,
		`{"model":{"id":"claude-sonnet-4-6","providerID":"anthropic"},`+
			`"tokens":{"input":100,"output":50,"reasoning":10,"cache":{"read":200,"write":0}},`+
			`"time":{"created":1779976545000,"completed":1779976545100},`+
			`"content":[{"type":"tool","id":"call_v2_0001","name":"bash",`+
			`"state":{"status":"completed"},"time":{"created":1779976545050,"completed":1779976545080}}]}`); err != nil {
		t.Fatal(err)
	}

	// The migrated session's message is in both message tables, and its tool call
	// is in both the content array and the part table.
	migrated := `{"role":"assistant","modelID":"claude-sonnet-4-6",` +
		`"tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":0,"write":0}},` +
		`"time":{"created":1779976645000,"completed":1779976645100}}`
	if _, err := db.Exec(
		`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		 VALUES (?,?,?,?,?,?,?)`,
		"msg_migasst00000001", "ses_migrated000001", "assistant", 1,
		1779976645000, 1779976645000, migrated); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`,
		"msg_migasst00000001", "ses_migrated000001", 1779976645000, 1779976645000, migrated); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
		 VALUES (?,?,?,?,?,?)`,
		"prt_mig0001", "msg_migasst00000001", "ses_migrated000001", 1779976645050, 1779976645080,
		`{"type":"tool","callID":"call_v2_0001","tool":"bash",`+
			`"state":{"status":"completed","time":{"start":1779976645050,"end":1779976645080}}}`); err != nil {
		t.Fatal(err)
	}
	// And one message the migration never copied.
	if _, err := db.Exec(
		`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`,
		"msg_migonly0000001", "ses_migrated000001", 1779976646000, 1779976646100,
		`{"role":"assistant","modelID":"claude-sonnet-4-6",`+
			`"tokens":{"input":7,"output":3,"reasoning":0,"cache":{"read":0,"write":0}},`+
			`"time":{"created":1779976646000,"completed":1779976646100}}`); err != nil {
		t.Fatal(err)
	}

	// The child session's messages belong to its parent's transcript.
	if _, err := db.Exec(
		`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		 VALUES (?,?,?,?,?,?,?)`,
		"msg_child00000001", "ses_v2child00000001", "assistant", 1, 1779976745000, 1779976745000,
		`{"model":{"id":"claude-sonnet-4-6"},`+
			`"tokens":{"input":999,"output":999,"reasoning":0,"cache":{"read":0,"write":0}}}`); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestScanOpenCodeIngestsBothStorageShapes is the test that would have caught the
// original defect: a session that exists only in session_v2 has to reach the
// store, and a session in both tables has to reach it once.
func TestScanOpenCodeIngestsBothStorageShapes(t *testing.T) {
	path := openCodeV2FixtureDB(t)
	st := newTestStore(t)
	sc := buildOpenCodeScanner(t, st, path)
	ctx := context.Background()

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	totals, err := st.Totals(ctx, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}

	// Two sessions: the 2.x-only one and the migrated one. The child session is
	// not one of them, and neither is the 2.x table's absence of a `session`
	// counterpart for the first.
	if totals.Sessions != 2 {
		t.Errorf("stored %d sessions, want 2 (one 2.x-only, one migrated); OpenCode 2 "+
			"sessions were invisible while this passed", totals.Sessions)
	}
	// Three calls: the 2.x message, the migrated message present in both tables
	// (one call, not two), and the message left behind in the old table.
	if totals.Calls != 3 {
		t.Errorf("stored %d calls, want 3; a message in both message tables must be "+
			"counted once and a message only in the old table must not be dropped", totals.Calls)
	}
	// The 2.x message's own model, read out of the object 2.x writes.
	v2Calls, err := st.SessionCalls(ctx, "ses_v2only00000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(v2Calls) != 1 {
		t.Fatalf("the 2.x-only session holds %d calls, want 1", len(v2Calls))
	}
	if got, want := v2Calls[0].Model, "claude-sonnet-4-6"; got != want {
		t.Errorf("model = %q, want %q; 2.x writes the model as an object and reading "+
			"it as a flat id prices every 2.x call as unknown", got, want)
	}
	if got, want := v2Calls[0].CacheReadTokens, 200; got != int64(want) {
		t.Errorf("cache read = %d, want %d", got, want)
	}

	// The child session is a real session with a real message in it, filed under
	// the same project as the migrated session. Ingesting it would put the
	// subagent's 999 input tokens and its call in the dashboard a second time.
	projectTotals, err := st.Totals(ctx, store.Filter{Projects: []string{"/home/testuser/project"}})
	if err != nil {
		t.Fatal(err)
	}
	if projectTotals.Calls != 2 || projectTotals.InputTokens != 17 {
		t.Errorf("the shared project holds %d calls / %d input tokens, want 2 / 17: "+
			"the child session's message belongs to its parent's transcript",
			projectTotals.Calls, projectTotals.InputTokens)
	}
}

// TestScanOpenCodeV2PassIsIncremental is the same property for the 2.x tables: a
// second pass reads nothing, and a message appended afterwards extends its
// session rather than replacing it.
//
// The append half is where the cursor matters. A session read incrementally
// returns only the new rows, so committing that as a replacement would delete
// every call already stored for the session.
func TestScanOpenCodeV2PassIsIncremental(t *testing.T) {
	path := openCodeV2FixtureDB(t)
	st := newTestStore(t)
	sc := buildOpenCodeScanner(t, st, path)
	ctx := context.Background()

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := st.Totals(ctx, store.Filter{})

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := st.Totals(ctx, store.Filter{})
	if second != first {
		t.Errorf("an unchanged pass changed the totals: %+v then %+v", first, second)
	}
	statuses, _ := st.ScanStatuses()
	for _, s := range statuses {
		if s.Agent == model.AgentOpencode && s.FilesChanged != 0 {
			t.Errorf("the second pass re-ingested %d OpenCode sessions; the time "+
				"cursor did not hold against the 2.x tables", s.FilesChanged)
		}
	}

	// A new 2.x message on an existing session.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	const added = 1779977000000
	if _, err := db.Exec(
		`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		 VALUES (?,?,?,?,?,?,?)`,
		"msg_v2appended0001", "ses_v2only00000001", "assistant", 2, added, added,
		`{"model":{"id":"claude-sonnet-4-6"},`+
			`"tokens":{"input":13,"output":4,"reasoning":0,"cache":{"read":0,"write":0}},`+
			`"time":{"created":1779977000000,"completed":1779977000100},`+
			`"content":[{"type":"tool","id":"call_v2_0002","name":"read","state":{"status":"completed"},`+
			`"time":{"created":1779977000000,"completed":1779977000050}}]}`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	third, _ := st.Totals(ctx, store.Filter{})
	if third.Calls != first.Calls+1 {
		t.Errorf("after appending a 2.x message there are %d calls, want %d",
			third.Calls, first.Calls+1)
	}
	grown, err := st.SessionCalls(ctx, "ses_v2only00000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(grown) != 2 {
		t.Errorf("the grown 2.x session holds %d calls, want 2: an incremental read "+
			"committed as a replacement deletes the calls it did not re-read", len(grown))
	}
	// The session that did not move is untouched.
	other, err := st.SessionCalls(ctx, "ses_migrated000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 2 {
		t.Errorf("the migrated session holds %d calls, want 2: the cursor is per "+
			"session, so an append to one must not disturb another", len(other))
	}

	// And the tool rows survive an incremental pass. They used to be keyed by
	// their position in the batch, which restarts at zero on every pass, so each
	// increment overwrote the first tool call the session had — leaving a
	// session that was still running reporting one tool call where it had made
	// several.
	tools, err := st.Tools(ctx, store.Filter{Projects: []string{"/home/testuser/v2-only"}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	for _, tool := range tools {
		calls += tool.Calls
	}
	if calls != 2 {
		t.Errorf("the grown 2.x session has %d tool calls, want 2: a tool row keyed "+
			"by its position in the batch is overwritten by the next pass (%+v)",
			calls, tools)
	}
}
