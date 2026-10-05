// Package store owns the SQLite database: its schema, the idempotent writes
// that ingestion performs, and every read the web layer needs.
//
// Two decisions shape this package.
//
// First, the unit of storage is one row per LLM call, not a per-session
// summary. A summary cannot answer "what did I spend last March" once the
// session log that recorded March has been rotated away; individual calls can,
// which is the whole point of persisting them.
//
// Second, the database runs in WAL mode. That gives readers a consistent
// snapshot while a scan is writing, so the website is never blocked by — or
// blocked from — an in-progress scan. That is what decouples the two halves of
// the process.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
	_ "modernc.org/sqlite"
)

// Store is a handle on the database.
type Store struct {
	db *sql.DB
	// Path is retained for diagnostics; empty means an in-memory database.
	Path string
}

// Open opens (creating if needed) the database at path and applies the schema.
//
// Pragmas are set per connection rather than once: SQLite's journal_mode is
// persistent in the file, but busy_timeout and foreign_keys are not.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		// A shared cache keeps an in-memory database alive across the pool's
		// connections, which the default of one private database would not.
		dsn = "file::memory:?cache=shared"
	} else {
		// SQLite will not create intermediate directories, so the first run
		// against the default path under ~/.local/share would fail with an
		// opaque "unable to open database file".
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create %s: %w", dir, err)
			}
		}
		dsn = path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One writer at a time. SQLite serialises writes anyway; capping the pool
	// turns lock contention into a queue instead of SQLITE_BUSY errors, and the
	// scan is the only writer.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, Path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the handle for callers that need a transaction.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// schema is the shape a fresh database is created in, and is applied on every
