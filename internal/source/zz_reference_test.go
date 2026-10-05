package source

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// The README claims the parsers are verified against a dump of the previous
// Python implementation. This file is where that claim is either true or
// decorative, so it reads only committed files.
//
// It used to read /tmp/opencode/py_agy.json and glob the developer's own
// ~/.gemini/antigravity/conversations, skipping when either was absent — which
// is always, on a clean checkout and in CI. A test that can only fail on one
// machine verifies nothing. The dump's numbers for one conversation are
// committed under testdata/reference, alongside a sanitised fixture built to
// reproduce them, so the equivalence is checkable by anyone.

// pyReference is one conversation's totals as the Python implementation
// reported them.
type pyReference struct {
	Messages  int     `json:"messages"`
	Input     int     `json:"input"`
	Output    int     `json:"output"`
	Cached    int     `json:"cached"`
	Reasoning int     `json:"reasoning"`
	Cost      float64 `json:"cost"`
}

// pyReferenceFile is the committed dump.
type pyReferenceFile struct {
	Comment  string                 `json:"comment"`
	Sessions map[string]pyReference `json:"sessions"`
}

// loadPythonReference reads the committed dump, refusing to proceed on a
// placeholder. A reference file that had quietly emptied itself would otherwise
// turn this into a test that compares the parser against nothing and passes.
func loadPythonReference(t *testing.T) pyReferenceFile {
	t.Helper()
	var ref pyReferenceFile
	readFixture(t, "reference/antigravity_python_reference.json", &ref)
	if len(ref.Sessions) == 0 {
		t.Fatal("the committed Python dump holds no sessions: this test would compare the parser against nothing")
	}
	if ref.Comment == "" {
		t.Error("the dump no longer records where its numbers came from")
	}
	return ref
}

// TestAgainstPythonReference checks the Antigravity parser against the Python
// implementation's own totals for the conversation the committed fixture was
// derived from.
//
// It runs unconditionally. Every input is a committed file, so there is nothing
// that could be missing on a machine that has never seen this repository, and a
// skip here would mean the check silently stopped happening.
func TestAgainstPythonReference(t *testing.T) {
	ref := loadPythonReference(t)
	pricer := testPricer(t)
	p := NewAntigravityParser()

	for uid, want := range ref.Sessions {
		t.Run(uid, func(t *testing.T) {
			// The file name is the conversation id, which is how Antigravity names
			// its databases and what the parser derives the session uid from. It
			// has to match the key in the dump or the two would not be comparable.
			path := buildAntigravityDB(t, uid+".db")
			if got := p.SessionUID(path); got != uid {
				t.Fatalf("session uid = %q, want %q", got, uid)
			}

			sw, _, err := p.Parse(path, model.ScanState{}, pricer)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			checkInvariants(t, sw)

			// The call count is the assertion the token totals cannot make. A
			// corrupt blob that becomes a zero-token call leaves every total
			// untouched while inflating the number of calls by one, so this is
			// what catches a step being counted twice or not at all.
			if got := len(sw.Calls); got != want.Messages {
				t.Errorf("parsed %d calls, python had %d", got, want.Messages)
			}

			tot := totalsOf(sw)
			// Both report the non-reasoning remainder as output.
			if tot.input != want.Input {
				t.Errorf("input %d, python %d", tot.input, want.Input)
			}
			if tot.output != want.Output {
				t.Errorf("output %d, python %d", tot.output, want.Output)
			}
			if tot.cacheRead != want.Cached {
				t.Errorf("cache read %d, python %d", tot.cacheRead, want.Cached)
			}
			// An equality, not a ceiling. Reasoning is the largest single token
			// bucket in this conversation, so a one-sided check lets a parser that
			// halved it — or dropped two thirds of it — ship: only over-reporting
			// was ever rejected. No step in the fixture has reasoning above its
			// output, so the clamp is inactive here and the two figures must match
			// exactly; TestAntigravityClampsReasoningAboveOutput covers the clamp
			// on the one case that needs it.
			if tot.reasoning != want.Reasoning {
				t.Errorf("reasoning %d, python %d", tot.reasoning, want.Reasoning)
			}
			// A cent is the rounding the dashboard itself displays at, so this is
			// tight enough to catch a wrong rate and loose enough to survive a
			// pricing table gaining a digit.
			if diff := tot.cost - want.Cost; diff < -0.01 || diff > 0.01 {
				t.Errorf("cost $%.4f, python $%.4f", tot.cost, want.Cost)
			}

			// Every call must be priced. A conversation priced as unknown reads as
			// free, and the totals above would still reconcile.
			if tot.priced != len(sw.Calls) {
				t.Errorf("%d of %d calls are priced; an unpriced model reads as free",
					tot.priced, len(sw.Calls))
			}
		})
	}
}

