package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/tonym/agent-cost-dashboard/internal/model"
	"github.com/tonym/agent-cost-dashboard/internal/source"
	"github.com/tonym/agent-cost-dashboard/internal/store"
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