// open. It is only ever the *starting* shape, not the whole story: every
// statement in it is `IF NOT EXISTS`, so on a database that already exists it
// is a no-op. An existing database is brought forward by the migrations below,
// which is the only mechanism that can actually change a table that is already
// there — `CREATE TABLE IF NOT EXISTS call (...)` with a new column in it does
// nothing at all to a table called `call` that already exists.
const schema = `
CREATE TABLE IF NOT EXISTS call (
	session_uid       TEXT    NOT NULL,
	call_key          TEXT    NOT NULL,
	agent             TEXT    NOT NULL,
	project           TEXT    NOT NULL DEFAULT '',
	model             TEXT    NOT NULL DEFAULT '',
	ts                INTEGER NOT NULL DEFAULT 0,   -- epoch seconds, 0 when unknown
	day               TEXT    NOT NULL DEFAULT '',
	input_tokens      INTEGER NOT NULL DEFAULT 0,
	output_tokens     INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
	total_tokens      INTEGER NOT NULL DEFAULT 0,
	llm_seconds       REAL    NOT NULL DEFAULT 0,
	cost_usd          REAL    NOT NULL DEFAULT 0,
	priced            INTEGER NOT NULL DEFAULT 1,
	PRIMARY KEY (session_uid, call_key)
) WITHOUT ROWID;

-- ts drives every time-windowed query; day drives the cheap calendar grouping.
CREATE INDEX IF NOT EXISTS call_ts       ON call(ts);
CREATE INDEX IF NOT EXISTS call_day      ON call(day);
CREATE INDEX IF NOT EXISTS call_project  ON call(project);
CREATE INDEX IF NOT EXISTS call_model    ON call(model);
CREATE INDEX IF NOT EXISTS call_agent    ON call(agent);
CREATE INDEX IF NOT EXISTS call_session  ON call(session_uid);

CREATE TABLE IF NOT EXISTS tool_call (
	session_uid TEXT    NOT NULL,
	call_key    TEXT    NOT NULL,
	agent       TEXT    NOT NULL,
	project     TEXT    NOT NULL DEFAULT '',
	tool        TEXT    NOT NULL,
	ts          INTEGER NOT NULL DEFAULT 0,
	seconds     REAL    NOT NULL DEFAULT 0,
	is_error    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (session_uid, call_key)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS tool_ts     ON tool_call(ts);
CREATE INDEX IF NOT EXISTS tool_name   ON tool_call(tool);
-- Backs the "which request was this tool issued in" lookup.
CREATE INDEX IF NOT EXISTS tool_session_ts ON tool_call(session_uid, ts);

-- Session rows are derived from call/tool_call and rebuilt whenever a session's
-- log is (re)read, so they can never drift from the rows they summarise.
CREATE TABLE IF NOT EXISTS session (
	uid               TEXT PRIMARY KEY,
	agent             TEXT NOT NULL,
	project           TEXT NOT NULL DEFAULT '',
	path              TEXT NOT NULL DEFAULT '',
	title             TEXT NOT NULL DEFAULT '',
	first_ts          INTEGER NOT NULL DEFAULT 0,
	last_ts           INTEGER NOT NULL DEFAULT 0,
	wall_seconds      REAL    NOT NULL DEFAULT 0,
	calls             INTEGER NOT NULL DEFAULT 0,
	input_tokens      INTEGER NOT NULL DEFAULT 0,
	output_tokens     INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
	total_tokens      INTEGER NOT NULL DEFAULT 0,
	cost_usd          REAL    NOT NULL DEFAULT 0,
	llm_seconds       REAL    NOT NULL DEFAULT 0,
	tool_seconds      REAL    NOT NULL DEFAULT 0,
	tool_calls        INTEGER NOT NULL DEFAULT 0,
	tool_errors       INTEGER NOT NULL DEFAULT 0,
	-- orphaned is set when the session's log has disappeared. The stored rows
	-- stay: they are the historical record, and a window covering this session
	-- must still answer correctly after the log is rotated away. What is lost
	-- is the transcript, so the page stops offering to open it.
	orphaned          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS session_last    ON session(last_ts);
CREATE INDEX IF NOT EXISTS session_project ON session(project);

-- Incremental scan bookkeeping. offset is the byte position just past the last
-- complete line consumed, so a growing log is resumed rather than re-read.
CREATE TABLE IF NOT EXISTS scan_state (
	path        TEXT PRIMARY KEY,
	agent       TEXT NOT NULL,
	size        INTEGER NOT NULL DEFAULT 0,
	mtime_ns    INTEGER NOT NULL DEFAULT 0,
	offset      INTEGER NOT NULL DEFAULT 0,
	prefix_hash TEXT    NOT NULL DEFAULT '',
	prefix_len  INTEGER NOT NULL DEFAULT 0,
	cursor      TEXT    NOT NULL DEFAULT 'bytes',
	session_uid TEXT NOT NULL DEFAULT '',
	complete    INTEGER NOT NULL DEFAULT 0,
	scanned_at  INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- Facts about the scan itself, surfaced on the page so a stale dashboard is
-- explainable rather than mysterious.
CREATE TABLE IF NOT EXISTS scan_status (
	agent          TEXT PRIMARY KEY,
	last_scan_at   INTEGER NOT NULL DEFAULT 0,
	last_full_at   INTEGER NOT NULL DEFAULT 0,
	files_seen     INTEGER NOT NULL DEFAULT 0,
	files_changed  INTEGER NOT NULL DEFAULT 0,
	calls_ingested INTEGER NOT NULL DEFAULT 0,
	error          TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
`

// ---------------------------------------------------------------- migrations

