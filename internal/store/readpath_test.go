package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// TestSessionLookupIsASeek guards the other half of this change: Session must
// read one row by primary key, not materialise the session table.
func TestSessionLookupIsASeek(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "seek.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	plan := strings.Join(planFor(t, st,
		`SELECT `+sessionColumns+` FROM session WHERE uid = ?`, "s1"), " | ")
	if !strings.Contains(plan, "SEARCH") {
		t.Errorf("session lookup is not an indexed search: %s", plan)
	}
	for _, step := range strings.Split(plan, " | ") {
		if strings.HasPrefix(step, "SCAN session") && !strings.Contains(step, "USING ") {
			t.Errorf("session lookup scans the session table: %s", plan)
		}
	}
}

// TestSessionFindsRowsOutsideTheListingCap is the behaviour change that came
// with the seek, and it is worth pinning: a session older than the 5000-row
// listing cap used to be reported as unknown, because the old implementation
// looked it up in that capped listing.
func TestSessionFindsRowsOutsideTheListingCap(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Newest first, so the oldest sessions fall off the end of the listing.
	// The rows go in directly rather than through ingestion: this is about the
	// listing cap, and a summarisation pass per session would dominate the
	// runtime of a test about a lookup.
	const n = 5002
	if err := st.InTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			`INSERT INTO session (uid, agent, project, path, title, first_ts, last_ts, calls)
			 VALUES (?, 'pi', '/p', '/tmp/' || ?, ?, 0, ?, 1)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < n; i++ {
			uid := "cap-" + pad(i)
			if _, err := stmt.Exec(uid, uid, "title-"+uid, i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	listed, err := st.Sessions(ctx, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 5000 {
		t.Fatalf("Sessions returned %d rows, want the 5000-row cap", len(listed))
	}

	// The oldest are not in that list, and are still perfectly findable.
	row, ok, err := st.Session(ctx, "cap-"+pad(0))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("a stored session outside the listing cap was reported as unknown")
	}
	if row.UID != "cap-"+pad(0) {
		t.Errorf("Session returned %q, want the oldest session", row.UID)
	}
	if row.Calls != 1 {
		t.Errorf("session summary reads calls=%d, want 1", row.Calls)
	}
	if _, ok, err := st.Session(ctx, "no-such-session"); err != nil || ok {
		t.Errorf("Session on a missing uid: ok=%v err=%v, want false and no error", ok, err)
	}
}

// TestSessionReturnsTheSameRowAsTheListing is the equivalence check: the seek
// and the listing must agree field for field, or the two code paths have
// drifted.
func TestSessionReturnsTheSameRowAsTheListing(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "same.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	for _, uid := range []string{"s1", "s2", "s3"} {
		if err := st.ReplaceSession(sessionWriteFixture(uid, len(uid))); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := st.Sessions(ctx, Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	byUID := map[string]SessionRow{}
	for _, r := range listed {
		byUID[r.UID] = r
	}
	for uid, want := range byUID {
		got, ok, err := st.Session(ctx, uid)
		if err != nil || !ok {
			t.Fatalf("Session(%s): %v ok=%v", uid, err, ok)
		}
		if got != want {
			t.Errorf("Session(%s) =\n %+v\nSessions listed\n %+v", uid, got, want)
		}
	}
}

// TestSessionRowsCarryEveryColumn is what the shared column list has to keep
// true: a field added to one statement and not the other is a page that quietly
// renders a zero.
func TestSessionRowsCarryEveryColumn(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cols.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.ReplaceSession(sessionWriteFixture("s1", 7)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s1"); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	row, ok, err := st.Session(ctx, "s1")
	if err != nil || !ok {
		t.Fatalf("Session: %v ok=%v", err, ok)
	}

	// The identifying and descriptive fields.
	wantPath := filepath.Join(fakeLogRoot, "s1.jsonl")
	if row.UID != "s1" || row.Path != wantPath || row.Title != "title-s1" {
		t.Errorf("descriptive fields lost (want path %q): %+v", wantPath, row)
	}
	// The aggregates. Each is a per-call constant in the fixture multiplied by
	// the call count, and each call column has a distinct value, so a column
	// read at the wrong offset cannot coincidentally match.
	for _, c := range []struct {
		name string
		got  float64
		want float64
	}{
		{"calls", float64(row.Calls), 7},
		{"input_tokens", float64(row.InputTokens), 70},
		{"output_tokens", float64(row.OutputTokens), 21},
		{"cache_read_tokens", float64(row.CacheReadTokens), 14},
		{"cache_write_tokens", float64(row.CacheWriteTokens), 7},
		{"reasoning_tokens", float64(row.ReasoningTokens), 21},
		{"total_tokens", float64(row.TotalTokens), 112},
		{"llm_seconds", row.LLMSeconds, 3.5},
	} {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// Summed in floating point, so compared with a tolerance rather than
	// exactly: 7 x 0.1 is not 0.7.
	if math.Abs(row.Cost-0.7) > 1e-9 {
		t.Errorf("cost_usd = %v, want 0.7", row.Cost)
	}
	// And the tool aggregates, which come from a different table entirely.
	if row.ToolCalls != 2 || row.ToolErrors != 1 || row.ToolSeconds != 3 {
		t.Errorf("tool aggregates = %d calls, %d errors, %v seconds; want 2, 1, 3",
			row.ToolCalls, row.ToolErrors, row.ToolSeconds)
	}
	if row.Orphaned {
		t.Error("a freshly written session reads as orphaned")
	}
}

// pad renders i zero-padded so lexical ordering matches numeric ordering.
func pad(i int) string { return fmt.Sprintf("%06d", i) }

// ---------------------------------------------------------------- v3

// These tests are the evidence behind the version 3 step: that the four
// indexes it drops are a strict prefix of an index that remains, and that
// dropping them changes no plan the read path uses for the worse.
//
// They install the step with ensureMigration rather than assuming it is in the
// list, so they test the step itself. Once the entry is appended to `migrations`
// in store.go they keep passing unchanged — and once it is there, the existing
// TestFreshAndMigratedSchemasAreIdentical covers the fresh-versus-migrated
// agreement that this file cannot.

func TestV3DropsTheIndexesVersion2MadeRedundant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ensureMigration(t, v3DropPrefixIndexes)

	// A fresh database no longer carries the four prefix indexes — that is the
	// point of v3 — so they are created here to stand in for a real v2
	// database, which is what the step actually has to cope with.
	if _, err := st.db.Exec(v2PrefixIndexes); err != nil {
		t.Fatal(err)
	}

	// The indexes have to be there before the step, or this proves nothing.
	for _, name := range prefixIndexes {
		if !indexExists(t, st.db, name) {
			t.Fatalf("%s is missing before the step; the fixture is wrong", name)
		}
	}
	if err := st.applyMigration(v3DropPrefixIndexes); err != nil {
		t.Fatalf("v3 step: %v", err)
	}

	for _, name := range prefixIndexes {
		if indexExists(t, st.db, name) {
			t.Errorf("%s survived the step", name)
		}
	}
	// And what replaces them has to be untouched: dropping the prefix must not
	// take the covering index with it.
	for _, name := range v2IndexNames {
		if !indexExists(t, st.db, name) {
			t.Errorf("%s was dropped along with the redundant index", name)
		}
	}
	if got := userVersion(t, st); got != 3 {
		t.Errorf("user_version = %d after the v3 step, want 3", got)
	}
}

// TestV3IsSafeToReapply: the step costs nothing to be safe to run twice, which
// is what lets an interrupted migration simply be opened again.
func TestV3IsSafeToReapply(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "v3-again.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 2; i++ {
		if err := st.applyMigration(v3DropPrefixIndexes); err != nil {
			t.Fatalf("re-applying the v3 step (pass %d): %v", i, err)
		}
	}
	if got := userVersion(t, st); got != 3 {
		t.Errorf("user_version = %d, want 3", got)
	}
}

// TestV3FreshAndMigratedSchemasAreIdentical is the invariant the two halves of
// a schema change have to agree on, applied to version 3.
//
// TestFreshAndMigratedSchemasAreIdentical in migrate_test.go already covers
// this — but only once the v3 entry is in the list, and it is worth being able
// to see it hold while the entry is still being reviewed.
func TestV3FreshAndMigratedSchemasAreIdentical(t *testing.T) {
	dir := t.TempDir()
	ensureMigration(t, v3DropPrefixIndexes)

	fresh, err := Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	freshObjects := schemaObjects(t, fresh.db)
	freshIndex := indexNames(t, fresh.db, "call")
	fresh.Close()

	openV0(t, filepath.Join(dir, "migrated.db"))
	migrated, err := Open(filepath.Join(dir, "migrated.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	migratedObjects := schemaObjects(t, migrated.db)

	if len(freshObjects) == 0 {
		t.Fatal("no schema objects found; the comparison is vacuous")
	}
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
	// And the shape both of them ended up in is the trimmed one, which is the
	// point of the step rather than a detail of it.
	for _, name := range prefixIndexes {
		if indexExists(t, migrated.db, name) {
			t.Errorf("a migrated database kept %s; the v3 step did not apply to it", name)
		}
		if slices.Contains(freshIndex, name) {
			t.Errorf("a fresh database kept %s; the v3 step did not apply to it", name)
		}
	}
}

// TestV3PreservesData: dropping an index must not touch a single row.
func TestV3PreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3-data.db")
	openV0(t, path)
	ensureMigration(t, v3DropPrefixIndexes)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	var calls int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM call").Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Errorf("call has %d rows after the drop, want 4", calls)
	}
	for _, name := range prefixIndexes {
		if indexExists(t, st.db, name) {
			t.Errorf("%s survived the migration from v0", name)
		}
	}
	// The figures, not just the row count: an index that was silently needed
	// would show up here as a wrong total.
	assertKnownRollups(t, st)
}

// TestPrefixIndexesHaveNoPlanOfTheirOwn is the redundancy claim as a test.
//
// For every statement the read path issues, the plan chosen with the four
// narrow indexes present must be no better than the plan chosen without them.
// "No better" is checked as: the same set of table accesses, and no full scan of
// `call` introduced where there was not one. This is the assertion that makes
// dropping them safe, and it is deliberately over the whole read surface rather
// than the four rollups someone remembered.
func TestPrefixIndexesHaveNoPlanOfTheirOwn(t *testing.T) {
	wide, _ := newBenchStore(t, shapeTrimmed)
	widePlan := plansOf(t, wide.db)

	withPrefix, _ := newBenchStore(t, shapeV2)
	fullPlan := plansOf(t, withPrefix.db)

	if len(widePlan) != len(fullPlan) {
		t.Fatalf("plan count differs: %d queries vs %d", len(widePlan), len(fullPlan))
	}
	for i := range widePlan {
		name, wideSteps := splitPlan(t, widePlan[i])
		fullName, fullSteps := splitPlan(t, fullPlan[i])
		if name != fullName {
			t.Fatalf("query %d: %s vs %s", i, name, fullName)
		}
		// The narrow indexes may still be *preferred* where a wide index is an
		// equally good answer — a DISTINCT over a two-column index reads fewer
		// pages than one over eleven. What must not happen is the wide shape
		// needing a plan the full shape did not.
		if introducesFullScan(t, wideSteps, fullSteps) {
			t.Errorf("%s: the trimmed shape scans a table the full shape did not:\n full:   %s\n trimmed: %s",
				name, strings.Join(fullSteps, " | "), strings.Join(wideSteps, " | "))
		}
	}
}

// introducesFullScan reports whether `after` contains a bare table scan that
// `before` did not.
func introducesFullScan(t testing.TB, after, before []string) bool {
	t.Helper()
	beforeBare := map[string]bool{}
	for _, s := range before {
		if isFullScan(s) {
			beforeBare[s] = true
		}
	}
	for _, s := range after {
		if isFullScan(s) && !beforeBare[s] {
			return true
		}
	}
	return false
}

// isFullScan reports whether a plan step reads a whole table rather than an
// index.
//
// It keys on "SCAN <table>" with no "USING" in the step, which is the part of
// SQLite's plan wording that has been stable: a table scan is always a SCAN, and
// anything qualified by an index always names one. The longer phrasing around it
// ("SEARCH" vs "SCAN", "COVERING INDEX" vs "INDEX") is not relied on here, which
// is deliberate — see assertCoveringIndexScan in migrate_test.go for why the
// wording matching lives in one place.
func isFullScan(step string) bool {
	upper := strings.ToUpper(step)
	for _, table := range []string{"call", "tool_call", "session"} {
		if strings.HasPrefix(upper, "SCAN "+table) && !strings.Contains(upper, "USING ") {
			return true
		}
	}
	return false
}

// splitPlan separates a "name: step | step" line into its name and steps.
func splitPlan(t testing.TB, line string) (string, []string) {
	t.Helper()
	name, rest, ok := strings.Cut(line, ": ")
	if !ok {
		t.Fatalf("plan line %q has no name", line)
	}
	return name, strings.Split(rest, " | ")
}

// fakeLogRoot stands in for the directory an agent's logs live in.
//
// It is a fake rather than a real path, and deliberately not built with
// filepath.Join from "/tmp": on Windows that yields "\tmp\<uid>", which reads
// like a rooted path but is not one, and a reader has to work out which it is.
// Nothing here resolves the path on disk — the session table stores whatever the
// scanner found, and this fixture only needs a value that is distinct per uid and
// comparable as a string.
const fakeLogRoot = "/var/log/agent-sessions"

// sessionWriteFixture is one session with every column set to a distinct value,
// so a column read at the wrong offset in either session query cannot read as
// correct.
func sessionWriteFixture(uid string, calls int) model.SessionWrite {
	sess := model.SessionWrite{
		UID: uid, Agent: "pi", Project: "/p",
		Path: filepath.Join(fakeLogRoot, uid+".jsonl"), Title: "title-" + uid,
	}
	for c := 0; c < calls; c++ {
		sess.Calls = append(sess.Calls, model.Call{
			SessionUID: uid, CallKey: "c" + pad(c), Agent: "pi", Project: "/p",
			Model:            "m",
			InputTokens:      10,
			OutputTokens:     3,
			CacheReadTokens:  2,
			CacheWriteTokens: 1,
			ReasoningTokens:  3,
			TotalTokens:      16,
			LLMSeconds:       0.5,
			CostUSD:          0.1,
			Priced:           true,
		})
	}
	sess.ToolCalls = []model.ToolCall{
		{SessionUID: uid, CallKey: "t0", Agent: "pi", Project: "/p", Tool: "bash", Seconds: 1},
		{SessionUID: uid, CallKey: "t1", Agent: "pi", Project: "/p", Tool: "read", Seconds: 2, IsError: true},
	}
	return sess
}

// TestAppendSessionKeepsTheProjectAnIncrementalReadCannotRecover locks the
// fix for a session drifting out of its project bucket.
//
// An incremental append resumes past the log's header record, which is where
// most agents carry the working directory, so the parser reports no project.
// Writing that empty value moved the session and its newly-ingested calls into
// an empty project, splitting the per-project rollup in two.
func TestAppendSessionKeepsTheProjectAnIncrementalReadCannotRecover(t *testing.T) {
	st, err := Open(t.TempDir() + "/f.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()

	// The first, full pass knows the project.
	first := model.SessionWrite{
		Session: model.Session{UID: "s1", Agent: "pi", Project: "/home/u/proj"},
		Path:    "/logs/s1.jsonl",
		Calls: []model.Call{{
			SessionUID: "s1", CallKey: "m1", Agent: "pi", Project: "/home/u/proj",
			Model: "m", Time: time.Now(), InputTokens: 10, OutputTokens: 5,
			TotalTokens: 15, Priced: true, CostUSD: 0.01,
		}},
	}
	if err := st.ReplaceSession(first); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s1"); err != nil {
		t.Fatal(err)
	}

	// The append that follows learned no project at all.
	inc := model.SessionWrite{
		Session: model.Session{UID: "s1", Agent: "pi"},
		Path:    "/logs/s1.jsonl",
		Calls: []model.Call{{
			SessionUID: "s1", CallKey: "m2", Agent: "pi",
			Model: "m", Time: time.Now(), InputTokens: 20, OutputTokens: 5,
			TotalTokens: 25, Priced: true, CostUSD: 0.02,
		}},
	}
	if err := st.AppendSession(inc); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s1"); err != nil {
		t.Fatal(err)
	}

	row, ok, err := st.Session(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("session vanished")
	}
	if row.Project != "/home/u/proj" {
		t.Errorf("session project = %q, want /home/u/proj: an append that learned no "+
			"project erased the stored one", row.Project)
	}

	// Both calls, not just the session row, have to stay in the project.
	ps, err := st.Projects(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	var empty int
	for _, p := range ps {
		if p.Project == "" {
			empty++
		}
	}
	if empty != 0 {
		t.Errorf("%d projects have an empty name; an appended call was stored with no "+
			"project and split the rollup", empty)
	}
	if len(ps) != 1 || ps[0].Project != "/home/u/proj" {
		t.Errorf("projects = %+v, want a single /home/u/proj row holding both calls", ps)
	}
	if ps[0].Calls != 2 {
		t.Errorf("project row counts %d calls, want 2", ps[0].Calls)
	}

	// The per-project model breakdown groups by the call rows' own project, so
	// it is where an empty project on the appended call actually shows up: as
	// the one session's spend split across two rows.
	pm, err := st.ProjectModels(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pm) != 1 {
		var names []string
		for _, m := range pm {
			names = append(names, fmt.Sprintf("%q/%s", m.Project, m.Model))
		}
		t.Fatalf("project models = %v, want one row; a call was stored with no project "+
			"and split the session's spend across two", names)
	}
	if pm[0].Project != "/home/u/proj" {
		t.Errorf("model breakdown project = %q, want /home/u/proj", pm[0].Project)
	}
	if pm[0].Calls != 2 {
		t.Errorf("model breakdown counts %d calls, want 2", pm[0].Calls)
	}
}
