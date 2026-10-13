package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// The scanner is wired up in run(), so the only way to know which OpenCode
// database it will actually open is to run a pass and look at what came out of
// it. That matters because -opencode used to be parsed and then discarded: the
// scanner was built from the location under -home instead, so a user who
// pointed the flag at a database elsewhere — XDG_DATA_HOME, a second install, a
// colleague's copy — was shown an empty OpenCode panel and no indication that the
// file they had named was never opened.

// TestRunScansTheOpenCodeDatabaseTheFlagNames runs a real pass with the flag
// pointing somewhere unusual, and requires the sessions to arrive.
func TestRunScansTheOpenCodeDatabaseTheFlagNames(t *testing.T) {
	dbPath := openCodeV2DB(t)
	home := t.TempDir() // no agent logs under here at all

	var stdout bytes.Buffer
	o := options{
		db:       filepath.Join(home, "dashboard.db"),
		home:     home,
		opencode: dbPath,
		// The embedded fallback table is enough to price the fixture; a missing
		// models.json must not be what makes this test pass or fail.
		models: "",
	}
	if err := run(o, "scan", &stdout); err != nil {
		t.Fatalf("scan: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "calls      1") {
		t.Errorf("stats after scanning the database named by -opencode:\n%s\nwant one "+
			"call; the flag was ignored and the default location scanned instead", out)
	}
	if !strings.Contains(out, "sessions   1") {
		t.Errorf("stats after scanning the database named by -opencode:\n%s\nwant one session", out)
	}
	// The status line names the source, so a miss is attributable. A pass that
	// found nothing would otherwise be indistinguishable from one that found an
	// empty database.
	if !strings.Contains(out, "opencode") {
		t.Errorf("stats do not report an opencode row:\n%s", out)
	}
}

// TestRunScansTheDefaultOpenCodeLocationWithoutTheFlag is the other half: with
// no flag, the database under -home is the one that must be read. Pinning only
// the flag would allow the fix to be "always use the flag", which reports the
// same numbers for a user who set none.
func TestRunScansTheDefaultOpenCodeLocationWithoutTheFlag(t *testing.T) {
	home := t.TempDir()
	dbDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dbDir, "opencode.db")
	copyFile(t, openCodeV2DB(t), want)

	var stdout bytes.Buffer
	o := options{
		db:     filepath.Join(home, "dashboard.db"),
		home:   home,
		models: "",
	}
	if err := run(o, "scan", &stdout); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if out := stdout.String(); !strings.Contains(out, "sessions   1") {
		t.Errorf("stats after scanning %s with no -opencode flag:\n%s\nwant one session",
			want, out)
	}
}

// openCodeV2DB writes a minimal OpenCode 2 database — the tables the scanner and
// parser read, one session, one assistant message with usage — and returns its
// path.
//
// It is built here rather than imported from internal/source because that
// package's fixtures are test files in another package, and what this needs is
// the shape of the database, not the parser's sanitised transcript.
func openCodeV2DB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE session_v2 (
			id TEXT PRIMARY KEY, parent_id TEXT, directory TEXT, title TEXT, agent TEXT,
			model TEXT, time_created INTEGER, time_updated INTEGER)`,
		`CREATE TABLE session_message (
			id TEXT PRIMARY KEY, session_id TEXT, type TEXT, seq INTEGER,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create %s: %v", stmt, err)
		}
	}
	const created = 1779976545000
	if _, err := db.Exec(
		`INSERT INTO session_v2 (id, parent_id, directory, title, agent, model,
		                         time_created, time_updated)
		 VALUES (?,?,?,?,?,?,?,?)`,
		"ses_flag0000000001", "", "/home/testuser/project", "a session", "opencode",
		"claude-sonnet-4-6", created, created); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		 VALUES (?,?,?,?,?,?,?)`,
		"msg_flag0000000001", "ses_flag0000000001", "assistant", 1, created, created,
		`{"model":{"id":"claude-sonnet-4-6","providerID":"anthropic"},`+
			`"tokens":{"input":1000,"output":200,"reasoning":50,"cache":{"read":3000,"write":0}},`+
			`"time":{"created":1779976545000,"completed":1779976545900}}`); err != nil {
		t.Fatal(err)
	}
	return path
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