// targetVersion is the schema version this build of the code produces: the
// version reached by the last entry in migrations.
//
// It is derived from the list rather than kept as a constant of its own, because
// a constant beside the list is a second thing to forget to update — and the
// failure mode of forgetting is a build that silently refuses to open a
// database it could have migrated.
func targetVersion() int {
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// migration is one ordered step from (version-1) to version.
//
// apply runs inside a transaction together with the `user_version` write, so a
// step either lands completely or not at all. A nil apply is a deliberate no-op
// and is how the version 0 -> 1 step is expressed.
type migration struct {
	version int
	name    string
	apply   func(tx *sql.Tx) error
}

// migrations is the ordered list of schema steps, and the only place a schema
// change may be recorded.
//
// To change the schema, both of these:
//
//  1. edit the shape in `schema` above (adding a new const for the new DDL,
//     concatenated into `schema` the way v2RollupIndexes is), so that a fresh
//     database is created directly in the new shape;
//  2. append an entry below that brings an existing database from the current
//     version to the new one — using addColumn for a new column, because
//     `CREATE TABLE IF NOT EXISTS` cannot add a column to a table that already
//     exists, which is the entire reason this list exists.
//
// There is no version constant to bump: the target version is whatever the last
// entry says (see targetVersion), and TestFreshAndMigratedSchemasAreIdentical
// fails if the two halves of a change disagree.
//
// The list is append-only. Never edit, reorder or delete an entry that has
// shipped: databases out in the field have already run it, and the code that
// reads them assumes the result. A mistake in a released step is fixed by
// appending another step.
//
// Every step must be safe to run against a database that has already seen it
// (`IF NOT EXISTS`, or a column-existence guard via addColumn). It costs
// nothing — the version check in migrate skips steps that have already been
// applied — and it means a database left at an intermediate version by an
// interrupted run can simply be opened again.
var migrations = []migration{
	{
		// 0 -> 1: no-op, on purpose.
		//
		// Version 0 means "a database created before this list existed", i.e.
		// one written by a build whose only schema step was `Exec(schema)`.
		// Such a database already has the entire v1 shape — every table and
		// every index that predates v2 — and its user_version is 0 only because
		// nothing ever wrote it. Re-asserting v1 here would be harmless, but
		// asserting it by *running DDL* is not: a statement that is only safe
		// against a fresh database (adding a column, say) would corrupt a v0
		// database that is already at the v1 shape. So this step claims the
		// version and changes nothing at all. The invariant it relies on —
		// "a v0 database is already a v1 database" — is asserted by
		// TestMigrationFromV0Database.
		version: 1,
		name:    "baseline schema (claimed, not applied)",
	},
}

// execScript turns a multi-statement script into a migration step.
func execScript(script string) func(*sql.Tx) error {
	return func(tx *sql.Tx) error {
		_, err := tx.Exec(script)
		return err
	}
}

// addColumn adds a column to a table unless the table already has one by that
// name.
//
// SQLite has no `ADD COLUMN IF NOT EXISTS`, and a plain ALTER TABLE that names
// an existing column fails, so the check is made here. This is the helper a
// migration adding a column to an existing table should use: it is the step
// that `CREATE TABLE IF NOT EXISTS` cannot perform.
func addColumn(tx *sql.Tx, table, column, decl string) error {
	has, err := hasColumn(tx, table, column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	// table and column come from the migration list, never from user input, so
	// there is nothing to bind and nothing to quote.
	_, err = tx.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl))
	return err
}

// hasColumn reports whether a table already has a column.
func hasColumn(tx *sql.Tx, table, column string) (bool, error) {
	rows, err := tx.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// schemaVersion reads the database's recorded schema version.
func (s *Store) schemaVersion() (int, error) {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

func (s *Store) migrate() error {
	version, err := s.schemaVersion()
	if err != nil {
		return err
	}
	target := targetVersion()
	if version > target {
		// Refuse rather than guess. Writing to a newer schema with older code
		// is how a database gets quietly mangled, and the only safe move is to
		// tell the user to upgrade.
		return fmt.Errorf("database schema version %d is newer than this build understands (%d): upgrade dashd",
			version, target)
	}
	// The list is walked in order below, so a list that is not ordered would
	// silently skip a step. Cheap to check, and the mistake is invisible when it
	// happens.
	prev := 0
	for _, m := range migrations {
		if m.version <= prev {
			return fmt.Errorf("migration list is not in increasing version order: version %d follows %d", m.version, prev)
		}
		prev = m.version
	}
	// A fresh database gets the whole shape in one pass, and an existing one
	// gets a no-op from every statement in it.
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	for _, m := range migrations {
		if m.version <= version {
			continue
		}
		if err := s.applyMigration(m); err != nil {
			return err
		}
	}
	return nil
}

// applyMigration runs one step and records the version it reached.
//
// The step and the `user_version` write share a transaction, so a step that
// fails part-way leaves the database at the version it started from rather than
// claiming a version it did not reach. `PRAGMA user_version` takes no bind
// parameter, hence the formatted statement; the value is a constant from the
// migration list, never anything a caller supplied.
func (s *Store) applyMigration(m migration) error {
	err := s.InTx(func(tx *sql.Tx) error {
		if m.apply != nil {
			if err := m.apply(tx); err != nil {
				return err
			}
		}
		_, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version))
		return err
	})
	if err != nil {
		return fmt.Errorf("migrate to schema version %d (%s): %w", m.version, m.name, err)
	}
	return nil
}

