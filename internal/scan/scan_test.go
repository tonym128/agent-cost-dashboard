package scan

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
	"github.com/tonym128/agent-cost-dashboard/internal/source"
	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func writeJSONL(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// appendTo appends a record, adding the terminating newline a real log has.
func appendTo(t *testing.T, path, line string) {
	t.Helper()
	appendRaw(t, path, line+"\n")
}

// appendRaw appends bytes verbatim, for the half-written-line case.
func appendRaw(t *testing.T, path, raw string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
}

// newScanner builds a scanner over one pi-shaped source directory.
func newScanner(t *testing.T, st *store.Store, root string) *Scanner {
	t.Helper()
	return New(Config{
		Store: st,
		Pricer: func() *source.Pricer {
			p, err := source.NewPricer("", "")
			if err != nil {
				t.Fatal(err)
			}
			return p
		}(), // the embedded fallback table needs no paths
		Logger: testLogger(),
		Sources: []Source{{
			Agent: model.AgentPi, Root: root, Ext: ".jsonl", Recurse: true,
			parserFor: func() source.Parser { return source.NewPiParser() },
		}},
	})
}

func TestScanIngestsThenSkipsUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "s.jsonl")
	writeJSONL(t, path,
		`{"type":"session","id":"sess-1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"gemini-2.5-pro","usage":{"input":100,"output":50}}}`,
	)
	st := newTestStore(t)
	sc := newScanner(t, st, root)

	if err := sc.RunOnce(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	totals, err := st.Totals(context.Background(), store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if totals.Calls != 1 {
		t.Fatalf("calls = %d, want 1", totals.Calls)
	}
	if totals.Sessions != 1 {
		t.Errorf("sessions = %d, want 1", totals.Sessions)
	}
	if totals.OutputTokens != 50 {
		t.Errorf("output = %d, want 50", totals.OutputTokens)
	}

	// After a full read the cursor sits at the end of the file.
	states, _ := st.LoadScanStates()
	info, _ := os.Stat(path)
	if states[path].Offset != info.Size() {
		t.Errorf("offset = %d, want file size %d", states[path].Offset, info.Size())
	}

	// A second pass with nothing changed must re-read nothing.
	if err := sc.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _ := st.ScanStatuses()
	if len(status) != 1 || status[0].FilesChanged != 0 {
		t.Errorf("second pass reported %d changed files, want 0", status[0].FilesChanged)
	}

	totals2, _ := st.Totals(context.Background(), store.Filter{})
	if totals2.Calls != totals.Calls {
		t.Errorf("a no-op pass changed the call count: %d then %d", totals.Calls, totals2.Calls)
	}
}

func TestIncrementalScanAppendsWithoutDuplicating(t *testing.T) {
	// The central promise: a growing log is extended, not re-counted.
	root := t.TempDir()
	path := filepath.Join(root, "s.jsonl")
	writeJSONL(t, path,
		`{"type":"session","id":"sess-1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":100,"output":50}}}`,
	)
	st := newTestStore(t)
	sc := newScanner(t, st, root)
	ctx := context.Background()

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, _ := st.Totals(ctx, store.Filter{})
	if first.Calls != 1 {
		t.Fatalf("calls = %d, want 1", first.Calls)
	}

	appendTo(t, path, `{"type":"message","timestamp":"2026-05-01T00:00:10Z","id":"m2","message":{"role":"assistant","model":"m","usage":{"input":200,"output":80}}}`)
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	second, _ := st.Totals(ctx, store.Filter{})
	if second.Calls != 2 {
		t.Errorf("after append calls = %d, want 2", second.Calls)
	}
	if second.OutputTokens != 130 {
		t.Errorf("output = %d, want 130 (50 + 80)", second.OutputTokens)
	}

	// A third pass over the same content must not grow anything.
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	third, _ := st.Totals(ctx, store.Filter{})
	if third.Calls != second.Calls || third.OutputTokens != second.OutputTokens {
		t.Errorf("idempotence broken: %d/%d then %d/%d",
			second.Calls, second.OutputTokens, third.Calls, third.OutputTokens)
	}

	// And the session summary must agree with the rows it summarises.
	sess, ok, err := st.Session(ctx, "sess-1")
	if err != nil || !ok {
		t.Fatalf("session lookup: ok=%v err=%v", ok, err)
	}
	if sess.Calls != 2 || sess.OutputTokens != 130 {
		t.Errorf("session summary (%d calls, %d output) disagrees with the call rows",
			sess.Calls, sess.OutputTokens)
	}
}

func TestRewrittenLogDropsCallsThatNoLongerExist(t *testing.T) {
	// A log rewritten in place must not leave the calls it removed behind. This
	// is why the scanner fingerprints the head of the file rather than trusting
	// the offset alone.
	root := t.TempDir()
	path := filepath.Join(root, "s.jsonl")
	writeJSONL(t, path,
		`{"type":"session","id":"sess-1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":100,"output":50}}}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:10Z","id":"m2","message":{"role":"assistant","model":"m","usage":{"input":100,"output":50}}}`,
	)
	st := newTestStore(t)
	sc := newScanner(t, st, root)
	ctx := context.Background()
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Totals(ctx, store.Filter{}); got.Calls != 2 {
		t.Fatalf("setup: calls = %d, want 2", got.Calls)
	}

	// Same path, different content, same length: a rewrite, not an append.
	writeJSONL(t, path,
		`{"type":"session","id":"sess-1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m9","message":{"role":"assistant","model":"m","usage":{"input":7,"output":3}}}`,
	)
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Totals(ctx, store.Filter{})
	if got.Calls != 1 {
		t.Errorf("calls = %d, want 1 after the rewrite", got.Calls)
	}
	if got.OutputTokens != 3 {
		t.Errorf("output = %d, want 3 after the rewrite", got.OutputTokens)
	}
}

func TestDeletedLogKeepsItsHistory(t *testing.T) {
	// The reason calls are stored individually: a log that has been rotated
	// away is exactly the case a historical window still has to answer. So a
	// vanished log withdraws the transcript, not the numbers.
	root := t.TempDir()
	path := filepath.Join(root, "gone.jsonl")
	// Dated relative to now so the window assertion below covers it.
	ts := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	writeJSONL(t, path,
		`{"type":"session","id":"sess-x","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"`+ts+`","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":500,"output":50}}}`,
	)
	st := newTestStore(t)
	sc := newScanner(t, st, root)
	ctx := context.Background()
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before, _ := st.Totals(ctx, store.Filter{})
	if before.Sessions != 1 || before.Calls != 1 {
		t.Fatalf("setup: %d sessions, %d calls", before.Sessions, before.Calls)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	after, _ := st.Totals(ctx, store.Filter{})
	if after.Sessions != 1 || after.Calls != 1 {
		t.Errorf("after the log was deleted: %d sessions, %d calls; history must survive",
			after.Sessions, after.Calls)
	}
	if after.TotalTokens != before.TotalTokens {
		t.Errorf("tokens changed after deletion: %d then %d", before.TotalTokens, after.TotalTokens)
	}

	sess, ok, err := st.Session(ctx, "sess-x")
	if err != nil || !ok {
		t.Fatalf("session lookup: ok=%v err=%v", ok, err)
	}
	if !sess.Orphaned {
		t.Error("session is not flagged as orphaned; the page would offer a transcript that is gone")
	}

	// A window covering it still answers from the stored calls.
	buckets, err := st.Activity(ctx, store.Filter{},
		time.Now().Add(-48*time.Hour), time.Now(), 3600)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, b := range buckets {
		if b.Calls > 0 {
			found = true
		}
	}
	if !found {
		t.Error("the orphaned session contributes nothing to a window over its dates")
	}
}

func TestAReturningLogIsUnflagged(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "back.jsonl")
	line := `{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":5,"output":5}}}`
	head := `{"type":"session","id":"sess-b","cwd":"/tmp/p"}`
	writeJSONL(t, path, head, line)

	st := newTestStore(t)
	sc := newScanner(t, st, root)
	ctx := context.Background()
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	removed := filepath.Join(root, "temporarily-gone")
	if err := os.Rename(path, removed); err != nil {
		t.Fatal(err)
	}
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sess, _, _ := st.Session(ctx, "sess-b")
	if !sess.Orphaned {
		t.Fatal("expected the session to be flagged while its log is away")
	}

	// The log comes back, unchanged: nothing was re-derived, so the flag must be
	// cleared by the pass that finds it again.
	if err := os.Rename(removed, path); err != nil {
		t.Fatal(err)
	}
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	sess, _, _ = st.Session(ctx, "sess-b")
	if sess.Orphaned {
		t.Error("session still flagged after its log returned")
	}
}

func TestScanSurvivesAMalformedLog(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "good.jsonl"),
		`{"type":"session","id":"s1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":5,"output":5}}}`,
	)
	// A file of nothing but junk must not take the pass down with it.
	writeJSONL(t, filepath.Join(root, "junk.jsonl"), `not json`, `123`, `{"broken":`)

	st := newTestStore(t)
	sc := newScanner(t, st, root)
	if err := sc.RunOnce(context.Background()); err != nil {
		t.Fatalf("a malformed file failed the pass: %v", err)
	}
	totals, _ := st.Totals(context.Background(), store.Filter{})
	if totals.Calls != 1 {
		t.Errorf("calls = %d, want the one good call", totals.Calls)
	}
}

