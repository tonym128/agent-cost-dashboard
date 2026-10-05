package source

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// The README claims the parsers are verified against a dump of the previous
// Python implementation. This file is where that claim is either true or
// decorative.
//
// What was wrong with it, and what is now true instead:
//
// The claim was not independent. The dump's numbers were transcribed into
// antigravity_python_reference.json and the sanitised parser fixture was then
// built to reproduce them exactly, so every figure in that file was derived from
// the target rather than measured against it. The token sums were therefore
// self-consistent by construction, and the test that was supposed to police the
// fixture only counted `usage` steps and checked two fields were non-empty — it
// never checked a sum. Editing the fixture to match a broken parser would have
// gone unnoticed, which is the failure mode this file exists to prevent.
//
// It now says only what it can support. The Python implementation is gone, so
// there is no second implementation to re-run; what survives is:
//
//   - testdata/reference/antigravity_python_dump_slice.json, a verbatim slice of
//     the original dump with the full dump's sha256 recorded, so the transcription
//     into antigravity_python_reference.json is checkable by anyone who still
//     holds the dump. The two files are compared field by field.
//   - TestAntigravityFixtureSumsMatchTheCommittedSteps, which derives every token
//     total in the reference file from the committed step fixture with no parser
//     involved, and fails if they disagree. That is the internal consistency the
//     old test claimed to check and did not.
//
// The fixture is still built to reproduce the dump's numbers — there was no way
// round that, since the parser fixture has to be *some* conversation. What is
// no longer claimed is that this is an independent verification of it: it is a
// transcription check plus an internal-consistency check, and a parser that
// mis-reads the format still fails TestAgainstPythonReference on the call count.

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
			// The dollar total is checked as a relationship, not as an amount.
			//
			// It used to be compared against Python's own figure with a one-cent
			// tolerance, which is a claim about two price tables rather than about
			// this parser: models.json is rewritten by update_models.py, so a
			// vendor price change moved the Go figure and broke a test about
			// reading a protobuf. Python's own figure was computed from whatever
			// table it loaded at the time, so it cannot be reproduced exactly by a
			// different table at all.
			//
			// What must hold is that the cost is the token totals above priced at
			// the rates this pricer resolves, with the whole generated count — not
			// the itemised remainder — billed as output. That is the same assertion
			// the per-call test makes, aggregated over the conversation.
			rates, ok := pricer.Resolve(sw.Calls[0].Model)
			if !ok {
				t.Fatalf("%s is unpriced, so the cost cannot be checked", sw.Calls[0].Model)
			}
			wantCost := float64(tot.input)/1e6*rates.Input +
				float64(tot.output+tot.reasoning)/1e6*rates.Output +
				float64(tot.cacheRead)/1e6*rates.CacheRead
			if diff := tot.cost - wantCost; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("cost $%.8f, want $%.8f from the token totals priced at %v; the "+
					"whole generated count is billed as output", tot.cost, wantCost, rates)
			}
			// Python's figure is still checked, loosely and for a reason: it is a
			// different price table, so it cannot be matched exactly, but a
			// conversation of this size cannot plausibly cost a different order of
			// magnitude. A factor of ten is a bug somewhere; a per-cent drift is a
			// price change.
			if want.Cost > 0 {
				ratio := tot.cost / want.Cost
				if ratio < 0.1 || ratio > 10 {
					t.Errorf("cost $%.6f against python's $%.6f, a factor of %.2f: that is "+
						"a different order of magnitude, not a price-table drift",
						tot.cost, want.Cost, ratio)
				}
				t.Logf("cost $%.6f against python's $%.6f (factor %.3f, the two price "+
					"tables differ)", tot.cost, want.Cost, ratio)
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

// TestAntigravityFixtureIsWellFormed checks the fixture the comparison rests on
// for the properties that make it a conversation rather than a bag of records:
// that its usable, malformed and nameless steps are all present, and that it
// records where it came from.
//
// This used to be called TestPythonReferenceFixtureIsHonest, which overstated it:
// it counted `usage` steps and never verified a single token sum. The sum
// verification is TestAntigravityFixtureSumsMatchTheCommittedSteps below.
func TestAntigravityFixtureIsWellFormed(t *testing.T) {
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

// TestCommittedReferenceIsVerbatimFromTheDumpSlice checks the transcription.
//
// antigravity_python_reference.json is a hand copy of one conversation's entry
// from the Python dump, renamed from the real conversation id to the fixture's.
// Every figure in it must therefore match the committed slice exactly — that is
// what makes the transcription checkable by anyone holding the original dump,
// whose sha256 the slice records.
func TestCommittedReferenceIsVerbatimFromTheDumpSlice(t *testing.T) {
	// Reported is read as the same shape as the reference entry, so the two are
	// compared as numbers rather than as strings: a JSON float printed with %v
	// comes out in exponent form, which would report a transcription error where
	// there is none.
	var slice struct {
		Comment      string      `json:"comment"`
		Conversation string      `json:"conversation"`
		Reported     pyReference `json:"reported"`
	}
	readFixture(t, "reference/antigravity_python_dump_slice.json", &slice)
	if slice.Conversation == "" {
		t.Fatal("the dump slice names no conversation, so it cannot be compared with " +
			"the reference file")
	}
	if slice.Reported == (pyReference{}) {
		t.Fatal("the dump slice reports no totals")
	}
	// The recorded hash is what lets the slice be checked against the file it came
	// from; without it the slice is just another hand-written fixture.
	if !strings.Contains(slice.Comment, "sha256 is ") {
		t.Error("the dump slice no longer records the checksum of the dump it came from, " +
			"so its provenance cannot be verified")
	}

	ref := loadPythonReference(t)
	if len(ref.Sessions) != 1 {
		t.Fatalf("the reference file holds %d conversations, want 1: it is a copy of one "+
			"entry from the dump and cannot grow", len(ref.Sessions))
	}
	var got pyReference
	for _, v := range ref.Sessions {
		got = v
	}
	if got != slice.Reported {
		t.Errorf("the reference entry\n %+v\ndiffers from the dump slice\n %+v\n"+
			"the transcription no longer matches what Python reported", got, slice.Reported)
	}
}

// TestAntigravityFixtureSumsMatchTheCommittedSteps is the internal consistency
// the old honesty test claimed and did not check.
//
// Every token total in the reference file is re-derived here by summing the
// committed step fixture's own `usage` blocks, with no parser involved. The sums
// are non-trivial arithmetic — Antigravity's input and cache-read counters are
// disjoint buckets to be summed, and its output figure is the generated count
// that reasoning is then carved out of — so a fixture edited, truncated or
// hand-massaged to agree with a mis-parsing parser fails here even if the call
// count still matches.
//
// If these two ever disagree, either the fixture no longer represents the
// conversation or the reference file is wrong, and TestAgainstPythonReference
// above is no longer comparing anything.
func TestAntigravityFixtureSumsMatchTheCommittedSteps(t *testing.T) {
	ref := loadPythonReference(t)

	var fixture antigravityFixture
	readFixture(t, "antigravity/antigravity_steps.json", &fixture)
	if len(fixture.Steps) == 0 {
		t.Fatal("the Antigravity fixture holds no steps")
	}

	var calls, input, cached, generated, reasoning int
	for i, s := range fixture.Steps {
		if s.Usage == nil {
			continue
		}
		calls++
		input += int(s.Usage.Input)
		cached += int(s.Usage.CacheRead)
		generated += int(s.Usage.Output)
		reasoning += int(s.Usage.Reasoning)
		// A single step whose reasoning exceeds its output would mean the
		// reference file's split depends on the clamp, which the sum below
		// accounts for; flagging it keeps that visible rather than implicit.
		if s.Usage.Reasoning > s.Usage.Output {
			t.Logf("step %d reports reasoning %d above output %d, so the clamped split "+
				"differs from the raw sum", i, s.Usage.Reasoning, s.Usage.Output)
		}
	}
	// The visible remainder, the same split the parser applies per step.
	visible := 0
	for _, s := range fixture.Steps {
		if s.Usage == nil {
			continue
		}
		r := int(s.Usage.Reasoning)
		if r > int(s.Usage.Output) {
			r = int(s.Usage.Output)
		}
		visible += int(s.Usage.Output) - r
	}

	for uid, want := range ref.Sessions {
		if calls != want.Messages {
			t.Errorf("%s: the fixture holds %d usable usage steps, the reference reports %d",
				uid, calls, want.Messages)
		}
		for _, sum := range []struct {
			name string
			got  int
			want int
		}{
			{"input", input, want.Input},
			{"cached", cached, want.Cached},
			{"generated output", generated, want.Output + want.Reasoning},
			{"non-reasoning output", visible, want.Output},
			{"reasoning", reasoning, want.Reasoning},
		} {
			if sum.got != sum.want {
				t.Errorf("%s: the fixture's usage blocks sum to %d %s tokens, the "+
					"reference reports %d", uid, sum.got, sum.name, sum.want)
			}
		}
	}
}
