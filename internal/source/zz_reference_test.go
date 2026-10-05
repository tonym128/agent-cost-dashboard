package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tonym/agent-cost-dashboard/internal/model"
)

// TestAgainstPythonReference compares the Antigravity parser against a JSON dump
// produced by the Python implementation, so a divergence in the token
// arithmetic shows up as a test failure rather than a subtly wrong number.
type refSession struct {
	Messages  int     `json:"messages"`
	Input     int     `json:"input"`
	Output    int     `json:"output"`
	Cached    int     `json:"cached"`
	Reasoning int     `json:"reasoning"`
	Cost      float64 `json:"cost"`
}

func TestAgainstPythonReference(t *testing.T) {
	raw, err := os.ReadFile("/tmp/opencode/py_agy.json")
	if err != nil {
		t.Skipf("no reference dump: %v", err)
	}
	var ref map[string]refSession
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	pricer, err := NewPricer("../../models.json", "manual_pricing.json")
	if err != nil {
		t.Fatal(err)
	}
	p := NewAntigravityParser()
	dir := filepath.Join(os.Getenv("HOME"), ".gemini/antigravity/conversations")
	entries, _ := filepath.Glob(filepath.Join(dir, "*.db"))
	for _, db := range entries {
		uid := filepath.Base(db)
		uid = uid[:len(uid)-len(filepath.Ext(uid))]
		want, ok := ref[uid]
		if !ok {
			continue
		}
		sw, _, err := p.Parse(db, model.ScanState{}, pricer)
		if err != nil {
			t.Errorf("%s: %v", uid, err)
			continue
		}
		var in, outTok, cached, reasoning int
		var cost float64
		for _, c := range sw.Calls {
			in += c.InputTokens
			outTok += c.OutputTokens
			cached += c.CacheReadTokens
			reasoning += c.ReasoningTokens
			cost += c.CostUSD
		}
		if got, w := len(sw.Calls), want.Messages; got != w {
			t.Errorf("%s: %d calls, python had %d", uid, got, w)
		}
		if in != want.Input {
			t.Errorf("%s: input %d, python %d", uid, in, want.Input)
		}
		// Both report the non-reasoning remainder as output.
		if outTok != want.Output {
			t.Errorf("%s: output %d, python %d", uid, outTok, want.Output)
		}
		if cached != want.Cached {
			t.Errorf("%s: cache read %d, python %d", uid, cached, want.Cached)
		}
		// Reasoning is clamped to the generated count that contains it. The
		// Python reference does not clamp, so on blobs where reasoning exceeds
		// output it reports the larger of the two; matching it exactly would mean
		// storing a reasoning count bigger than the total it is part of.
		if reasoning > want.Reasoning {
			t.Errorf("%s: reasoning %d exceeds python %d", uid, reasoning, want.Reasoning)
		}
		diff := cost - want.Cost
		if diff < -0.01 || diff > 0.01 {
			t.Errorf("%s: cost $%.4f, python $%.4f", uid, cost, want.Cost)
		}
	}
}