// TestPythonReferenceFixtureIsHonest checks the fixture the equivalence rests
// on, because a fixture quietly edited to agree with the parser turns the test
// above into a tautology — it would confirm only that the parser agrees with
// itself.
//
// The totals here are summed straight out of the committed JSON, with no parser
// involved, and are compared against the dump. If the two ever disagree, either
// the fixture no longer represents the conversation or the dump is wrong, and
// the equivalence test above is no longer testing anything.
func TestPythonReferenceFixtureIsHonest(t *testing.T) {
	ref := loadPythonReference(t)

	var fixture antigravityFixture
	readFixture(t, "antigravity/antigravity_steps.json", &fixture)
	if len(fixture.Steps) == 0 {
		t.Fatal("the Antigravity fixture holds no steps")
	}

	var usage, tool, toolWithoutName, malformed int
	for _, s := range fixture.Steps {
		switch {
		case s.Usage != nil:
			usage++
		case s.TruncatedUsage != nil, s.VarintOverrun != nil:
			malformed++
		case s.Tool != "":
			tool++
		case s.ToolWithoutName:
			toolWithoutName++
		}
	}

	for _, want := range ref.Sessions {
		// Only steps carrying a usage block become calls. The malformed ones are
		// here precisely to prove they do not, so they are counted separately
		// rather than folded into either side of the comparison.
		if usage != want.Messages {
			t.Errorf("fixture holds %d usable LLM steps, dump reports %d messages", usage, want.Messages)
		}
		if malformed == 0 {
			t.Error("the fixture has no malformed steps: it no longer tests that a corrupt blob contributes nothing")
		}
		if toolWithoutName == 0 {
			t.Error("the fixture has no nameless tool step: it no longer tests that an unnamed tool is dropped")
		}
	}

	// The fixture's own comment has to keep saying where the numbers came from,
	// or the next person to edit it has no way to know it is load-bearing.
	if fixture.Comment == "" {
		t.Error("the Antigravity fixture no longer records what it was derived from")
	}
	if fixture.Epoch == 0 {
		t.Error("the fixture has no epoch, so its timestamps are all 1970")
	}
}

// TestAntigravityClampsReasoningAboveOutput is the one case where reasoning may
// legitimately exceed what Python reported, asserted on its own rather than as a
// ceiling over the whole conversation.
//
// The committed fixture has no step whose reasoning counter is above its output
// counter, so the clamp is inactive there and the reference comparison is an
// equality. Here a single step reports 900 reasoning against 100 output. The
// parser must clamp rather than store 900: the dashboard's reasoning column is
// drawn from a figure that is supposed to be a slice of the output next to it,
// and 900 beside 100 is a number no reader can reconcile.
//
// The conversation is built here rather than committed because it exists only to
// drive this branch — the committed fixtures are the shapes real conversation
// databases have, and none of them contains a counter pair like this.
func TestAntigravityClampsReasoningAboveOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reasoning-overflow.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL DEFAULT 0,
		 status INTEGER NOT NULL DEFAULT 0, has_subtrajectory numeric NOT NULL DEFAULT false,
		 metadata blob, error_details blob, permissions blob, task_details blob,
		 render_info blob, step_payload blob, step_format INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE trajectory_meta (trajectory_id text, cascade_id text,
		 trajectory_type integer, source integer, PRIMARY KEY (trajectory_id))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	// idx 1, not 0: the parser reads steps where `idx > cursor`, and a cursor of
	// zero means "nothing read yet", so a row numbered 0 would be skipped.
	if _, err := db.Exec(
		`INSERT INTO steps (idx, step_type, status, metadata, step_payload) VALUES (1,?,?,?,?)`,
		agyStepLLM1, 3,
		agyStepMetadata(1779976545, nil, 1319, 1000, 100, 0, 900),
		agyWorkspacePayload("/home/testuser/project"),
	); err != nil {
		t.Fatal(err)
	}

	sw, _, err := NewAntigravityParser().Parse(path, model.ScanState{}, testPricer(t))
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, sw)
	if len(sw.Calls) != 1 {
		t.Fatalf("parsed %d calls, want 1", len(sw.Calls))
	}
	c := sw.Calls[0]
	// The clamp bounds reasoning to the generated count it is a slice of, so a
	// step reporting 900 against 100 of generated output yields reasoning 100 and
	// no visible remainder. Storing 900 would put a reasoning figure on the
	// dashboard nine times the size of the output beside it.
	if c.ReasoningTokens != 100 {
		t.Errorf("reasoning = %d, want clamped to the 100 of generated output", c.ReasoningTokens)
	}
	if c.OutputTokens != 0 {
		t.Errorf("output = %d, want 0: the whole generated count is reasoning once the "+
			"clamp has bound it", c.OutputTokens)
	}
	// The partition still holds after the clamp, which is the property that makes
	// the two columns add up to the log's own figure.
	checkGenerated(t, c, 100)
	// What is billed is the whole generated count the log reported, not the
	// itemised pair: pricing the remainder here would drop this step's output
	// spend entirely, which is the failure the split exists to avoid.
	rates, ok := testPricer(t).Resolve(c.Model)
	if !ok {
		t.Fatalf("%s is unpriced, so its cost cannot be checked", c.Model)
	}
	want := float64(c.InputTokens)/1e6*rates.Input +
		100/1e6*rates.Output +
		float64(c.CacheReadTokens)/1e6*rates.CacheRead
	if diff := c.CostUSD - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("cost $%.8f, want $%.8f (the whole generated count billed, not the "+
			"itemised remainder)", c.CostUSD, want)
	}
}
