package source

import (
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
			// Reasoning is a ceiling rather than an equality. The Python reference
			// does not clamp reasoning to the generated count that contains it, so
			// on a blob where reasoning exceeds output it reports the larger of
			// the two. Matching it exactly would mean storing a reasoning count
			// bigger than the total it is part of.
			if tot.reasoning > want.Reasoning {
				t.Errorf("reasoning %d exceeds python %d", tot.reasoning, want.Reasoning)
			}
			// A cent is the rounding the dashboard itself displays at, so this is
			// tight enough to catch a wrong rate and loose enough to survive a
			// pricing table gaining a digit.
			if diff := tot.cost - want.Cost; diff < -0.01 || diff > 0.01 {
				t.Errorf("cost $%.4f, python $%.4f", tot.cost, want.Cost)
			}

			// The reasoning ceiling above is only meaningful if it is actually
			// reached: a parser that reported no reasoning at all would pass it
			// while losing the largest single token bucket in the conversation.
			if want.Reasoning > 0 && tot.reasoning == 0 {
				t.Errorf("reasoning total is zero, python reported %d", want.Reasoning)
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
