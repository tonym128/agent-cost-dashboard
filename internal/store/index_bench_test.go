package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// This file is the evidence behind the version 3 schema step: whether
// call_day, call_project, call_session and call_model are still worth
// maintaining now that the covering indexes of version 2 exist.
//
// Everything here is built to be run rather than reasoned about. The same rows
// are ingested through the same ingestion API under each candidate index set,
// and the same statements the page runs are timed and explained against each.
// Three shapes are compared:
//
//	baseline  v1: the six single-column indexes, no covering indexes. This is
//	          the database as it was before v2, and the "before" of the
//	          comparison.
//	v2        everything, as shipped.
//	trimmed  v2 minus the four indexes that are a strict prefix of one of its
//	          own. This is what v3 would leave behind.
const (
	shapeBaseline = "baseline"
	shapeV2       = "v2"
	shapeTrimmed  = "trimmed"
)

var shapes = []string{shapeBaseline, shapeV2, shapeTrimmed}

// prefixIndexes are the indexes the version 2 covering indexes made redundant:
// each is a strict prefix of one of them, so any plan that could use the narrow
// index can use the wide one instead. This is the set the v3 step drops, and it
// must stay in step with v3DropPrefixIndexes in migrate_v3.go — the test
// TestV3DropsTheIndexesVersion2MadeRedundant is what holds them together.
var prefixIndexes = []string{
	"call_day",
	"call_project",
	"call_session",
	"call_model",
}

// v2Indexes are the four covering indexes. v2IndexNames in migrate_test.go is
// the same list; this one is used for dropping, and spelled out separately so
// that the shape names above read without a lookup.
var v2Indexes = []string{
	"call_day_model_cost",
	"call_model_roll",
	"call_project_model_cost",
	"call_session_ts",
}

// applyShape rewrites an already-open database into one of the shapes above.
//
// It works by dropping rather than by creating, because the shipped schema is
// the widest one and every candidate is a subset of it. That means the trimmed
// shape is reached by exactly the statements the v3 migration runs, so what is
// timed here is what would ship.
func applyShape(tb testing.TB, db *sql.DB, shape string) {
	tb.Helper()
	var drop []string
	switch shape {
	case shapeBaseline:
		drop = append(drop, v2Indexes...)
	case shapeTrimmed:
		drop = append(drop, prefixIndexes...)
	case shapeV2:
	default:
		tb.Fatalf("unknown index shape %q", shape)
	}
	for _, name := range drop {
		if _, err := db.Exec("DROP INDEX IF EXISTS " + name); err != nil {
			tb.Fatalf("drop %s: %v", name, err)
		}
	}
}

