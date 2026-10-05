package source

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// OpenCode writes a message and its parts as separate rows and updates them
// independently, so a part's time_updated can be newer than its message's while
// the message row itself is not touched again. A tool result landing on an
// assistant message does exactly this.
//
// Messages and parts are gated on separate cursors, but a part is only ever
// rendered by walking the messages of the batch. So a part newer than the message
// cursor was read, discarded, and — because the cursor advanced past it — never
// read again: the tool call was lost permanently, not merely late.
//
// The reviewer's reproduction, reproduced exactly: insert a tool part with a
// newer time_updated and do not touch its message row, then rescan.

type orphanPartDB struct {
	path      string
	sessionID string
	msgID     string
	msgTime   int64
}

func newOrphanPartDB(t *testing.T, msgTime int64) orphanPartDB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT, title TEXT,
			agent TEXT, model TEXT, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	const sid = "ses_orphan000001"
	if _, err := db.Exec(`INSERT INTO session VALUES (?,?,?,?,?,?,?)`,
		sid, "/p", "t", "opencode", `{"id":"claude-sonnet-4-6"}`, msgTime, msgTime); err != nil {
		t.Fatal(err)
	}
	// An assistant message that already billed its call on the first pass.
	if _, err := db.Exec(`INSERT INTO message VALUES (?,?,?,?,?)`,
		"msg_assistant01", sid, msgTime, msgTime,
		`{"role":"assistant","tokens":{"input":100,"output":50,"reasoning":0}}`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	return orphanPartDB{path: path, sessionID: sid, msgID: "msg_assistant01", msgTime: msgTime}
}

// addToolPart inserts a tool part whose time_updated is strictly newer than its
// message's, leaving the message row untouched.
func (o orphanPartDB) addToolPart(t *testing.T, id string, updated int64, tool string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+o.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO part VALUES (?,?,?,?,?,?)`,
		id, o.msgID, o.sessionID, updated, updated,
		`{"type":"tool","tool":"`+tool+`","state":{"status":"completed",`+
			`"time":{"start":1,"end":2}}}`); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodePartNewerThanItsMessageIsNotLost(t *testing.T) {
	const msgTime = 1779976540000
	db := newOrphanPartDB(t, msgTime)
	pricer := testPricer(t)
	p := NewOpenCodeParser(db.path)

	// First pass: the assistant message is billed, and it has no parts yet.
	first, state, err := p.Parse(db.sessionID, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Calls) != 1 {
		t.Fatalf("first pass parsed %d calls, want 1", len(first.Calls))
	}
	if len(first.ToolCalls) != 0 {
		t.Errorf("first pass found %d tool calls, want 0", len(first.ToolCalls))
	}

	// OpenCode appends the tool result. The part row is newer; the message row is
	// not touched, which is the case that loses it.
	db.addToolPart(t, "prt_orphan_00001", msgTime+5000, "Bash")

	inc, _, err := p.Parse(db.sessionID, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, inc)
	if len(inc.ToolCalls) != 1 {
		t.Fatalf("the increment found %d tool calls, want 1: the part was dropped", len(inc.ToolCalls))
	}
	tc := inc.ToolCalls[0]
	if tc.Tool != "Bash" {
		t.Errorf("tool = %q, want Bash", tc.Tool)
	}
	if tc.SessionUID != db.sessionID || tc.Agent != model.AgentOpencode {
		t.Errorf("tool call lost its session/agent: %+v", tc)
	}
	// The message is not re-billed: it was already ingested under the same key,
	// and a part arriving later is not a new request.
	if len(inc.Calls) != 0 {
		t.Errorf("the increment produced %d calls, want 0: an old message was re-billed", len(inc.Calls))
	}
}

// TestOpenCodeOrphanPartSurvivesAFurtherPass checks the part is not merely late:
// it has to survive the cursor advancing, which is where the original loss
// happened. If the cursor moved past an unrendered part, the next pass sees
// nothing and the tool call is gone for good.
func TestOpenCodeOrphanPartSurvivesAFurtherPass(t *testing.T) {
	const msgTime = 1779976540000
	db := newOrphanPartDB(t, msgTime)
	pricer := testPricer(t)
	p := NewOpenCodeParser(db.path)

	_, state, err := p.Parse(db.sessionID, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	db.addToolPart(t, "prt_orphan_00001", msgTime+5000, "Bash")

	inc, state2, err := p.Parse(db.sessionID, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.ToolCalls) != 1 {
		t.Fatalf("the increment found %d tool calls, want 1", len(inc.ToolCalls))
	}

	// Two further passes over an unchanged database must find nothing: the part
	// was rendered once, and the cursor it advanced past is legitimately past it.
	for i := 0; i < 2; i++ {
		again, next, err := p.Parse(db.sessionID, state2, pricer)
		if err != nil {
			t.Fatal(err)
		}
		if len(again.ToolCalls) != 0 || len(again.Calls) != 0 {
			t.Errorf("pass %d of an unchanged database returned %d calls and %d tools, want none",
				i, len(again.Calls), len(again.ToolCalls))
		}
		state2 = next
	}
}

// TestOpenCodePartWithAMissingMessageIsDropped is the negative half: a part
// whose message row is gone cannot be attributed to anything, and re-reading it
// forever would stall the cursor for a row that will never arrive.
func TestOpenCodePartWithAMissingMessageIsDropped(t *testing.T) {
	const msgTime = 1779976540000
	db := newOrphanPartDB(t, msgTime)
	pricer := testPricer(t)
	p := NewOpenCodeParser(db.path)

	_, state, err := p.Parse(db.sessionID, model.ScanState{}, pricer)
	if err != nil {
		t.Fatal(err)
	}
	db.addToolPart(t, "prt_orphan_00001", msgTime+5000, "Bash")
	handle, err := sql.Open("sqlite", "file:"+db.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Exec(`DELETE FROM message WHERE id = ?`, db.msgID); err != nil {
		t.Fatal(err)
	}
	handle.Close()

	inc, state2, err := p.Parse(db.sessionID, state, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.ToolCalls) != 0 {
		t.Errorf("found %d tool calls for a part with no message, want 0", len(inc.ToolCalls))
	}
	// The cursor still advances, so the next pass is not stuck re-reading it.
	next, _, err := p.Parse(db.sessionID, state2, pricer)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.ToolCalls) != 0 {
		t.Errorf("the following pass re-read %d orphan parts; the cursor did not advance", len(next.ToolCalls))
	}
}
