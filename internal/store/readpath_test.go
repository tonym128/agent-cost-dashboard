package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"

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
	if row.UID != "s1" || row.Path != "/tmp/s1" || row.Title != "title-s1" {
		t.Errorf("descriptive fields lost: %+v", row)
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

// sessionWriteFixture is one session with every column set to a distinct value,
// so a column read at the wrong offset in either session query cannot read as
// correct.
func sessionWriteFixture(uid string, calls int) model.SessionWrite {
	sess := model.SessionWrite{
		UID: uid, Agent: "pi", Project: "/p",
		Path: filepath.Join("/tmp", uid), Title: "title-" + uid,
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

// pad renders i zero-padded so lexical ordering matches numeric ordering.
func pad(i int) string { return fmt.Sprintf("%06d", i) }