// InTx runs fn inside a transaction, rolling back on error.
//
// Ingestion writes many rows per file; committing them together means a failed
// parse leaves no half-ingested session behind.
func (s *Store) InTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- ingestion

// ReplaceSession rewrites everything known about one session from scratch.
//
// Used when a log is re-read in full, which happens on first sight and whenever
// the file was rewritten or truncated rather than appended to. Deleting first
// makes the stored state match exactly what the file now says; an append-only
// merge cannot make that guarantee, because a log that shrank means calls we
// already recorded no longer exist.
func (s *Store) ReplaceSession(sess model.SessionWrite) error {
	return s.InTx(func(tx *sql.Tx) error {
		if err := clearSession(tx, sess.UID); err != nil {
			return err
		}
		if err := insertSessionRows(tx, sess); err != nil {
			return err
		}
		// The identity of the session is written here and its aggregates are
		// filled in by RecomputeSession. It has to exist before that call:
		// an UPDATE against a row that was never inserted silently does
		// nothing, and the session then vanishes from every table.
		_, err := tx.Exec(`
			INSERT INTO session (uid, agent, project, path, title) VALUES (?,?,?,?,?)`,
			sess.UID, sess.Agent, sess.Project, sess.Path, sess.Title)
		return err
	})
}

// AppendSession adds newly-seen calls to a session that is already stored,
// leaving what is there untouched.
//
// This is the incremental path. Its correctness rests on the scanner having
// decided the file only grew: the prefix fingerprint and the size check together
// establish that, and AppendSession is not called otherwise. Rows are still
// written with an upsert so that re-reading a region is harmless rather than
// duplicating.
func (s *Store) AppendSession(sess model.SessionWrite) error {
	return s.InTx(func(tx *sql.Tx) error {
		if err := insertSessionRows(tx, sess); err != nil {
			return err
		}
		// Make sure a session row exists even if the very first scan of this
		// file produced no calls at all.
		_, err := tx.Exec(
			`INSERT INTO session (uid, agent, project, path) VALUES (?,?,?,?)
			 ON CONFLICT(uid) DO UPDATE SET project = excluded.project`,
			sess.UID, sess.Agent, sess.Project, sess.Path)
		return err
	})
}

func clearSession(tx *sql.Tx, uid string) error {
	for _, q := range []string{
		"DELETE FROM call      WHERE session_uid = ?",
		"DELETE FROM tool_call WHERE session_uid = ?",
		"DELETE FROM session   WHERE uid          = ?",
	} {
		if _, err := tx.Exec(q, uid); err != nil {
			return fmt.Errorf("clear session %s: %w", uid, err)
		}
	}
	return nil
}

