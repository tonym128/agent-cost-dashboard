package web

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
	"github.com/tonym128/agent-cost-dashboard/internal/scan"
)

// The source panel's whole purpose is to be acted on: a user whose pi logs dashd
// cannot read is told where pi keeps them, so they can go and look. That only
// works if the hint is the place the scanner actually looked.

// TestSourceHintsMatchTheScannerRoots is the check that was missing.
//
// knownSources and scan.DefaultSources used to hold the same six paths,
// independently, and nothing compared them. A reader could be pointed at a
// directory the scanner never walked and have no way to tell: the panel says
// "no logs found under ~/.pi/agent/sessions" whether or not that is where the
// scanner spent its pass.
//
// Both sides are now compared against scan.HomeRelativeRoots, so this fails if
// either list drifts. It passes today because the two lists do currently agree —
// which is the point: the reviewer made them disagree deliberately and nothing
// noticed.
func TestSourceHintsMatchTheScannerRoots(t *testing.T) {
	if len(knownSources) != len(scan.HomeRelativeRoots) {
		t.Errorf("the panel lists %d agents and the scanner knows %d: an agent "+
			"missing from either list is one whose logs are never read, or one whose "+
			"miss cannot be acted on",
			len(knownSources), len(scan.HomeRelativeRoots))
	}

	seen := map[string]bool{}
	for _, k := range knownSources {
		rel, ok := scan.HomeRelativeRoot(k.Agent)
		if !ok {
			t.Errorf("the panel lists %s, which the scanner does not know: its hint "+
				"points somewhere nothing is ever looked for", k.Agent)
			continue
		}
		seen[k.Agent] = true

		// The hint is written `~/`-prefixed for the reader; the scanner's is
		// relative to a home directory the user may have overridden with -home.
		// Comparing them means stripping the prefix, not comparing whole strings.
		want := "~/" + filepath.ToSlash(rel)
		got := strings.TrimSuffix(k.Hint, "/")
		if got != want {
			t.Errorf("%s: the panel points at %q but the scanner walks %q. A reader "+
				"sent to the wrong directory concludes dashd cannot read their logs "+
				"when it simply looked somewhere else.",
				k.Agent, k.Hint, want)
		}
		// And it must actually resolve to an absolute path on this machine, since
		// that is the form the page renders for pasting into a shell.
		if resolved := homePath(k.Hint); strings.HasPrefix(resolved, "~") {
			t.Errorf("%s: the hint %q does not resolve to an absolute path (%q), so "+
				"the page shows a tilde the reader has to expand themselves",
				k.Agent, k.Hint, resolved)
		}
	}
	for agent := range scan.HomeRelativeRoots {
		if !seen[agent] {
			t.Errorf("the scanner knows %s but the panel does not list it: its state "+
				"is invisible, so a user cannot tell whether it was scanned", agent)
		}
	}
}

// TestSourceHintsCoverTheAgentsTheParsersRead keeps the panel and the parser set
// in step, which is a third list and the one most likely to drift: adding a
// parser without adding a panel row means that agent is scanned and never
// reported.
func TestSourceHintsCoverTheAgentsTheParsersRead(t *testing.T) {
	parsers := map[string]func() string{
		model.AgentPi:       func() string { return "pi" },
		model.AgentClaude:   func() string { return "claude" },
		model.AgentCodex:    func() string { return "codex" },
		model.AgentGemini:   func() string { return "gemini" },
		model.AgentAgy:      func() string { return "agy" },
		model.AgentOpencode: func() string { return "opencode" },
	}
	// Every agent the panel knows must have a parser, so a row cannot promise a
	// location nothing reads.
	for _, k := range knownSources {
		if _, ok := parsers[k.Agent]; !ok {
			t.Errorf("the panel lists %s, which no parser reads", k.Agent)
		}
	}
	if len(knownSources) != len(parsers) {
		t.Errorf("the panel lists %d agents and there are %d parsers", len(knownSources), len(parsers))
	}
}

