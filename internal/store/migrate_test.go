package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// ---------------------------------------------------------------- fixtures

// v0Schema is the schema as it was before the migration list existed: a build
// that only ever ran `Exec(schema)` on open, and never wrote a user_version.
//
// It is the current `schema` with the version 2 statements taken back out,
// which is precisely the difference between what a pre-migration build wrote
// and what this build writes. Deriving it rather than pasting a frozen copy
// keeps the fixture from drifting into a second, wrong idea of the old shape;
// TestFreshAndMigratedSchemasAreIdentical is what guards the other direction.
func v0Schema() string {
	if !strings.Contains(schema, v2RollupIndexes) {
		panic("schema no longer includes v2RollupIndexes; the v0 fixture cannot be derived from it")
	}
	return strings.Replace(schema, v2RollupIndexes, "", 1)
}

const (
	fixtureDay   = "2026-03-15"
	fixtureStart = "2026-03-15T12:00:00"
	fixtureTime  = "2006-01-02T15:04:05"
)

// fixtureAt is the instant the fixture's first call is recorded at, in local
// time — which is what decides the day bucket, since day is derived from the
// call's own local date.
//
// The time.Local dependence is deliberate and load-bearing, not incidental: the
// call table stores a local YYYY-MM-DD the parser computed, and the queries group
// by that string. So the fixture has to be constructed in the same zone the
// queries will read it in, or every day-bucketing assertion fails for a reason
// that has nothing to do with the code. Verified across UTC, UTC+14 and a
// half-hour-DST zone; all three pass. Constructing the fixture in UTC instead
// would be simpler and wrong: it would only work in the zone it was written in.
func fixtureAt(t *testing.T) time.Time {
	t.Helper()
	at, err := time.ParseInLocation(fixtureTime, fixtureStart, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// fixture is a hand-checkable set of rows. Two sessions, two projects, two
// agents, three models, one unpriced call, and a tool call attributed to a cost
// that is not its own. Every rollup's expected output is written out below
// rather than recomputed, so a migration that quietly mangled a row would have
// to conspire to produce the same numbers to pass.
func fixture(t *testing.T, at time.Time) []model.SessionWrite {
	t.Helper()
	from := func(n float64) time.Time { return at.Add(time.Duration(n * float64(time.Minute))) }
	one := func(uid, key, agent, project, m string, minutes float64, in, out, cr, cw, re, tot int, llm, cost float64, priced bool) model.Call {
		return model.Call{
			SessionUID: uid, CallKey: key, Agent: agent, Project: project, Model: m,
			Time: from(minutes), InputTokens: in, OutputTokens: out,
			CacheReadTokens: cr, CacheWriteTokens: cw, ReasoningTokens: re,
			TotalTokens: tot, LLMSeconds: llm, CostUSD: cost, Priced: priced,
		}
	}
	return []model.SessionWrite{
		{
			UID: "s1", Agent: "claude", Project: "/alpha",
			Calls: []model.Call{
				one("s1", "c1", "claude", "/alpha", "m1", 0, 10, 5, 2, 1, 3, 21, 1.5, 1.00, true),
				one("s1", "c2", "claude", "/alpha", "m1", 1, 20, 10, 4, 2, 6, 42, 2.5, 2.00, true),
				// Unpriced: the call exists, its cost is unknown rather than zero.
				one("s1", "c3", "claude", "/alpha", "m2", 2, 0, 0, 0, 0, 0, 0, 0, 0, false),
			},
			ToolCalls: []model.ToolCall{
				// Both sit between calls, so each is attributed to the *next*
				// call's cost: 2.00 and 0.00.
				{SessionUID: "s1", CallKey: "t1", Agent: "claude", Project: "/alpha", Tool: "bash", Time: from(0.5), Seconds: 1.0},
				{SessionUID: "s1", CallKey: "t2", Agent: "claude", Project: "/alpha", Tool: "read", Time: from(1.5), Seconds: 2.0, IsError: true},
			},
		},
		{
			UID: "s2", Agent: "opencode", Project: "/beta",
			Calls: []model.Call{
				one("s2", "c1", "opencode", "/beta", "m2", 60, 1, 1, 0, 0, 0, 2, 0.5, 4.00, true),
			},
		},
	}
}

// openV0 creates at path a database in the pre-migration shape — the whole v1
// schema, no user_version, none of the version 2 indexes — populated with the
// fixture through the ordinary ingestion path, and leaves it closed.
func openV0(t *testing.T, path string) {
	t.Helper()
	db := rawDB(t, path)
	if _, err := db.Exec(v0Schema()); err != nil {
		t.Fatalf("create v0 schema: %v", err)
	}
	at := fixtureAt(t)
	st := &Store{db: db, q: db, Path: path}
	for _, sess := range fixture(t, at) {
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatalf("seed %s: %v", sess.UID, err)
		}
		if err := st.RecomputeSession(sess.UID); err != nil {
			t.Fatalf("seed summaries %s: %v", sess.UID, err)
		}
	}
	// The fixture has to be a v0 database, or the tests below prove nothing.
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 0 {
		t.Fatalf("v0 fixture reports version %d, want 0", v)
	}
	for _, name := range v2IndexNames {
		if indexExists(t, db, name) {
			t.Fatalf("v0 fixture already has %s; it is not a v0 database", name)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// v2IndexNames are the indexes version 2 adds. Kept as a list so the tests can
// assert both that a v0 database is missing them and that a migrated one has
// them, without repeating the names.
var v2IndexNames = []string{
	"call_day_model_cost",
	"call_model_roll",
	"call_project_model_cost",
	"call_session_ts",
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	return db
}

func userVersion(t *testing.T, st *Store) int {
	t.Helper()
	var v int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var found int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name = ?", name).Scan(&found); err != nil {
		t.Fatal(err)
	}
	return found > 0
}

// appendMigration installs an extra step for the duration of one test.
//
// The migration list is a package variable precisely so that the mechanism can
// be exercised with a step that did not exist when the code was written; this
// is how the "add a column to a table that is already there" case is tested
// without shipping a column nobody needs.
func appendMigration(t *testing.T, m migration) {
	t.Helper()
	saved := migrations
	t.Cleanup(func() { migrations = saved })
	migrations = append(append([]migration{}, saved...), m)
}

// ensureMigration appends m only when a step of that version is not already in
// the list.
//
// It lets a test exercise a step without caring whether the step has been wired
// into `migrations` yet: the append is a no-op once it is, so the test keeps
// meaning what it meant either way. Tests that need the list to be malformed —
// out of order, or with a deliberate duplicate — must use appendMigration
// instead, which always appends.
func ensureMigration(t *testing.T, m migration) {
	t.Helper()
	for _, existing := range migrations {
		if existing.version == m.version {
			return
		}
	}
	appendMigration(t, m)
}

func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

// ---------------------------------------------------------------- migration

// TestMigrationFromV0Database is the upgrade path that did not exist before:
// a database written by a build with no version bookkeeping, opened by a build
// that has some. It must be brought forward, keep every row, and be usable.
func TestMigrationFromV0Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v0.db")
	openV0(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open a v0 database: %v", err)
	}
	defer st.Close()

	if got := userVersion(t, st); got != targetVersion() {
		t.Errorf("user_version = %d after migrating from v0, want %d", got, targetVersion())
	}
	for _, name := range v2IndexNames {
		if !indexExists(t, st.db, name) {
			t.Errorf("index %s missing after migration", name)
		}
	}
	var calls int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM call").Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("call has %d rows after migration, want 4", calls)
	}
	assertKnownRollups(t, st)
}

// TestFreshDatabaseIsAtLatestVersion: a brand new database is created straight
// at the current version, and opening it again is a no-op.
func TestFreshDatabaseIsAtLatestVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := userVersion(t, st); got != targetVersion() {
		t.Errorf("fresh database is at version %d, want %d", got, targetVersion())
	}
	for _, name := range v2IndexNames {
		if !indexExists(t, st.db, name) {
			t.Errorf("fresh database is missing %s", name)
		}
	}
	st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st.Close()
	if got := userVersion(t, st); got != targetVersion() {
		t.Errorf("version moved to %d on reopen, want %d", got, targetVersion())
	}
}