// indexNames lists the named indexes currently on a table.
func indexNames(tb testing.TB, db *sql.DB, table string) []string {
	tb.Helper()
	rows, err := db.Query(
		"SELECT name FROM sqlite_master WHERE type='index' AND tbl_name = ? AND sql IS NOT NULL ORDER BY name", table)
	if err != nil {
		tb.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			tb.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		tb.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------- corpus

const (
	benchSessions = 200
	benchCalls    = 120
	benchTools    = 120
	benchModels   = 8
	benchProjects = 6
	benchAgents   = 3
)

var benchModelNames = []string{
	"claude-opus-4", "claude-sonnet-4", "claude-haiku-4", "gpt-5",
	"gpt-5-mini", "o3", "gemini-2.5-pro", "qwen3-coder",
}

var benchBase = time.Date(2026, 3, 15, 9, 0, 0, 0, time.Local)

// benchCorpus is the corpus every measurement here runs against: 200 sessions,
// 24k calls and 24k tool calls, spread over enough distinct models, projects and
// agents that every rollup has something to group.
func benchCorpus() []model.SessionWrite {
	out := make([]model.SessionWrite, 0, benchSessions)
	for s := 0; s < benchSessions; s++ {
		uid := fmt.Sprintf("bench-%04d", s)
		agent := benchModelNames[s%benchAgents]
		project := fmt.Sprintf("/p%d", s%benchProjects)
		sess := model.SessionWrite{UID: uid, Agent: agent, Project: project,
			Path: filepath.Join("/tmp/logs", uid+".jsonl"), Title: uid}
		for c := 0; c < benchCalls; c++ {
			at := benchBase.Add(time.Duration(s*benchCalls+c) * time.Second)
			sess.Calls = append(sess.Calls, model.Call{
				SessionUID: uid, CallKey: fmt.Sprintf("c%05d", c),
				Agent: agent, Project: project,
				Model:       benchModelNames[(s*7+c)%benchModels],
				Time:        at,
				InputTokens: 800 + c%400, OutputTokens: 120 + c%60,
				CacheReadTokens: 4000, CacheWriteTokens: 200,
				ReasoningTokens: c % 50, TotalTokens: 6000,
				LLMSeconds: 1.5, CostUSD: 0.0125, Priced: true,
			})
		}
		for t := 0; t < benchTools; t++ {
			at := benchBase.Add(time.Duration(s*benchCalls+t)*time.Second + 500*time.Millisecond)
			sess.ToolCalls = append(sess.ToolCalls, model.ToolCall{
				SessionUID: uid, CallKey: fmt.Sprintf("t%05d", t),
				Agent: agent, Project: project,
				Tool: fmt.Sprintf("tool%02d", t%25), Time: at,
				Seconds: 0.2, IsError: t%37 == 0,
			})
		}
		out = append(out, sess)
	}
	return out
}

// newBenchStore opens a database at a fresh path with the given index shape.
func newBenchStore(tb testing.TB, shape string) (*Store, []model.SessionWrite) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "bench.db")
	st, err := Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { st.Close() })
	applyShape(tb, st.db, shape)
	return st, benchCorpus()
}

// newLoadedBenchStore is newBenchStore with the corpus already written, which is
// what every read measurement needs.
func newLoadedBenchStore(tb testing.TB, shape string) (*Store, []model.SessionWrite) {
	tb.Helper()
	st, corpus := newBenchStore(tb, shape)
	ingest(tb, st, corpus)
	return st, corpus
}

// ingest writes a corpus through the real ingestion API.
func ingest(tb testing.TB, st *Store, corpus []model.SessionWrite) {
	tb.Helper()
	for _, sess := range corpus {
		if err := st.ReplaceSession(sess); err != nil {
			tb.Fatal(err)
		}
		if err := st.RecomputeSession(sess.UID); err != nil {
			tb.Fatal(err)
		}
	}
}