// TestEverySourceViewIsProducedForAKnownAgent checks the panel has a row per
// agent whether or not a scan has run, which is what makes a miss visible rather
// than inferred from an absent row.
//
// A reader with no scan_status rows sees "not scanned" for all six; a reader with
// rows for three sees those three plus "not part of the last scan" for the rest.
// Either way every agent appears.
func TestEverySourceViewIsProducedForAKnownAgent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []scanStatusView
	}{
		{"nothing scanned", nil},
		{"one source scanned", []scanStatusView{{Agent: model.AgentClaude, FilesSeen: 3}}},
		{"every source scanned", func() []scanStatusView {
			var out []scanStatusView
			for _, k := range knownSources {
				out = append(out, scanStatusView{Agent: k.Agent, FilesSeen: 1, CallsAdded: 2})
			}
			return out
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			views := sourceViews(tc.statuses)
			if len(views) != len(knownSources) {
				t.Fatalf("sourceViews produced %d rows, want one per known agent (%d)",
					len(views), len(knownSources))
			}
			for i, v := range views {
				if v.Agent != knownSources[i].Agent {
					t.Errorf("row %d is %s, want %s: the panel's order is the one the "+
						"template and the test above both read", i, v.Agent, knownSources[i].Agent)
				}
				if v.Status == "" || v.Detail == "" {
					t.Errorf("%s has status %q and detail %q; an agent with nothing to "+
						"say is indistinguishable from one that was not scanned",
						v.Agent, v.Status, v.Detail)
				}
				if v.Hint == "" {
					t.Errorf("%s has no hint: the point of the row is the path", v.Agent)
				}
				if strings.ContainsAny(v.State, " \t") {
					t.Errorf("%s's state %q is not a single CSS class token", v.Agent, v.State)
				}
			}
		})
	}
}

// TestSourceSummaryCountsWhatItSaysItCounts covers the one-line verdict above the
// table, which is what a reader actually reads first.
func TestSourceSummaryCountsWhatItSaysItCounts(t *testing.T) {
	all := func() []scanStatusView {
		var out []scanStatusView
		for _, k := range knownSources {
			out = append(out, scanStatusView{Agent: k.Agent, FilesSeen: 1, CallsAdded: 1})
		}
		return out
	}
	mixed := all()
	// Two found, two walked with nothing there, one failing, one never walked:
	// every state the summary counts, in one view.
	mixed[2] = scanStatusView{Agent: mixed[2].Agent, FilesSeen: 0}
	mixed[3] = scanStatusView{Agent: mixed[3].Agent, FilesSeen: 0, Error: "permission denied"}
	mixed[4] = scanStatusView{Agent: mixed[4].Agent, FilesSeen: 0}
	mixed = mixed[:5] // the sixth agent has no row at all: not scanned

	for _, tc := range []struct {
		name     string
		statuses []scanStatusView
		want     []string
	}{
		{"nothing at all", nil, []string{"0 of 6", "6 not scanned"}},
		{"everything found", all(), []string{"6 of 6", "sources found"}},
		{
			"some of each", mixed,
			[]string{"2 of 6", "2 with no logs", "1 failing", "1 not scanned"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := sourceSummary(sourceViews(tc.statuses))
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("summary %q does not contain %q", got, want)
				}
			}
			// The counts in the summary have to be the counts in the rows, or the
			// headline disagrees with the table beneath it.
			views := sourceViews(tc.statuses)
			var found, empty, failed, unscanned int
			for _, v := range views {
				switch v.State {
				case "found":
					found++
				case "empty":
					empty++
				case "error":
					failed++
				case "not-scanned":
					unscanned++
				}
			}
			if found+empty+failed+unscanned != len(views) {
				t.Errorf("%d of %d rows have a state the summary does not count",
					len(views)-(found+empty+failed+unscanned), len(views))
			}
			if anyNeedsAttention(views) != (empty+failed > 0) {
				t.Errorf("anyNeedsAttention is %v with %d empty and %d failing rows",
					anyNeedsAttention(views), empty, failed)
			}
		})
	}
}

// TestHomePathResolvesTheTilde checks the helper that makes the hint pasteable,
// including the case where it cannot be resolved — where the fallback must be the
// hint itself rather than a half-joined path.
func TestHomePathResolvesTheTilde(t *testing.T) {
	if got := homePath("/absolute/path"); got != "/absolute/path" {
		t.Errorf("homePath on an absolute path = %q, want it unchanged", got)
	}
	// A bare tilde with nothing after it is not a prefix this handles.
	if got := homePath("~"); got != "~" {
		t.Errorf("homePath(\"~\") = %q, want it unchanged: there is nothing to join", got)
	}
	resolved := homePath("~/.pi/agent/sessions")
	if strings.HasPrefix(resolved, "~") {
		t.Errorf("homePath did not expand a tilde-prefixed hint: %q", resolved)
	}
	if !strings.HasSuffix(resolved, filepath.Join(".pi", "agent", "sessions")) {
		t.Errorf("homePath = %q, want it to end in the hint's own path", resolved)
	}
}