// TestMigrationIsIdempotent: open, close, reopen. Every process start runs the
// migration path, so it has to be a no-op once the version has been recorded.
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "again.db")
	openV0(t, path)

	for i := 0; i < 3; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if got := userVersion(t, st); got != targetVersion() {
			t.Errorf("open %d: version %d, want %d", i, got, targetVersion())
		}
		var calls int
		if err := st.db.QueryRow("SELECT COUNT(*) FROM call").Scan(&calls); err != nil {
			t.Fatal(err)
		}
		// Each pass after the first has added one session of its own.
		if want := 4 + max(0, i-1); calls != want {
			t.Fatalf("open %d: %d calls, want %d", i, calls, want)
		}
		// Write through the store on every pass, so a re-run has something it
		// could plausibly disturb.
		if i > 0 {
			at := time.Now()
			sess := model.SessionWrite{UID: fmt.Sprintf("extra%d", i), Agent: "pi", Project: "/gamma"}
			sess.Calls = append(sess.Calls, model.Call{
				SessionUID: sess.UID, CallKey: "c1", Agent: "pi", Project: "/gamma",
				Model: "m9", Time: at, InputTokens: 1, TotalTokens: 1, CostUSD: 0.5, Priced: true,
			})
			if err := st.ReplaceSession(sess); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var calls, sessions int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM call").Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow("SELECT COUNT(*) FROM session").Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if calls != 6 {
		t.Errorf("call has %d rows after three reopens, want 6", calls)
	}
	if sessions != 4 {
		t.Errorf("session has %d rows after three reopens, want 4", sessions)
	}
}