// seedSessions adds n summary-only sessions, so a benchmark can put the session
// table at a size of its choosing.
func seedSessions(tb testing.TB, st *Store, n int) {
	tb.Helper()
	if n <= 0 {
		return
	}
	if err := st.InTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			`INSERT INTO session (uid, agent, project, path, title, first_ts, last_ts, calls)
			 VALUES (?, 'pi', '/p', '/tmp/x', 'x', 0, ?, 1)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < n; i++ {
			if _, err := stmt.Exec("extra-"+pad(i), i); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		tb.Fatal(err)
	}
}

// ---------------------------------------------------------------- write cost

// BenchmarkIngest measures what an index set costs on the write side: the whole
// corpus, written once, through the API the scanner uses.
//
// This is the cost that is paid constantly — the scanner runs every 30 seconds
// by default — set against rollup cost, which is paid once per page load.
func BenchmarkIngest(b *testing.B) {
	for _, shape := range shapes {
		b.Run(shape, func(b *testing.B) {
			corpus := benchCorpus()
			dir := b.TempDir()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				path := filepath.Join(dir, fmt.Sprintf("%d.db", i))
				b.StopTimer()
				st, err := Open(path)
				if err != nil {
					b.Fatal(err)
				}
				applyShape(b, st.db, shape)
				b.StartTimer()

				ingest(b, st, corpus)

				b.StopTimer()
				st.Close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkAppend measures the steady-state write, which is what the 30-second
// scan actually does: a handful of new calls into an already-populated
// database. Every iteration appends a session that has not been written before,
// so both the upsert path and the index maintenance are real work.
func BenchmarkAppend(b *testing.B) {
	for _, shape := range shapes {
		b.Run(shape, func(b *testing.B) {
			st, _ := newLoadedBenchStore(b, shape)
			base := time.Date(2026, 3, 20, 9, 0, 0, 0, time.Local)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				uid := fmt.Sprintf("append-%08d", i)
				project := fmt.Sprintf("/p%d", i%benchProjects)
				sess := model.SessionWrite{UID: uid, Agent: "pi", Project: project}
				for c := 0; c < 10; c++ {
					sess.Calls = append(sess.Calls, model.Call{
						SessionUID: uid, CallKey: fmt.Sprintf("c%d", c),
						Agent: "pi", Project: project,
						Model:       benchModelNames[c%benchModels],
						Time:        base.Add(time.Duration(c) * time.Second),
						InputTokens: 800, OutputTokens: 120, TotalTokens: 6000,
						LLMSeconds: 1.5, CostUSD: 0.0125, Priced: true,
					})
					sess.ToolCalls = append(sess.ToolCalls, model.ToolCall{
						SessionUID: uid, CallKey: fmt.Sprintf("t%d", c),
						Agent: "pi", Project: project, Tool: "bash",
						Time:    base.Add(time.Duration(c)*time.Second + 500*time.Millisecond),
						Seconds: 0.2,
					})
				}
				if err := st.AppendSession(sess); err != nil {
					b.Fatal(err)
				}
				if err := st.RecomputeSession(uid); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- read cost

// BenchmarkRollup measures the four rollups the dashboard runs on every page
// load, under each index shape, using the SQL the page runs.
func BenchmarkRollup(b *testing.B) {
	rollups := []struct {
		name string
		run  func(*Store) error
	}{
		{"daily", func(st *Store) error {
			_, err := st.Daily(context.Background(), Filter{})
			return err
		}},
		{"models", func(st *Store) error {
			_, err := st.Models(context.Background(), Filter{})
			return err
		}},
		{"projectmodels", func(st *Store) error {
			_, err := st.ProjectModels(context.Background(), Filter{})
			return err
		}},
		{"tools", func(st *Store) error {
			_, err := st.Tools(context.Background(), Filter{})
			return err
		}},
	}
	for _, r := range rollups {
		r := r
		for _, shape := range shapes {
			b.Run(r.name+"/"+shape, func(b *testing.B) {
				st, _ := newLoadedBenchStore(b, shape)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := r.run(st); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkPageLoad measures one dashboard page load end to end: the totals,
// the facets, every rollup and the session listing, which is what handleIndex in
// the web layer issues.
//
// It is the number that has to be weighed against ingest, because a page load
// is the thing a human notices and a scan every 30 seconds is not.
func BenchmarkPageLoad(b *testing.B) {
	for _, shape := range shapes {
		b.Run(shape, func(b *testing.B) {
			st, _ := newLoadedBenchStore(b, shape)
			now := time.Date(2026, 3, 16, 9, 0, 0, 0, time.Local)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := st.Totals(ctx, Filter{}); err != nil {
					b.Fatal(err)
				}
				if _, _, _, _, _, err := st.Facets(ctx); err != nil {
					b.Fatal(err)
				}
				if _, err := st.Daily(ctx, Filter{}); err != nil {
					b.Fatal(err)
				}
				if _, err := st.Activity(ctx, Filter{}, now.Add(-24*time.Hour), now, 3600); err != nil {
					b.Fatal(err)
				}
				if _, err := st.Models(ctx, Filter{}); err != nil {
					b.Fatal(err)
				}
				if _, err := st.ProjectModels(ctx, Filter{}); err != nil {
					b.Fatal(err)
				}
				if _, err := st.Tools(ctx, Filter{}); err != nil {
					b.Fatal(err)
				}
				if _, err := st.ProjectTools(ctx, Filter{}); err != nil {
					b.Fatal(err)
				}
				if _, err := st.Projects(ctx, Filter{}); err != nil {
					b.Fatal(err)
				}
				if _, err := st.Sessions(ctx, Filter{}, 5000); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSessionLookup measures the single-session page, which is where the
// Session lookup is on the path, at two database sizes.
//
// The session count is the point: the lookup this replaced read the listing of
// up to 5000 sessions on every visit to a single session's page, so its cost
// tracked the size of the database rather than the size of the answer. The
// `old-listing-lookup` variant reproduces that shape against the same data, so
// the two differ only in how they find the row.
func BenchmarkSessionLookup(b *testing.B) {
	for _, n := range []int{200, 20000} {
		for _, shape := range shapes {
			b.Run(fmt.Sprintf("sessions=%d/%s", n, shape), func(b *testing.B) {
				st, corpus := newLoadedBenchStore(b, shape)
				seeded := n - len(corpus) + 1
				seedSessions(b, st, seeded)
				ctx := context.Background()
				uids := []string{corpus[0].UID, "extra-" + pad(seeded-1)}
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					row, ok, err := st.Session(ctx, uids[i%len(uids)])
					if err != nil || !ok {
						b.Fatalf("Session: %v ok=%v", err, ok)
					}
					if _, err := st.SessionCalls(ctx, row.UID); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run(fmt.Sprintf("sessions=%d/%s/old-listing-lookup", n, shape), func(b *testing.B) {
				st, corpus := newLoadedBenchStore(b, shape)
				seeded := n - len(corpus) + 1
				seedSessions(b, st, seeded)
				ctx := context.Background()
				uid := "extra-" + pad(seeded-1)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					rows, err := st.Sessions(ctx, Filter{}, 0)
					if err != nil {
						b.Fatal(err)
					}
					var found bool
					for _, r := range rows {
						if r.UID == uid {
							found = true
							break
						}
					}
					if !found {
						b.Fatal("session not found")
					}
					if _, err := st.SessionCalls(ctx, uid); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// ---------------------------------------------------------------- the numbers

// TestIndexShapes reports the decision inputs: database size and the plan
// chosen for every statement the read path issues, under each shape.
//
// It is a test rather than only a benchmark so the numbers land in `go test -v`
// output, where they can be compared against a later change instead of being
// rediscovered by hand.
func TestIndexShapes(t *testing.T) {
	sizes := map[string]int64{}
	indexes := map[string][]string{}
	plans := map[string][]string{}
	for _, shape := range shapes {
		st, _ := newLoadedBenchStore(t, shape)
		if _, err := st.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
		sizes[shape] = dbBytes(t, st.db)
		indexes[shape] = indexNames(t, st.db, "call")
		plans[shape] = plansOf(t, st.db)
	}

	for _, shape := range shapes {
		t.Logf("shape %-9s call indexes: %v", shape, indexes[shape])
		t.Logf("shape %-9s database %s", shape, humanBytes(sizes[shape]))
		if base := sizes[shapeBaseline]; base > 0 {
			t.Logf("shape %-9s database %+.0f%% vs baseline", shape,
				100*float64(sizes[shape]-base)/float64(base))
		}
		for _, line := range plans[shape] {
			t.Logf("  [%s] %s", shape, line)
		}
	}
}

// readQuery is one statement the read path issues, with the arguments it is
// issued with.
type readQuery struct {
	name  string
	query string
	args  []any
}

// readQueries is every statement the dashboard's read path issues against `call`,
// `tool_call` and `session`, in one place.
//
// It exists so that "dropping these indexes changes no plan for the worse" can
// be a claim about the whole read surface rather than about the four queries
// someone remembered to check. A query missing from this list is a gap in the
// evidence, not a passing test.
func readQueries() []readQuery {
	empty := Filter{}
	where, args := empty.where("", "ts")
	twhere, targs := empty.where("t", "ts")
	swhere, sargs := empty.whereSession("s")
	return []readQuery{
		{"totals", `
			SELECT COALESCE(SUM(cost_usd),0), COUNT(*),
			       COALESCE(SUM(total_tokens),0), COALESCE(SUM(input_tokens),0),
			       COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			       COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(reasoning_tokens),0),
			       COALESCE(SUM(llm_seconds),0),
			       COALESCE(SUM(CASE WHEN priced = 0 THEN 1 ELSE 0 END),0),
			       COALESCE(MIN(NULLIF(ts,0)),0), COALESCE(MAX(ts),0)
			FROM call` + where, args},
		{"tool_seconds", `SELECT SUM(t.seconds) FROM tool_call t` + twhere, targs},
		{"daily", dailySQL(where), args},
		{"activity", `
			SELECT (ts / ?) * ?, COUNT(*), COALESCE(SUM(input_tokens),0),
			       COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			       COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(reasoning_tokens),0),
			       COALESCE(SUM(total_tokens),0), COALESCE(SUM(llm_seconds),0),
			       COALESCE(SUM(cost_usd),0),
			       COALESCE(SUM(CASE WHEN priced = 0 THEN 1 ELSE 0 END),0)
			FROM call` + where + ` GROUP BY 1 ORDER BY 1`, append([]any{60, 60}, args...)},
		{"history", `SELECT MIN(NULLIF(ts,0)), MAX(ts) FROM call` + where, args},
		{"models", modelsSQL(where), args},
		{"project_models", projectModelsSQL(where), args},
		{"tools", toolsSQL(twhere), targs},
		{"project_tools", `
			SELECT t.project, t.tool, COUNT(*), COALESCE(SUM(t.seconds),0),
			       COALESCE(SUM(t.is_error),0)
			FROM tool_call t` + twhere + ` GROUP BY t.project, t.tool`, targs},
		{"projects", `
			SELECT s.project, s.agent, COALESCE(SUM(s.cost_usd),0),
			       COALESCE(SUM(s.calls),0), COUNT(*),
			       COALESCE(MIN(NULLIF(s.first_ts,0)),0), COALESCE(MAX(s.last_ts),0)
			FROM session s` + swhere + ` GROUP BY s.project, s.agent`, sargs},
		{"sessions", `
			SELECT ` + sessionColumns + `
			FROM session s` + swhere + ` ORDER BY s.last_ts DESC LIMIT ?`,
			append(append([]any{}, sargs...), 5000)},
		{"session_seek", `SELECT ` + sessionColumns + ` FROM session WHERE uid = ?`,
			[]any{"bench-0000"}},
		{"totals_sessions", `
			SELECT COUNT(*), COUNT(DISTINCT project), COUNT(DISTINCT agent)
			FROM session s` + swhere, sargs},
		{"facet_model", `SELECT DISTINCT model FROM call WHERE model != '' ORDER BY model`, nil},
		{"facet_agent", `SELECT DISTINCT agent FROM call WHERE agent != '' ORDER BY agent`, nil},
		{"facet_project", `SELECT DISTINCT project FROM call WHERE project != '' ORDER BY project`, nil},
		{"session_calls", `
			SELECT ts, model, input_tokens, output_tokens, cache_read_tokens,
			       cache_write_tokens, reasoning_tokens, total_tokens, llm_seconds,
			       cost_usd, priced
			FROM call WHERE session_uid = ? ORDER BY ts, call_key`, []any{"bench-0000"}},
		{"recompute_calls", `
			SELECT COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_usd),0)
			FROM call WHERE session_uid = ?`, []any{"bench-0000"}},
	}
}

// planOf returns the EXPLAIN QUERY PLAN rows for a statement.
func planOf(t testing.TB, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
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
	return out
}

// plansOf runs every read query through EXPLAIN QUERY PLAN and returns one
// "name: plan" line each, in the order readQueries lists them.
func plansOf(tb testing.TB, db *sql.DB) []string {
	tb.Helper()
	var out []string
	for _, q := range readQueries() {
		out = append(out, q.name+": "+strings.Join(planOf(tb, db, q.query, q.args...), " | "))
	}
	return out
}

// dbBytes is the size of the database as SQLite accounts for it, which is the
// number that matters for a dashboard that keeps every call forever.
func dbBytes(t testing.TB, db *sql.DB) int64 {
	t.Helper()
	var pages, pageSize int64
	if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	return pages * pageSize
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f MiB", float64(n)/float64(div))
}
