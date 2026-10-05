package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// Reprice exists to correct stale pricing. Its first obligation is therefore
// not to change anything at all on a database whose prices have not moved: a
// command whose whole purpose is "make the numbers right again" must never make
// them worse, and `dashd reprice` is documented as the remedy — so a user
// following that advice on a healthy database is the common case, not the edge.

// repriceBillable is the flat per-token rate every bucket is charged at in
// these fixtures. One rate means the arithmetic the test checks is readable by
// inspection: billable tokens times the rate, with no rate-mix confusion.
const repriceBillable = 5e-6

func repricePrice(_ string, in, out, cr, cw int64) (float64, bool) {
	return float64(in+out+cr+cw) * repriceBillable, true
}

// writeRepriceCalls inserts calls whose token columns are split the way every
// parser splits them: OutputTokens is the non-reasoning remainder and
// ReasoningTokens is the carved-out slice, while the billable generated count is
// the sum of the two.
func writeRepriceCalls(t *testing.T, st *Store) {
	t.Helper()
	uid := "reprice-session"
	sess := model.SessionWrite{}
	sess.UID, sess.Agent, sess.Project = uid, model.AgentClaude, "/p"
	at := time.Unix(1_700_000_000, 0).UTC()
	for i, spec := range []struct {
		in, out, reasoning, cr, cw int
	}{
		{1000, 200, 800, 5000, 300},
		{0, 0, 0, 40000, 0},
		{7, 0, 11, 0, 0},
	} {
		c := model.Call{
			SessionUID: uid, CallKey: fmt.Sprintf("call-%d", i), Agent: model.AgentClaude,
			Project: "/p", Model: "claude-opus-4-8", Time: at.Add(time.Duration(i) * time.Minute),
			InputTokens: spec.in, OutputTokens: spec.out, ReasoningTokens: spec.reasoning,
			CacheReadTokens: spec.cr, CacheWriteTokens: spec.cw,
		}
		c.CostUSD, c.Priced = repricePrice(c.Model, int64(c.InputTokens),
			int64(c.OutputTokens+c.ReasoningTokens), int64(c.CacheReadTokens), int64(c.CacheWriteTokens))
		c.TotalTokens = c.InputTokens + c.OutputTokens + c.CacheReadTokens +
			c.CacheWriteTokens + c.ReasoningTokens
		sess.Calls = append(sess.Calls, c)
	}
	if err := st.ReplaceSession(sess); err != nil {
		t.Fatal(err)
	}
	if err := st.RecomputeSession(uid); err != nil {
		t.Fatal(err)
	}
}

func storedReasoning(t *testing.T, st *Store) int64 {
	t.Helper()
	var v int64
	if err := st.DB().QueryRow(`SELECT COALESCE(SUM(reasoning_tokens),0) FROM call`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func storedBillableTokens(t *testing.T, st *Store) int64 {
	t.Helper()
	var v int64
	if err := st.DB().QueryRow(`SELECT COALESCE(SUM(input_tokens + output_tokens +
		reasoning_tokens + cache_read_tokens + cache_write_tokens),0) FROM call`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func storedCallCount(t *testing.T, st *Store) int64 {
	t.Helper()
	var v int64
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM call`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func storedCost(t *testing.T, st *Store) float64 {
	t.Helper()
	var v float64
	if err := st.DB().QueryRow(`SELECT COALESCE(SUM(cost_usd),0) FROM call`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestRepriceIsIdempotentAtUnchangedPrices is the invariant that makes reprice
// safe to run: with the same price table it must reproduce the stored costs
// exactly, token for token.
//
// It fails before the fix because Reprice priced `output_tokens` alone while
// every parser bills `output_tokens + reasoning_tokens`. Reasoning was simply
// dropped, so running the documented remedy on a healthy database lowered every
// total — 12.3% on the committed reference conversation.
func TestRepriceIsIdempotentAtUnchangedPrices(t *testing.T) {
	st, err := Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	writeRepriceCalls(t, st)

	before := storedCost(t, st)
	want := float64(storedBillableTokens(t, st)) * repriceBillable
	if before != want {
		t.Fatalf("fixture stored $%.9f, want $%.9f: the test would not detect a reprice error", before, want)
	}

	// Exactly the price function the fixture was written with, so a reprice has
	// nothing to correct.
	if _, err := st.Reprice(context.Background(), repricePrice); err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	if after := storedCost(t, st); after != want {
		t.Errorf("repricing at unchanged prices moved the total from $%.9f to $%.9f "+
			"(%.2f%% lower): the remedy cut reported spend on a healthy database",
			before, after, 100*(before-after)/before)
	}

	// The session rollup has to follow the same arithmetic, or the session page
	// disagrees with the totals.
	var sessCost float64
	if err := st.DB().QueryRow(`SELECT cost_usd FROM session WHERE uid = 'reprice-session'`).Scan(&sessCost); err != nil {
		t.Fatal(err)
	}
	if sessCost != want {
		t.Errorf("session rollup is $%.9f, want $%.9f", sessCost, want)
	}
}

// TestRepricePricesReasoning guards the specific omission rather than only its
// arithmetic consequence: a price function is handed the reasoning it is given,
// and a caller that must pay for thinking can tell whether it was passed.
func TestRepricePricesReasoning(t *testing.T) {
	st, err := Open(t.TempDir() + "/r2.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	writeRepriceCalls(t, st)

	reasoning := storedReasoning(t, st)
	if reasoning == 0 {
		t.Fatal("fixture has no reasoning tokens")
	}
	// A per-reasoning-token surcharge on top of the flat rate. If Reprice never
	// hands reasoning to the price function, this term is absent from the total
	// and the shortfall is exactly reasoning*Surcharge.
	const surcharge = 1e-3
	before := storedCost(t, st)
	if _, err := st.Reprice(context.Background(), func(m string, in, out, cr, cw int64) (float64, bool) {
		cost, _ := repricePrice(m, in, out, cr, cw)
		return cost + float64(reasoning)*surcharge, true
	}); err != nil {
		t.Fatalf("Reprice: %v", err)
	}
	// The surcharge is charged per call, so it lands once per row.
	want := float64(storedBillableTokens(t, st))*repriceBillable +
		float64(reasoning)*surcharge*float64(storedCallCount(t, st))
	if got := storedCost(t, st); got != want {
		t.Errorf("after reprice total = $%.9f, want $%.9f "+
			"(reasoning of %d tokens was not passed to the price function)", got, want, reasoning)
	}
	if before == storedCost(t, st) {
		t.Error("the reprice changed nothing; the fixture is not exercising the path")
	}
}