// TestFreshAndMigratedSchemasAreIdentical pins the invariant the two halves of
// a schema change have to agree on: a database created fresh and one upgraded
// from v0 must end up with exactly the same objects.
//
// This is the test that catches the mistake the mechanism exists to prevent. If
// someone adds an index or a table to `schema` and forgets the matching
// migration, the fresh path gets it and the upgrade path does not — and the
// symptom is a column or an index that is silently missing on exactly the
// databases people already had.
func TestFreshAndMigratedSchemasAreIdentical(t *testing.T) {
	dir := t.TempDir()
	freshPath := filepath.Join(dir, "fresh.db")
	migratedPath := filepath.Join(dir, "migrated.db")

	fresh, err := Open(freshPath)
	if err != nil {
		t.Fatal(err)
	}
	freshObjects := schemaObjects(t, fresh.db)
	fresh.Close()

	openV0(t, migratedPath)
	migrated, err := Open(migratedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	migratedObjects := schemaObjects(t, migrated.db)

	for name, sql := range freshObjects {
		got, ok := migratedObjects[name]
		if !ok {
			t.Errorf("%s exists in a fresh database but not in a migrated one", name)
			continue
		}
		if got != sql {
			t.Errorf("%s differs:\n fresh:    %s\n migrated: %s", name, sql, got)
		}
	}
	for name := range migratedObjects {
		if _, ok := freshObjects[name]; !ok {
			t.Errorf("%s exists in a migrated database but not in a fresh one", name)
		}
	}
	if len(freshObjects) == 0 {
		t.Fatal("no schema objects found; the comparison is vacuous")
	}
}

func schemaObjects(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(
		"SELECT type, name, COALESCE(sql,'') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var typ, name, sql string
		if err := rows.Scan(&typ, &name, &sql); err != nil {
			t.Fatal(err)
		}
		out[typ+" "+name] = sql
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAddColumnMigrationReachesAnExistingTable is the regression test for the
// defect this mechanism was written to fix.
//
// Before it, every schema statement was `CREATE TABLE IF NOT EXISTS`, so a
// version that added a column to an existing table would apply cleanly to a
// database that already had the table — and change nothing. The database would
// then fail at query time with "no such column", and there was no version
// bookkeeping anywhere to notice. Here a column is added to a table that
// already exists and already has rows in it.
func TestAddColumnMigrationReachesAnExistingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	openV0(t, path)

	// Stand in for the next release: a new column on `call`, plus an index that
	// uses it, which is what a real feature would bring with it.
	note := "TEXT NOT NULL DEFAULT ''"
	appendMigration(t, migration{
		version: targetVersion() + 1,
		name:    "add call.note",
		apply: func(tx *sql.Tx) error {
			if err := addColumn(tx, "call", "note", note); err != nil {
				return err
			}
			_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS call_note ON call(note)`)
			return err
		},
	})

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, want := userVersion(t, st), targetVersion(); got != want {
		t.Errorf("user_version = %d, want %d", got, want)
	}
	if !columnExists(t, st.db, "call", "note") {
		t.Fatal("the migration did not add the column to the pre-existing table")
	}
	if !indexExists(t, st.db, "call_note") {
		t.Error("the migration did not create the index that uses the new column")
	}

	// The rows that were already there are intact and carry the new column's
	// default, rather than being rewritten or dropped.
	rows, err := st.db.Query("SELECT call_key, cost_usd, note FROM call ORDER BY session_uid, call_key")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	for rows.Next() {
		var key, got string
		var cost float64
		if err := rows.Scan(&key, &cost, &got); err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("call %s has note %q, want the empty default", key, got)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Errorf("%d pre-existing calls survived, want 4", n)
	}

	// And the new column is usable by the code that needs it.
	var noted int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM call WHERE note = ''").Scan(&noted); err != nil {
		t.Fatal(err)
	}
	if noted != 4 {
		t.Errorf("%d rows match the new column's default, want 4", noted)
	}
	// A row written after the migration gets the same default, and an update
	// through the new column sticks.
	if _, err := st.db.Exec("UPDATE call SET note = 'x' WHERE session_uid = 's1' AND call_key = 'c1'"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening must not try to add the column a second time — that is the
	// idempotency the column-existence guard buys, and it is why an
	// interrupted run can simply be retried.
	st, err = Open(path)
	if err != nil {
		t.Fatalf("reopen after the column migration: %v", err)
	}
	defer st.Close()
	if got, want := userVersion(t, st), targetVersion(); got != want {
		t.Errorf("user_version = %d after reopen, want %d", got, want)
	}
	var marked int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM call WHERE note = 'x'").Scan(&marked); err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Errorf("%d rows carry the value written after the migration, want 1", marked)
	}
	// Force the step to run again on an already-migrated database: the guard,
	// not the version check, is what makes that safe.
	if err := st.applyMigration(migrations[len(migrations)-1]); err != nil {
		t.Errorf("re-running an applied migration: %v", err)
	}
	if !columnExists(t, st.db, "call", "note") {
		t.Error("re-running the migration lost the column")
	}
	if got := userVersion(t, st); got != targetVersion() {
		t.Errorf("re-running the migration moved the version to %d, want %d", got, targetVersion())
	}
}

// TestMigrationFailureRollsBack: a step that fails half-way must leave the
// database at the version it started from, with none of its effects applied.
//
// The version write shares the step's transaction precisely so that this holds;
// a step that recorded its version anyway would leave a database claiming a
// shape it does not have, and nothing would ever retry it.
func TestMigrationFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fail.db")
	openV0(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	before := userVersion(t, st)

	boom := errors.New("synthetic failure")
	version := before + 1
	err = st.applyMigration(migration{
		version: version,
		name:    "step that fails",
		apply: func(tx *sql.Tx) error {
			// A real effect first, so the rollback has something to undo.
			if err := addColumn(tx, "call", "never_added", "TEXT"); err != nil {
				return err
			}
			return boom
		},
	})
	if err == nil {
		t.Fatal("a failing migration reported success")
	}
	// The message has to name the step, or a failure in the field is a puzzle.
	for _, want := range []string{fmt.Sprint(version), "step that fails", boom.Error()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if got := userVersion(t, st); got != before {
		t.Errorf("user_version = %d after a failed migration, want %d", got, before)
	}
	if columnExists(t, st.db, "call", "never_added") {
		t.Error("the failed migration's column survived; the transaction did not roll back")
	}
	var calls int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM call").Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("call has %d rows after a failed migration, want 4", calls)
	}
}

// TestNewerSchemaIsRefused: a database written by a future dashd must not be
// opened by this one. Guessing at a newer shape is how a database gets quietly
// mangled, so the only safe answer is to refuse and say so.
func TestNewerSchemaIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", targetVersion()+50)); err != nil {
		t.Fatal(err)
	}
	st.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("opened a database from a newer schema version")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error %q does not say the database is newer", err)
	}
}

// TestMigrationListIsOrdered guards the list itself: an entry appended out of
// order would be skipped, and a duplicate would silently re-run a step.
func TestMigrationListIsOrdered(t *testing.T) {
	appendMigration(t, migration{version: 1, name: "out of order"})
	_, err := Open(filepath.Join(t.TempDir(), "unordered.db"))
	if err == nil {
		t.Fatal("an out-of-order migration list was accepted")
	}
	if !strings.Contains(err.Error(), "increasing version order") {
		t.Errorf("error %q does not name the problem", err)
	}
}

// ---------------------------------------------------------------- rollups

// assertKnownRollups checks every rollup against figures worked out by hand
// from the fixture. Run against a fresh database and against a migrated one, it
// is the guard that a migration did not disturb stored data.
func assertKnownRollups(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	at := fixtureAt(t)

	totals, err := st.Totals(ctx, Filter{})
	if err != nil {
		t.Fatalf("Totals: %v", err)
	}
	want := Totals{
		Cost: 7.00, Calls: 4,
		TotalTokens: 65, InputTokens: 31, OutputTokens: 16,
		CacheReadTokens: 6, CacheWriteTokens: 3, ReasoningTokens: 9,
		LLMSeconds: 4.5, ToolSeconds: 3.0,
		Sessions: 2, Projects: 2, Agents: 2,
		UnpricedCalls: 1,
		FirstTS:       at.Unix(), LastTS: at.Add(time.Hour).Unix(),
	}
	if diff := totals; diff != want {
		t.Errorf("Totals:\n got  %+v\n want %+v", diff, want)
	}

	daily, err := st.Daily(ctx, Filter{})
	if err != nil {
		t.Fatalf("Daily: %v", err)
	}
	if len(daily) != 1 {
		t.Fatalf("Daily returned %d buckets, want 1", len(daily))
	}
	if daily[0].Day != fixtureDay {
		t.Errorf("Daily day = %q, want %q", daily[0].Day, fixtureDay)
	}
	if math.Abs(daily[0].Cost-7.00) > 0.001 {
		t.Errorf("Daily cost = %.4f, want 7.00", daily[0].Cost)
	}
	for model, cost := range map[string]float64{"m1": 3.00, "m2": 4.00} {
		if math.Abs(daily[0].Models[model]-cost) > 0.001 {
			t.Errorf("Daily %s = %.4f, want %.2f", model, daily[0].Models[model], cost)
		}
	}

	models, err := st.Models(ctx, Filter{})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("Models returned %d rows, want 2", len(models))
	}
	// Most expensive first.
	if models[0].Model != "m2" || math.Abs(models[0].Cost-4.00) > 0.001 ||
		models[0].Calls != 2 || models[0].Unpriced != 1 || models[0].TotalTokens != 2 {
		t.Errorf("Models[0] = %+v, want m2 cost 4.00 over 2 calls, 1 unpriced", models[0])
	}
	if models[1].Model != "m1" || math.Abs(models[1].Cost-3.00) > 0.001 ||
		models[1].Calls != 2 || models[1].Unpriced != 0 || models[1].TotalTokens != 63 {
		t.Errorf("Models[1] = %+v, want m1 cost 3.00 over 2 calls, 63 tokens", models[1])
	}
	if models[0].FirstTS != at.Add(2*time.Minute).Unix() ||
		models[0].LastTS != at.Add(time.Hour).Unix() {
		t.Errorf("Models[0] spans %d..%d, want %d..%d",
			models[0].FirstTS, models[0].LastTS,
			at.Add(2*time.Minute).Unix(), at.Add(time.Hour).Unix())
	}

	pm, err := st.ProjectModels(ctx, Filter{})
	if err != nil {
		t.Fatalf("ProjectModels: %v", err)
	}
	if len(pm) != 3 {
		t.Fatalf("ProjectModels returned %d rows, want 3", len(pm))
	}
	checks := []struct {
		project, model string
		cost           float64
		calls          int
	}{
		{"/alpha", "m1", 3.00, 2},
		{"/alpha", "m2", 0.00, 1},
		{"/beta", "m2", 4.00, 1},
	}
	for i, c := range checks {
		if pm[i].Project != c.project || pm[i].Model != c.model {
			t.Errorf("ProjectModels[%d] = %s/%s, want %s/%s", i, pm[i].Project, pm[i].Model, c.project, c.model)
			continue
		}
		if math.Abs(pm[i].Cost-c.cost) > 0.001 || pm[i].Calls != c.calls {
			t.Errorf("ProjectModels[%d] = %s/%s cost %.4f over %d calls, want %.2f over %d",
				i, c.project, c.model, pm[i].Cost, pm[i].Calls, c.cost, c.calls)
		}
	}

	tools, err := st.Tools(ctx, Filter{})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("Tools returned %d rows, want 2", len(tools))
	}
	// bash sits between c1 and c2, so it is attributed to c2's cost; read sits
	// between c2 and c3, the unpriced call.
	wantTools := map[string]ToolStat{
		"bash": {Tool: "bash", Calls: 1, Seconds: 1.0, Errors: 0, Cost: 2.00},
		"read": {Tool: "read", Calls: 1, Seconds: 2.0, Errors: 1, Cost: 0.00},
	}
	for _, tool := range tools {
		w, ok := wantTools[tool.Tool]
		if !ok {
			t.Errorf("unexpected tool %q", tool.Tool)
			continue
		}
		if math.Abs(tool.Cost-w.Cost) > 0.001 || tool.Calls != w.Calls ||
			tool.Errors != w.Errors || math.Abs(tool.Seconds-w.Seconds) > 0.001 {
			t.Errorf("Tools[%s] = %+v, want %+v", tool.Tool, tool, w)
		}
	}
}

// TestRollupsAreCorrectOnAFreshDatabase runs the same expected figures as the
// migration test against a database that was never migrated, so a wrong
// expectation cannot be mistaken for a migration bug and vice versa.
func TestRollupsAreCorrectOnAFreshDatabase(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := fixtureAt(t)
	for _, sess := range fixture(t, at) {
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(sess.UID); err != nil {
			t.Fatal(err)
		}
	}
	assertKnownRollups(t, st)
}

// TestIngestRoundTrip checks that what ingestion writes is what the rollups
// read back, including for a call with no timestamp at all — the one row shape
// every date-windowed query has to exclude rather than misplace.
func TestIngestRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "round.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// Re-reading the same session must replace, not duplicate.
	sess := model.SessionWrite{UID: "s", Agent: "pi", Project: "/p", Path: "/tmp/log", Title: "t"}
	sess.Calls = []model.Call{
		{SessionUID: "s", CallKey: "a", Agent: "pi", Project: "/p", Model: "m", InputTokens: 5, TotalTokens: 5, CostUSD: 1, Priced: true},
		{SessionUID: "s", CallKey: "b", Agent: "pi", Project: "/p", Model: "m", InputTokens: 5, TotalTokens: 5, CostUSD: 1, Priced: true},
	}
	sess.ToolCalls = []model.ToolCall{{SessionUID: "s", CallKey: "a", Tool: "bash", Seconds: 1}}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s"); err != nil {
		t.Fatal(err)
	}
	// A second full read, now with one call: the row that is gone must be gone.
	sess.Calls = sess.Calls[:1]
	sess.ToolCalls = nil
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s"); err != nil {
		t.Fatal(err)
	}

	totals, err := st.Totals(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Calls != 1 {
		t.Errorf("Calls = %d after a shrinking re-read, want 1", totals.Calls)
	}
	if totals.Cost != 1 {
		t.Errorf("Cost = %v, want 1", totals.Cost)
	}
	if totals.FirstTS != 0 || totals.LastTS != 0 {
		t.Errorf("timestamps %d..%d, want 0 for a call with no time", totals.FirstTS, totals.LastTS)
	}
	row, ok, err := st.Session(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Session: %v ok=%v", err, ok)
	}
	if row.Calls != 1 || row.Cost != 1 {
		t.Errorf("session summary calls=%d cost=%v, want 1 and 1", row.Calls, row.Cost)
	}
	if row.Title != "t" || row.Path != "/tmp/log" {
		t.Errorf("session metadata lost: %+v", row)
	}

	calls, err := st.SessionCalls(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Model != "m" || !calls[0].Priced {
		t.Errorf("SessionCalls = %+v, want the one surviving call", calls)
	}
}

// ---------------------------------------------------------------- plans

// TestRollupPlansUseCoveringIndexes pins the performance claim.
//
// The rollups read `call`, which is WITHOUT ROWID, so an index that does not
// carry the aggregated columns costs a primary-key btree descent per row. These
// are the plans that make each rollup a covering-index scan; a later schema edit
// that quietly drops or shadows one of these indexes would show up here as a
// table scan rather than as a slow page nobody can explain.
func TestRollupPlansUseCoveringIndexes(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "plans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	at := fixtureAt(t)
	for _, sess := range fixture(t, at) {
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
	}

	where, args := Filter{}.where("", "ts")
	twhere, targs := Filter{}.where("t", "ts")
	for _, tc := range []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{"daily", dailySQL(where), args, "call_day_model_cost"},
		{"models", modelsSQL(where), args, "call_model_roll"},
		{"project models", projectModelsSQL(where), args, "call_project_model_cost"},
		{"tools", toolsSQL(twhere), targs, "call_session_ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := strings.Join(planFor(t, st, tc.query, tc.args...), " | ")
			t.Logf("plan: %s", plan)
			assertCoveringIndexScan(t, plan, tc.want)
		})
	}
}

// assertCoveringIndexScan checks one EXPLAIN QUERY PLAN result.
//
// SQLite's plan wording is not a stable interface: the exact phrasing of a
// covering-index scan has changed between releases ("USING COVERING INDEX x",
// "SEARCH ... USING COVERING INDEX x", "SCAN ... USING COVERING INDEX x"), and a
// modernc.org/sqlite bump can change it without any change to this schema or
// these queries. Matching the literal string meant a version bump produced a
// failure that looked like a missing index.
//
// So the wording is matched loosely and in one place, and what is asserted is the
// property rather than the phrasing: the named index is used to satisfy the query,
// and it does so as a covering scan rather than by reading the table. A plan that
// mentions the index but also scans the table for the row is the failure this
// catches, and it is stated in those terms.
func assertCoveringIndexScan(t *testing.T, plan, index string) {
	t.Helper()
	steps := strings.Split(plan, " | ")

	usesIndex, covering := false, false
	for _, step := range steps {
		if !strings.Contains(step, index) {
			continue
		}
		usesIndex = true
		// "COVERING" is the one word that carries the meaning here and is stable
		// across the phrasings above: it says every column the query needs is in
		// the index, so the table is not read.
		if strings.Contains(strings.ToUpper(step), "COVERING") {
			covering = true
		}
	}
	if !usesIndex {
		t.Errorf("the plan does not use %s, which exists to make this rollup a "+
			"covering scan:\n%s", index, plan)
		return
	}
	if !covering {
		t.Errorf("the plan uses %s but not as a covering index, so it reads the "+
			"table for every row:\n%s", index, plan)
	}
	// A full table scan is the failure this whole test exists for, and it is
	// detectable independently of any wording: a step that starts by scanning the
	// table with no index named.
	for _, step := range steps {
		if isFullScan(step) {
			t.Errorf("the plan scans a whole table:\n%s", plan)
		}
	}
}

// planFor returns the rows of EXPLAIN QUERY PLAN, one per line.
func planFor(t *testing.T, st *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("EXPLAIN QUERY PLAN returned no rows; the assertion would pass vacuously")
	}
	return out
}