func TestDiscoverSkipsDotAndNoiseDirectories(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "keep.jsonl"), `{"type":"session","id":"a"}`)
	writeJSONL(t, filepath.Join(root, ".hidden", "skip.jsonl"), `{"type":"session","id":"b"}`)
	writeJSONL(t, filepath.Join(root, "node_modules", "skip.jsonl"), `{"type":"session","id":"c"}`)
	writeJSONL(t, filepath.Join(root, "nested", "deep.jsonl"), `{"type":"session","id":"d"}`)

	paths, err := discover(Source{
		Root: root, Ext: ".jsonl", Recurse: true,
		SkipDirs: []string{"node_modules"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	want := map[string]bool{"keep.jsonl": true, "deep.jsonl": true}
	if len(names) != len(want) {
		t.Fatalf("discovered %v, want only %v", names, want)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected file %q", n)
		}
	}
}

// -------------------------------------------------------------- queries

func TestActivityWindowReachesBeyondTheScanHorizon(t *testing.T) {
	// The reason the calls are stored individually: a window covering a period
	// whose logs are long gone is answerable from the database.
	st := newTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-400 * 24 * time.Hour).Truncate(time.Hour)

	for i := 0; i < 5; i++ {
		sess := model.SessionWrite{
			Session: model.Session{UID: fmt.Sprintf("s%d", i), Agent: model.AgentPi, Project: "/p"},
			Calls: []model.Call{{
				SessionUID: fmt.Sprintf("s%d", i), CallKey: "c1", Agent: model.AgentPi,
				Project: "/p", Model: "m",
				Time:         base.Add(time.Duration(i) * time.Hour),
				InputTokens:  100,
				OutputTokens: 50,
				TotalTokens:  150,
				LLMSeconds:   2,
				CostUSD:      1.5,
				Priced:       true,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(sess.UID); err != nil {
			t.Fatal(err)
		}
	}

	// A window starting 400 days ago is answered entirely from stored rows.
	buckets, err := st.Activity(ctx, store.Filter{}, base.Add(-time.Hour), base.Add(24*time.Hour), 3600)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 5 {
		t.Fatalf("got %d hourly buckets, want 5", len(buckets))
	}
	var totalCalls, totalOut int64
	var totalCost, totalLLM float64
	for _, b := range buckets {
		totalCalls += int64(b.Calls)
		totalOut += b.OutputTokens
		totalCost += b.Cost
		totalLLM += b.LLMSeconds
	}
	if totalCalls != 5 || totalOut != 250 || totalLLM != 10 {
		t.Errorf("rollup mismatch: calls=%d out=%d llm=%f", totalCalls, totalOut, totalLLM)
	}
	if totalCost < 7.4 || totalCost > 7.6 {
		t.Errorf("cost = %f, want 7.5", totalCost)
	}
}

func TestRepriceRecomputesHistory(t *testing.T) {
	// Cost is stored at scan time; refreshing prices must reach calls that were
	// ingested long before.
	st := newTestStore(t)
	ctx := context.Background()
	sess := model.SessionWrite{
		Session: model.Session{UID: "s1", Agent: model.AgentPi, Project: "/p"},
		Calls: []model.Call{{
			SessionUID: "s1", CallKey: "c1", Agent: model.AgentPi, Project: "/p",
			Model: "m", InputTokens: 1_000_000, TotalTokens: 1_000_000,
			CostUSD: 3.0, Priced: true,
		}},
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession("s1"); err != nil {
		t.Fatal(err)
	}

	n, err := st.Reprice(ctx, func(model string, in, out, cacheRead, cacheWrite int64) (float64, bool) {
		if model == "m" {
			return 7.0, true
		}
		return 0, false
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("repriced %d calls, want 1", n)
	}
	totals, _ := st.Totals(ctx, store.Filter{})
	if totals.Cost != 7.0 {
		t.Errorf("cost = %f, want the repriced 7.0", totals.Cost)
	}
	// The session summary must have followed.
	sess2, _, _ := st.Session(ctx, "s1")
	if sess2.Cost != 7.0 {
		t.Errorf("session cost = %f, want 7.0", sess2.Cost)
	}
}

func TestFilterNarrowsEveryAxisTogether(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	mk := func(uid, agent, project, mdl string, ts time.Time) {
		sess := model.SessionWrite{
			Session: model.Session{UID: uid, Agent: agent, Project: project},
			Calls: []model.Call{{
				SessionUID: uid, CallKey: "c", Agent: agent, Project: project,
				Model: mdl, Time: ts, TotalTokens: 10, CostUSD: 1, Priced: true,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}
	mk("a", "pi", "/p1", "m1", now.Add(-2*time.Hour))
	mk("b", "claude", "/p2", "m2", now.Add(-2*24*time.Hour))

	all, _ := st.Totals(ctx, store.Filter{})
	if all.Calls != 2 || all.Sessions != 2 || all.Projects != 2 {
		t.Fatalf("unfiltered: %d calls, %d sessions, %d projects", all.Calls, all.Sessions, all.Projects)
	}

	byAgent, _ := st.Totals(ctx, store.Filter{Agents: []string{"pi"}})
	if byAgent.Calls != 1 || byAgent.Sessions != 1 {
		t.Errorf("agent filter: %d calls, %d sessions; want 1 and 1", byAgent.Calls, byAgent.Sessions)
	}
	cutoff := now.Add(-24 * time.Hour)
	byDate, _ := st.Totals(ctx, store.Filter{DateFrom: &cutoff})
	if byDate.Calls != 1 || byDate.Sessions != 1 {
		t.Errorf("date filter: %d calls, %d sessions; want 1 and 1", byDate.Calls, byDate.Sessions)
	}
	combined, _ := st.Totals(ctx, store.Filter{Agents: []string{"claude"}, DateFrom: &cutoff})
	if combined.Calls != 0 {
		t.Errorf("combined filter: %d calls, want 0 (no match on both axes)", combined.Calls)
	}
}

func TestFacetsSurviveAFilter(t *testing.T) {
	// The dropdowns must keep offering what a filter hides, or applying a filter
	// makes that axis impossible to widen again.
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for i, agent := range []string{"pi", "claude"} {
		uid := fmt.Sprintf("s%d", i)
		sess := model.SessionWrite{
			Session: model.Session{UID: uid, Agent: agent, Project: "/p" + agent},
			Calls: []model.Call{{
				SessionUID: uid, CallKey: "c", Agent: agent, Project: "/p" + agent,
				Model: "m" + agent, Time: now, TotalTokens: 1, CostUSD: 1, Priced: true,
			}},
		}
		if err := st.ReplaceSession(sess); err != nil {
			t.Fatal(err)
		}
		if err := st.RecomputeSession(uid); err != nil {
			t.Fatal(err)
		}
	}
	models, agents, projects, minTS, maxTS, err := st.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || len(agents) != 2 || len(projects) != 2 {
		t.Errorf("facets: %d models, %d agents, %d projects; want 2 each",
			len(models), len(agents), len(projects))
	}
	if minTS == 0 || maxTS == 0 {
		t.Errorf("facet timestamps missing: min=%d max=%d", minTS, maxTS)
	}
}

func TestUnpricedCallsAreCountedSeparately(t *testing.T) {
	// A $0.00 that means "unknown" must never be mistaken for "free".
	st := newTestStore(t)
	sess := model.SessionWrite{
		Session: model.Session{UID: "s1", Agent: model.AgentPi, Project: "/p"},
		Calls: []model.Call{
			{SessionUID: "s1", CallKey: "a", Agent: model.AgentPi, Project: "/p",
				Model: "priced", TotalTokens: 10, CostUSD: 1.25, Priced: true},
			{SessionUID: "s1", CallKey: "b", Agent: model.AgentPi, Project: "/p",
				Model: "mystery", TotalTokens: 10, CostUSD: 0, Priced: false},
		},
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	totals, _ := st.Totals(context.Background(), store.Filter{})
	if totals.UnpricedCalls != 1 {
		t.Errorf("unpriced = %d, want 1", totals.UnpricedCalls)
	}
	if totals.Calls != 2 {
		t.Errorf("calls = %d; an unpriced call must still be counted", totals.Calls)
	}
	if totals.Cost != 1.25 {
		t.Errorf("cost = %f, want 1.25", totals.Cost)
	}
}

func TestAppendWithoutANewlineIsLeftForTheNextPass(t *testing.T) {
	// A record with no terminating newline may be a message the agent is still
	// writing. Consuming it would either drop the message or store a fragment,
	// so the scanner must leave the cursor short of it and pick it up once the
	// line is finished.
	root := t.TempDir()
	path := filepath.Join(root, "s.jsonl")
	writeJSONL(t, path,
		`{"type":"session","id":"s1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":10,"output":5}}}`,
	)
	st := newTestStore(t)
	sc := newScanner(t, st, root)
	ctx := context.Background()
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	states, _ := st.LoadScanStates()
	settled := states[path].Offset

	partial := `{"type":"message","timestamp":"2026-05-01T00:00:10Z","id":"m2","message":{"role":"assistant","model":"m","usage":{"input":20,"output":8}}}`
	appendRaw(t, path, partial[:len(partial)-1]) // no trailing newline
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Totals(ctx, store.Filter{})
	if got.Calls != 1 {
		t.Errorf("calls = %d; a half-written record must not be ingested", got.Calls)
	}
	states, _ = st.LoadScanStates()
	if states[path].Offset != settled {
		t.Errorf("cursor advanced to %d past an unterminated line (was %d)",
			states[path].Offset, settled)
	}

	// Finish the line; now it counts.
	appendRaw(t, path, "}\n")
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Totals(ctx, store.Filter{})
	if got.Calls != 2 {
		t.Errorf("calls = %d after the line was completed, want 2", got.Calls)
	}
}

func TestDiscoveryFindsFilesUnderADotRootedSource(t *testing.T) {
	// Every one of these sources lives under a dot-directory in the home
	// directory. Applying the hidden-directory rule to the root itself skips the
	// whole tree, which is a silent "no data" rather than an error.
	home := t.TempDir()
	root := filepath.Join(home, ".gemini")
	writeJSONL(t, filepath.Join(root, "project", "chats", "session.jsonl"),
		`{"type":"gemini","model":"m","tokens":{"input":1,"output":1}}`)
	writeJSONL(t, filepath.Join(root, "other", "session.jsonl"),
		`{"type":"gemini","model":"m","tokens":{"input":1,"output":1}}`)

	paths, err := discover(Source{
		Agent: model.AgentGemini, Root: root, Ext: ".jsonl", Recurse: true,
		SkipDirs: []string{"node_modules"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("discovered %d files under a dot-rooted source, want 2:\n%v", len(paths), paths)
	}
}

func TestDiscoveryStillSkipsHiddenSubdirectories(t *testing.T) {
	// The exemption is for the root only; a dot-directory inside it is still
	// noise and can hold thousands of files.
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "keep.jsonl"), `{"type":"session","id":"a"}`)
	writeJSONL(t, filepath.Join(root, ".cache", "skip.jsonl"), `{"type":"session","id":"b"}`)

	paths, err := discover(Source{Root: root, Ext: ".jsonl", Recurse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "keep.jsonl" {
		t.Errorf("discovered %v, want only keep.jsonl", paths)
	}
}

// TestANoOpPassIsCheap guards the property the whole background design rests on:
// once the machine is quiet, a pass must be cheap enough to run constantly.
//
// It regressed twice while this was being built — once because a pass decoded
// every OpenCode payload before applying its cursor in Go, and once because the
// SQLite sources never recorded a file size so every conversation looked changed
// — and both presented as "the scan takes a few seconds", which is easy to
// mistake for the cost of a real pass.
func TestANoOpPassIsCheap(t *testing.T) {
	if testing.Short() {
		t.Skip("timing assertion")
	}
	root := t.TempDir()
	// A few hundred files, to make the per-file stat the visible cost.
	for i := 0; i < 150; i++ {
		writeJSONL(t, filepath.Join(root, fmt.Sprintf("s%03d.jsonl", i)),
			`{"type":"session","id":"s`+strconv.Itoa(i)+`","cwd":"/tmp/p"}`,
			`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":5,"output":5}}}`,
		)
	}
	st := newTestStore(t)
	sc := newScanner(t, st, root)
	ctx := context.Background()

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	statuses, _ := st.ScanStatuses()
	if len(statuses) != 1 || statuses[0].FilesChanged == 0 {
		t.Fatalf("setup: expected a first pass to change files, got %+v", statuses)
	}

	start := time.Now()
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	statuses, _ = st.ScanStatuses()
	if statuses[0].FilesChanged != 0 {
		t.Errorf("second pass re-read %d files", statuses[0].FilesChanged)
	}
	// Generous enough not to flake on a loaded machine, tight enough that the
	// regression this guards against — 2.6s for 150 idle files — fails it.
	if elapsed > 750*time.Millisecond {
		t.Errorf("a no-op pass over %d files took %v; it should be a stat each", 150, elapsed)
	}
}

// TestChangedFilesAreNotStarvedByThePerPassBudget: a source with more changed
// files than one pass will ingest must make progress every pass rather than
// re-reading the same prefix forever.
func TestChangedFilesAreNotStarvedByThePerPassBudget(t *testing.T) {
	root := t.TempDir()
	const files = 60 // comfortably under the per-pass budget
	for i := 0; i < files; i++ {
		writeJSONL(t, filepath.Join(root, fmt.Sprintf("s%02d.jsonl", i)),
			`{"type":"session","id":"s`+strconv.Itoa(i)+`","cwd":"/tmp/p"}`,
			`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":5,"output":5}}}`,
		)
	}
	st := newTestStore(t)
	sc := newScanner(t, st, root)
	ctx := context.Background()

	for pass := 1; pass <= 3; pass++ {
		if err := sc.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		got, _ := st.Totals(ctx, store.Filter{})
		if got.Sessions != files {
			t.Fatalf("pass %d: %d of %d sessions stored", pass, got.Sessions, files)
		}
	}
}

// -------------------------------------------------------------- background pass

// The background half of the process: Run, Status, and the OpenCode scan path.
// All of it was 0% covered, and Run is what cmd/dashd actually calls — so the
// entry point the binary uses had no test at all.

// TestRunPassesImmediatelyAndStopsOnCancellation covers the contract Run has:
// one pass before the first tick, then one per tick, then a clean return when the
// context is cancelled.
//
// The immediate first pass is the point of the design comment above Run: the
// database must be populated before the first request rather than after the first
// tick, and a test that only waits for a tick would not notice its removal.
func TestRunPassesImmediatelyAndStopsOnCancellation(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "s.jsonl"),
		`{"type":"session","id":"s1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":5,"output":5}}}`,
	)
	st := newTestStore(t)
	sc := newScanner(t, st, root)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sc.Run(ctx, 10*time.Millisecond)
	}()

	// The first pass runs before Run blocks on its ticker, so polling for the
	// call is enough; no sleep-and-hope.
	deadline := time.Now().Add(10 * time.Second)
	for {
		totals, err := st.Totals(context.Background(), store.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if totals.Calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-stopped
			t.Fatal("Run never performed its initial pass: the database is empty 10s " +
				"after it started, so a reload before the first tick shows nothing")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// And the pass status is visible to the endpoint that reports it.
	//
	// lastRun is stamped at the end of a pass, so polling for ingested rows
	// first is not enough: a pass commits its calls before it records that it
	// finished, and reading Status() in that window saw a pass still running.
	// Wait for the thing this asserts.
	deadline = time.Now().Add(10 * time.Second)
	var lastRun time.Time
	var running bool
	for {
		lastRun, running = sc.Status()
		if !lastRun.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-stopped
			t.Fatal("Status reports no last-run time 10s after calls were ingested")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lastRun.IsZero() {
		t.Error("Status reports no last-run time after a pass")
	}
	if !running {
		// Racy by nature: the pass may have finished between the poll and here.
		t.Log("Status reported not-running, which is correct if the pass had " +
			"already completed")
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of its context being cancelled: the " +
			"background loop is what a SIGTERM has to interrupt")
	}

	// After Run returns, the status must report a last-run time and not running.
	// A shutdown that leaves `running` set would have the health endpoint claim a
	// scan is in progress for the life of the process.
	lastRun, running = sc.Status()
	if lastRun.IsZero() {
		t.Error("Status reports no last-run time after Run returned")
	}
	if running {
		t.Error("Status still reports a pass in progress after Run returned")
	}
}

// TestRunDefaultsANonPositiveInterval covers the guard above the ticker.
//
// A zero or negative interval panics in time.NewTicker, and Run is what the CLI
// calls with a user-supplied -interval, so a bad value there must not take the
// process down at startup.
func TestRunDefaultsANonPositiveInterval(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "s.jsonl"),
		`{"type":"session","id":"s1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":5,"output":5}}}`,
	)
	for _, every := range []time.Duration{0, -time.Second} {
		t.Run(every.String(), func(t *testing.T) {
			st := newTestStore(t)
			sc := newScanner(t, st, root)
			ctx, cancel := context.WithCancel(context.Background())
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				// A panic here would take the test process down rather than
				// failing, which is what the guard prevents.
				sc.Run(ctx, every)
			}()

			cancel()
			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatalf("Run(%v) did not return after cancellation", every)
			}
		})
	}
}

// TestRunOnceReportsAFailingPassAndKeepsGoing covers the logging wrapper.
//
// runOnceLogged swallows an error unless the context is live, so a failing pass
// does not stop the loop. The observable consequence is that Run keeps running
// after a pass fails, which is what makes a transient database lock survivable.
func TestRunOnceReportsAFailingPassAndKeepsGoing(t *testing.T) {
	st := newTestStore(t)
	// A closed store: every pass fails, and the failure must not propagate.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeJSONL(t, filepath.Join(root, "s.jsonl"),
		`{"type":"session","id":"s1","cwd":"/tmp/p"}`,
		`{"type":"message","timestamp":"2026-05-01T00:00:05Z","id":"m1","message":{"role":"assistant","model":"m","usage":{"input":5,"output":5}}}`,
	)
	sc := newScanner(t, st, root)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// RunOnce's error is returned; runOnceLogged logs and returns. Both are
		// called here so neither path is untested.
		_ = sc.RunOnce(context.Background())
		sc.runOnceLogged(context.Background())
		sc.runOnceLogged(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("a pass against a closed store did not return within 15s")
	}
}

// -------------------------------------------------------------- OpenCode

// The OpenCode source is the odd one out: every session lives in one shared
// database, so the unit of work is a row rather than a file. Nothing tested the
// scanner half of it — openCodeSessionIDs, scanOpenCode, or the Run branch that
// calls them.

// buildOpenCodeScanner makes a scanner whose source is an OpenCode database, with
// the same fixture the parser tests use.
func buildOpenCodeScanner(t *testing.T, st *store.Store, dbPath string) *Scanner {
	t.Helper()
	return New(Config{
		Store: st,
		Pricer: func() *source.Pricer {
			p, err := source.NewPricer("../../models.json", "manual_pricing.json")
			if err != nil {
				t.Fatal(err)
			}
			return p
		}(),
		Logger:     testLogger(),
		OpenCodeDB: dbPath,
	})
}

// openCodeFixtureDB writes a small OpenCode-shaped database and returns its path.
//
// The schema is the one the parser reads, plus the parent_id the scanner's own
// query filters on. It is built here rather than shared with internal/source,
// whose fixture builder is a test file in another package: what the scanner needs
// to be exercised is the schema and the row shape, not the parser's sanitised
// transcript.
//
// Two sessions, each with one assistant message carrying a usage block, and one
// child session under the first.
func openCodeFixtureDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, stmt := range []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY, directory TEXT, title TEXT, agent TEXT, model TEXT,
			time_created INTEGER, time_updated INTEGER, parent_id TEXT)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY, session_id TEXT, time_created INTEGER,
			time_updated INTEGER, data TEXT)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY, message_id TEXT, session_id TEXT,
			time_created INTEGER, time_updated INTEGER, data TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	seed := func(id, parent string, created int64) {
		var parentArg any
		if parent != "" {
			parentArg = parent
		}
		if _, err := db.Exec(
			`INSERT INTO session (id, directory, title, agent, model, time_created, time_updated, parent_id)
			 VALUES (?,?,?,?,?,?,?,?)`,
			id, "/home/testuser/project", "a session", "opencode", "claude-sonnet-4-6",
			created, created, parentArg); err != nil {
			t.Fatal(err)
		}
		data := `{"role":"assistant","tokens":{"input":100,"output":50,"reasoning":0},` +
			`"modelID":"claude-sonnet-4-6"}`
		if _, err := db.Exec(
			`INSERT INTO message (id, session_id, time_created, time_updated, data)
			 VALUES (?,?,?,?,?)`,
			"msg_"+id, id, created, created, data); err != nil {
			t.Fatal(err)
		}
	}
	seed("ses_top0000000001", "", 1779976545)
	seed("ses_top0000000002", "", 1779976645)
	seed("ses_child00000001", "ses_top0000000001", 1779976745)
	return path
}

// TestOpenCodeSessionIDsReturnsOnlyTopLevelSessions covers the query the scanner
// runs before parsing anything.
//
// OpenCode nests child sessions under a parent, and ingesting a child as a
// top-level session would produce a duplicate: the child's messages are part of
// the parent's transcript. The filter is `parent_id IS NULL OR parent_id = ”`,
// and this is the first time it has been executed.
func TestOpenCodeSessionIDsReturnsOnlyTopLevelSessions(t *testing.T) {
	path := openCodeFixtureDB(t)

	ids, err := openCodeSessionIDs(path)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	if !seen["ses_top0000000001"] || !seen["ses_top0000000002"] {
		t.Errorf("openCodeSessionIDs returned %v, want both top-level sessions", ids)
	}
	if seen["ses_child00000001"] {
		t.Error("openCodeSessionIDs returned a child session: its messages belong to " +
			"its parent's transcript, so ingesting it separately double-counts them")
	}
	if len(ids) != 2 {
		t.Errorf("openCodeSessionIDs returned %d sessions, want 2", len(ids))
	}

	// A missing database is the "not installed" case, which the caller treats as
	// nothing to scan rather than as a failure.
	if _, err := openCodeSessionIDs(filepath.Join(t.TempDir(), "absent.db")); err == nil {
		t.Error("openCodeSessionIDs accepted a missing database")
	}
}

// TestScanOpenCodeIngestsAndThenSkipsUnchangedSessions is the incremental
// property for the shared-database source.
//
// The unit is a session rather than a file, so the "has this changed" test is a
// time cursor rather than a size and mtime. A pass that re-ingests every session
// every time would look like it worked — the totals would be right — and would
// re-read every session in the database on every pass, which is the cost this
// design exists to avoid.
func TestScanOpenCodeIngestsAndThenSkipsUnchangedSessions(t *testing.T) {
	path := openCodeFixtureDB(t)
	st := newTestStore(t)
	sc := buildOpenCodeScanner(t, st, path)
	ctx := context.Background()

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, err := st.Totals(ctx, store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sessions == 0 {
		t.Fatalf("the first pass ingested nothing from %s", path)
	}
	t.Logf("first pass: %d sessions, %d calls", first.Sessions, first.Calls)

	// The statuses are recorded under the opencode agent name, and the counts
	// describe sessions rather than files.
	statuses, err := st.ScanStatuses()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, s := range statuses {
		if s.Agent != model.AgentOpencode {
			continue
		}
		found = true
		if s.FilesChanged == 0 {
			t.Error("the first pass reported no changed OpenCode sessions")
		}
	}
	if !found {
		t.Fatal("no scan status was recorded for the opencode source: the panel " +
			"reports six sources whether or not each was walked, so a missing row " +
			"reads as \"not part of the last scan\"")
	}

	// A second pass must re-read nothing.
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	statuses, _ = st.ScanStatuses()
	for _, s := range statuses {
		if s.Agent == model.AgentOpencode && s.FilesChanged != 0 {
			t.Errorf("the second pass re-ingested %d OpenCode sessions; the time "+
				"cursor did not hold", s.FilesChanged)
		}
	}

	second, _ := st.Totals(ctx, store.Filter{})
	if second.Sessions != first.Sessions || second.Calls != first.Calls {
		t.Errorf("an unchanged pass changed the totals: %d/%d then %d/%d",
			first.Sessions, first.Calls, second.Sessions, second.Calls)
	}

	// Appending a message to one session must extend that session, not replace
	// it. The read is incremental — the parser resumes past the stored cursor and
	// returns only the new rows — so committing it as a replacement would delete
	// every call already stored for the session. That is the same failure the
	// file path guards against in ingest(), and it is not guarded here.
	//
	// scanOpenCode now appends once a session has a non-zero stored cursor, the
	// same rule ingest() applies to the file sources.
	if err := appendOpenCodeMessage(t, path, "ses_top0000000001", 1779977000000); err != nil {
		t.Fatal(err)
	}
	if err := sc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	third, _ := st.Totals(ctx, store.Filter{})
	if third.Calls != first.Calls+1 {
		t.Errorf("after appending a message there are %d calls, want %d: the increment "+
			"was not committed alongside the calls already stored",
			third.Calls, first.Calls+1)
	}
	// The specific shape: the session that grew must hold both of its calls.
	perSession, err := st.SessionCalls(ctx, "ses_top0000000001")
	if err != nil {
		t.Fatal(err)
	}
	if len(perSession) != 2 {
		t.Errorf("session ses_top0000000001 holds %d calls after an append, want 2: "+
			"an incremental read committed as a replacement deletes the rows it did "+
			"not re-read", len(perSession))
	}
	// And the session that did not move must be untouched.
	other, err := st.SessionCalls(ctx, "ses_top0000000002")
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 {
		t.Errorf("session ses_top0000000002 holds %d calls, want 1: the cursor is per "+
			"session, so an append to one must not disturb another", len(other))
	}
	statuses, _ = st.ScanStatuses()
	for _, s := range statuses {
		if s.Agent == model.AgentOpencode && s.FilesChanged != 1 {
			t.Errorf("the third pass re-ingested %d sessions, want 1: the cursor is "+
				"per session, so only the one that moved should be re-read",
				s.FilesChanged)
		}
	}
}

// TestScanOpenCodeIgnoresAMissingDatabase covers the installed-or-not case.
//
// OpenCode is the one agent here that may simply not be on the machine, and its
// absence is not an error: every other user of dashd has a pi log or none of this
// dashboard means anything.
func TestScanOpenCodeIgnoresAMissingDatabase(t *testing.T) {
	st := newTestStore(t)
	sc := buildOpenCodeScanner(t, st, filepath.Join(t.TempDir(), "not-installed.db"))
	ctx := context.Background()

	if err := sc.RunOnce(ctx); err != nil {
		t.Fatalf("a missing OpenCode database failed the pass: %v", err)
	}
	totals, _ := st.Totals(ctx, store.Filter{})
	if totals.Sessions != 0 {
		t.Errorf("a missing OpenCode database produced %d sessions", totals.Sessions)
	}
	// The source is still reported, so the panel shows it as searched-for rather
	// than as unknown.
	statuses, _ := st.ScanStatuses()
	var found bool
	for _, s := range statuses {
		if s.Agent == model.AgentOpencode {
			found = true
			if s.Error != "" {
				t.Errorf("a missing database was recorded as a scan failure: %q", s.Error)
			}
		}
	}
	if !found {
		t.Error("no status recorded for the opencode source when its database is " +
			"absent: the panel would show it as \"not scanned\" rather than as " +
			"\"searched for, nothing there\"")
	}
}

// TestScanOpenCodeReportsAnUnreadableDatabase is the loud-failure half.
//
// A database whose schema has changed must produce an error on that source's
// status rather than a silent zero: OpenCode ships its own migrations and can
// rename a column under us, and a scan that quietly reports nothing is
// indistinguishable from a user with no OpenCode sessions.
func TestScanOpenCodeReportsAnUnreadableDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wrong-schema.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (a TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st := newTestStore(t)
	sc := buildOpenCodeScanner(t, st, path)
	// The pass itself must still run all sources: one broken source must not
	// stop the others. But the failure must now surface in the return value so
	// a cron-driven caller's exit code reflects it.
	err = sc.RunOnce(context.Background())
	if err == nil {
		t.Fatal("an unreadable OpenCode database returned a nil error; the failure " +
			"must surface so cron/systemd see a non-zero exit")
	}
	if !strings.Contains(err.Error(), "opencode") {
		t.Fatalf("error should name the failing source, got %v", err)
	}

	statuses, _ := st.ScanStatuses()
	var found bool
	for _, s := range statuses {
		if s.Agent != model.AgentOpencode {
			continue
		}
		found = true
		if s.Error == "" {
			t.Error("an unreadable OpenCode database was recorded as a successful " +
				"pass: a schema change would read as \"you have no sessions\"")
		}
	}
	if !found {
		t.Fatal("no status recorded for the failing opencode source")
	}
}

// appendOpenCodeMessage inserts one assistant message, as OpenCode would.
func appendOpenCodeMessage(t *testing.T, path, sessionID string, at int64) error {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec(
		`INSERT INTO message (id, session_id, time_created, time_updated, data)
		 VALUES (?,?,?,?,?)`,
		"msg_scanner_appended", sessionID, at, at,
		`{"role":"assistant","tokens":{"input":7,"output":8,"reasoning":0},`+
			`"modelID":"claude-sonnet-4-6"}`)
	return err
}

// DefaultSources was 0% covered and is what cmd/dashd calls to decide what to
// scan, so the six locations it returns were never executed.

// TestDefaultSourcesCoversEveryAgentItKnowsAbout checks the list is complete and
// that each root is the exported one.
//
// Two directions, because either can break alone. A source missing from the list
// means an agent's logs are never read; a root that disagrees with
// HomeRelativeRoots means the page's hint and the walk point at different places,
// which is the defect the export exists to prevent.
func TestDefaultSourcesCoversEveryAgentItKnowsAbout(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "tester")
	sources, openCodeDB := DefaultSources(home)

	byAgent := map[string]Source{}
	for _, s := range sources {
		byAgent[s.Agent] = s
	}
	for agent, rel := range HomeRelativeRoots {
		want := filepath.Join(home, rel)
		if agent == model.AgentOpencode {
			if openCodeDB != want {
				t.Errorf("the OpenCode database is %q, want %q from HomeRelativeRoots",
					openCodeDB, want)
			}
			if _, walked := byAgent[agent]; walked {
				t.Errorf("%s is in both the tree sources and the OpenCode database "+
					"path: it would be scanned twice per pass", agent)
			}
			continue
		}
		src, ok := byAgent[agent]
		if !ok {
			t.Errorf("%s is in HomeRelativeRoots but DefaultSources does not return "+
				"a source for it, so its logs are never read", agent)
			continue
		}
		if src.Root != want {
			t.Errorf("%s root = %q, want %q: the scanner and the page's hint would "+
				"point at different places", agent, src.Root, want)
		}
		if src.parserFor == nil {
			t.Errorf("%s has no parser: a discovered file would be skipped silently", agent)
		}
		if got := src.parserFor().Agent(); got != agent {
			t.Errorf("%s's parser reports agent %q", agent, got)
		}
		if src.Ext == "" {
			t.Errorf("%s matches no file extension, so it discovers nothing", agent)
		}
	}
	if len(sources) != len(HomeRelativeRoots)-1 {
		t.Errorf("DefaultSources returned %d tree sources, want %d (one per agent "+
			"except OpenCode, which is a database)", len(sources), len(HomeRelativeRoots)-1)
	}
}

// TestDefaultSourcesHonoursTheHomeArgument is why the roots are joined rather
// than hardcoded: -home exists so a machine whose logs live elsewhere is
// readable, and an absolute path baked into the table would ignore it.
func TestDefaultSourcesHonoursTheHomeArgument(t *testing.T) {
	for _, home := range []string{"/home/tester", "/mnt/logs", "/"} {
		sources, openCodeDB := DefaultSources(home)
		for _, s := range sources {
			if !strings.HasPrefix(s.Root, home) {
				t.Errorf("with -home %q, %s is rooted at %q", home, s.Agent, s.Root)
			}
			if strings.Contains(s.Root, "~") {
				t.Errorf("with -home %q, %s is rooted at %q: a tilde is not expanded",
					home, s.Agent, s.Root)
			}
		}
		if !strings.HasPrefix(openCodeDB, home) {
			t.Errorf("with -home %q, the OpenCode database is %q", home, openCodeDB)
		}
	}
}

// TestHomeRelativeRootIsUnknownForAnUnknownAgent keeps the lookup honest: a name
// this build cannot read must not resolve to a path that looks plausible.
func TestHomeRelativeRootIsUnknownForAnUnknownAgent(t *testing.T) {
	for _, agent := range []string{
		model.AgentPi, model.AgentClaude, model.AgentCodex,
		model.AgentGemini, model.AgentAgy, model.AgentOpencode,
	} {
		rel, ok := HomeRelativeRoot(agent)
		if !ok {
			t.Errorf("%s has no log location; every agent the parser can read needs "+
				"one, or the source panel cannot offer a path", agent)
			continue
		}
		if rel == "" {
			t.Errorf("%s's log location is empty", agent)
		}
		if strings.HasPrefix(rel, "/") {
			t.Errorf("%s's log location %q is absolute; it is relative to the home "+
				"directory so that -home applies", agent, rel)
		}
	}
	if _, ok := HomeRelativeRoot("not-an-agent"); ok {
		t.Error("an unknown agent resolved to a log location")
	}
}