func insertSessionRows(tx *sql.Tx, sess model.SessionWrite) error {
	callStmt, err := tx.Prepare(`
		INSERT INTO call (session_uid, call_key, agent, project, model, ts, day,
		                  input_tokens, output_tokens, cache_read_tokens,
		                  cache_write_tokens, reasoning_tokens, total_tokens,
		                  llm_seconds, cost_usd, priced)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(session_uid, call_key) DO UPDATE SET
			model = excluded.model, ts = excluded.ts, day = excluded.day,
			input_tokens = excluded.input_tokens, output_tokens = excluded.output_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			total_tokens = excluded.total_tokens, llm_seconds = excluded.llm_seconds,
			cost_usd = excluded.cost_usd, priced = excluded.priced`)
	if err != nil {
		return err
	}
	defer callStmt.Close()

	for _, c := range sess.Calls {
		var ts int64
		if !c.Time.IsZero() {
			ts = c.Time.Unix()
		}
		day := c.Day
		if day == "" && !c.Time.IsZero() {
			day = c.Time.Local().Format("2006-01-02")
		}
		if _, err := callStmt.Exec(
			c.SessionUID, c.CallKey, c.Agent, c.Project, c.Model, ts, day,
			c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens,
			c.ReasoningTokens, c.TotalTokens, c.LLMSeconds, c.CostUSD, boolInt(c.Priced),
		); err != nil {
			return fmt.Errorf("insert call %s/%s: %w", c.SessionUID, c.CallKey, err)
		}
	}

	if len(sess.ToolCalls) > 0 {
		toolStmt, err := tx.Prepare(`
			INSERT INTO tool_call (session_uid, call_key, agent, project, tool, ts, seconds, is_error)
			VALUES (?,?,?,?,?,?,?,?)
			ON CONFLICT(session_uid, call_key) DO UPDATE SET
				tool = excluded.tool, ts = excluded.ts,
				seconds = excluded.seconds, is_error = excluded.is_error`)
		if err != nil {
			return err
		}
		defer toolStmt.Close()
		for _, t := range sess.ToolCalls {
			var ts int64
			if !t.Time.IsZero() {
				ts = t.Time.Unix()
			}
			if _, err := toolStmt.Exec(
				t.SessionUID, t.CallKey, t.Agent, t.Project, t.Tool, ts, t.Seconds, boolInt(t.IsError),
			); err != nil {
				return fmt.Errorf("insert tool call %s/%s: %w", t.SessionUID, t.CallKey, err)
			}
		}
	}
	return nil
}

// RecomputeSession rebuilds a session's summary from the rows it summarises.
//
// Done in SQL rather than in Go so the summary cannot drift from its inputs, and
// so it is correct after either ingestion path: an append leaves the old summary
// stale, and a full read replaces it.
func (s *Store) RecomputeSession(uid string) error {
	_, err := s.db.Exec(`
		UPDATE session SET
			calls             = COALESCE((SELECT COUNT(*)  FROM call      WHERE session_uid = session.uid), 0),
			input_tokens      = COALESCE((SELECT SUM(input_tokens)      FROM call WHERE session_uid = session.uid), 0),
			output_tokens     = COALESCE((SELECT SUM(output_tokens)     FROM call WHERE session_uid = session.uid), 0),
			cache_read_tokens = COALESCE((SELECT SUM(cache_read_tokens) FROM call WHERE session_uid = session.uid), 0),
			cache_write_tokens= COALESCE((SELECT SUM(cache_write_tokens)FROM call WHERE session_uid = session.uid), 0),
			reasoning_tokens  = COALESCE((SELECT SUM(reasoning_tokens)  FROM call WHERE session_uid = session.uid), 0),
			total_tokens      = COALESCE((SELECT SUM(total_tokens)      FROM call WHERE session_uid = session.uid), 0),
			cost_usd          = COALESCE((SELECT SUM(cost_usd)          FROM call WHERE session_uid = session.uid), 0),
			llm_seconds       = COALESCE((SELECT SUM(llm_seconds)       FROM call WHERE session_uid = session.uid), 0),
			tool_calls        = COALESCE((SELECT COUNT(*)  FROM tool_call WHERE session_uid = session.uid), 0),
			tool_errors       = COALESCE((SELECT SUM(is_error) FROM tool_call WHERE session_uid = session.uid), 0),
			tool_seconds      = COALESCE((SELECT SUM(seconds)    FROM tool_call WHERE session_uid = session.uid), 0),
			first_ts          = COALESCE((SELECT MIN(NULLIF(ts,0)) FROM call WHERE session_uid = session.uid), first_ts),
			last_ts           = COALESCE((SELECT MAX(ts)             FROM call WHERE session_uid = session.uid), last_ts),
			wall_seconds      = MAX(0,
			                COALESCE((SELECT MAX(ts) FROM call WHERE session_uid = session.uid), 0)
			              - COALESCE((SELECT MIN(NULLIF(ts,0)) FROM call WHERE session_uid = session.uid), 0))
		WHERE uid = ?`, uid)
	return err
}

// RecomputeAllSessions refreshes every summary. Used after a bulk operation
// such as repricing, where per-session updates would be N round trips.
func (s *Store) RecomputeAllSessions() error {
	rows, err := s.db.Query("SELECT uid FROM session")
	if err != nil {
		return err
	}
	var uids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			rows.Close()
			return err
		}
		uids = append(uids, uid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, uid := range uids {
		if err := s.RecomputeSession(uid); err != nil {
			return err
		}
	}
	return nil
}

// Summarise derives a session's summary from its calls and tool calls, for the
// full-replacement path where the summary is written in the same transaction as
// the rows.
func Summarise(sess model.SessionWrite) model.Session {
	out := model.Session{
		UID:     sess.UID,
		Agent:   sess.Agent,
		Project: sess.Project,
	}
	for _, c := range sess.Calls {
		out.Calls++
		out.InputTokens += c.InputTokens
		out.OutputTokens += c.OutputTokens
		out.CacheReadTokens += c.CacheReadTokens
		out.CacheWriteTokens += c.CacheWriteTokens
		out.ReasoningTokens += c.ReasoningTokens
		out.TotalTokens += c.TotalTokens
		out.CostUSD += c.CostUSD
		out.LLMSeconds += c.LLMSeconds
		if c.Time.IsZero() {
			continue
		}
		if out.FirstTS.IsZero() || c.Time.Before(out.FirstTS) {
			out.FirstTS = c.Time
		}
		if out.LastTS.IsZero() || c.Time.After(out.LastTS) {
			out.LastTS = c.Time
		}
	}
	if out.FirstTS.IsZero() {
		out.FirstTS = sess.FirstTS
	}
	if out.LastTS.IsZero() {
		out.LastTS = sess.LastTS
	}
	if !out.FirstTS.IsZero() && out.LastTS.After(out.FirstTS) {
		out.WallSecs = out.LastTS.Sub(out.FirstTS).Seconds()
	}
	for _, t := range sess.ToolCalls {
		out.ToolCalls++
		out.ToolSeconds += t.Seconds
		if t.IsError {
			out.ToolErrors++
		}
		if t.Time.IsZero() {
			continue
		}
		if out.FirstTS.IsZero() || t.Time.Before(out.FirstTS) {
			out.FirstTS = t.Time
		}
		if out.LastTS.IsZero() || t.Time.After(out.LastTS) {
			out.LastTS = t.Time
		}
	}
	return out
}

// MarkOrphaned flags sessions whose source file has disappeared, keeping their
// calls, tool calls and totals.
//
// Deleting them would be the intuitive choice and would defeat the point of
// storing calls individually: the moment a log is rotated, an accurate "what did
// I spend in March" would become an impossible question. So the history is kept
// and only the transcript link is withdrawn.
//
// ForgetSession is the explicit, destructive counterpart, for a caller that
// genuinely wants a session gone rather than merely unreachable.
func (s *Store) MarkOrphaned(uids []string) error {
	if len(uids) == 0 {
		return nil
	}
	return s.InTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			"UPDATE session SET orphaned = 1 WHERE uid = ?")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, uid := range uids {
			if _, err := stmt.Exec(uid); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClearOrphaned un-flags a session, called when its log reappears.
func (s *Store) ClearOrphaned(uid string) error {
	_, err := s.db.Exec("UPDATE session SET orphaned = 0 WHERE uid = ?", uid)
	return err
}

// ForgetSession removes a session and everything derived from it.
func (s *Store) ForgetSession(uid string) error {
	return s.InTx(func(tx *sql.Tx) error {
		for _, q := range []string{
			"DELETE FROM call WHERE session_uid = ?",
			"DELETE FROM tool_call WHERE session_uid = ?",
			"DELETE FROM session WHERE uid = ?",
		} {
			if _, err := tx.Exec(q, uid); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------- scan state

// LoadScanStates returns the bookkeeping for every known source file, keyed by
// path. Loading them all at once keeps the scaner's per-file decision to a map
// lookup plus a stat.
func (s *Store) LoadScanStates() (map[string]model.ScanState, error) {
	rows, err := s.db.Query(`
		SELECT path, agent, size, mtime_ns, offset, prefix_hash, prefix_len, cursor,
		       session_uid, complete, scanned_at
		FROM scan_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]model.ScanState, 256)
	for rows.Next() {
		var st model.ScanState
		var mtimeNS, scannedAt int64
		var complete int
		if err := rows.Scan(&st.Path, &st.Agent, &st.Size, &mtimeNS, &st.Offset,
			&st.PrefixHash, &st.PrefixLen, &st.Cursor, &st.SessionUID, &complete,
			&scannedAt); err != nil {
			return nil, err
		}
		st.ModTime = time.Unix(0, mtimeNS)
		st.Scanned = time.Unix(0, scannedAt)
		st.Complete = complete != 0
		out[st.Path] = st
	}
	return out, rows.Err()
}

// SaveScanState records how much of a file has been consumed.
func (s *Store) SaveScanState(st model.ScanState) error {
	_, err := s.db.Exec(`
		INSERT INTO scan_state (path, agent, size, mtime_ns, offset, prefix_hash,
		                        prefix_len, cursor, session_uid, complete, scanned_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET
			agent = excluded.agent, size = excluded.size, mtime_ns = excluded.mtime_ns,
			offset = excluded.offset, prefix_hash = excluded.prefix_hash,
			prefix_len = excluded.prefix_len, cursor = excluded.cursor,
			session_uid = excluded.session_uid,
			complete = excluded.complete, scanned_at = excluded.scanned_at`,
		st.Path, st.Agent, st.Size, st.ModTime.UnixNano(), st.Offset,
		st.PrefixHash, st.PrefixLen, st.Cursor, st.SessionUID, boolInt(st.Complete),
		st.Scanned.Unix())
	return err
}

// ForgetScanStates removes bookkeeping for files that no longer exist. The
// caller is responsible for deciding whether the session should also be
// forgotten.
func (s *Store) ForgetScanStates(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	return s.InTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare("DELETE FROM scan_state WHERE path = ?")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, p := range paths {
			if _, err := stmt.Exec(p); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordScanStatus stores the outcome of one scan of one source.
func (s *Store) RecordScanStatus(agent string, st model.ScanStatus) error {
	full := 0
	if st.LastFull {
		full = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO scan_status (agent, last_scan_at, last_full_at, files_seen, files_changed, calls_ingested, error)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(agent) DO UPDATE SET
			last_scan_at   = MAX(scan_status.last_scan_at, excluded.last_scan_at),
			last_full_at   = CASE WHEN excluded.last_full_at > 0 THEN excluded.last_full_at ELSE scan_status.last_full_at END,
			files_seen     = excluded.files_seen,
			files_changed  = excluded.files_changed,
			calls_ingested = excluded.calls_ingested,
			error          = excluded.error`,
		agent, st.LastScanAt.Unix(), full, st.FilesSeen, st.FilesChanged, st.CallsIngested, st.Error)
	return err
}

// ScanStatuses returns the per-source scan state for display.
func (s *Store) ScanStatuses() ([]model.ScanStatus, error) {
	rows, err := s.db.Query(`
		SELECT agent, last_scan_at, last_full_at, files_seen, files_changed, calls_ingested, error
		FROM scan_status ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ScanStatus
	for rows.Next() {
		var st model.ScanStatus
		var lastScan, lastFull int64
		if err := rows.Scan(&st.Agent, &lastScan, &lastFull, &st.FilesSeen,
			&st.FilesChanged, &st.CallsIngested, &st.Error); err != nil {
			return nil, err
		}
		st.LastScanAt = time.Unix(lastScan, 0)
		if lastFull > 0 {
			st.LastFullAt = time.Unix(lastFull, 0)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- utilities

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// inPlaceholders builds "?,?,?" for n parameters.
func inPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
