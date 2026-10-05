package web

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// Source detection.
//
// The value proposition is that dashd reads six agents' private log formats. On
// a machine where it cannot, the page used to look exactly like a machine with
// no usage: seven cards at $0.00 and four empty tables. That is the one state
// where being wrong is most expensive, because the reader concludes the tool does
// not work rather than that the logs were not found.
//
// The panel is built from scan_status, which records a row per agent per pass
// whether or not anything was found. `files_seen` is therefore the honest test
// for "found": a row with zero files means the agent's log directory was walked
// and was empty or absent, which is a different fact from "never scanned".

// knownSources is the set of agents this build can read, with the log location
// each is looked for in.
//
// The paths mirror scan.DefaultSources: they are shown so a miss can be acted
// on, and they are the first thing to check when an agent reports nothing. They
// are duplicated rather than imported because the scanner resolves its roots
// against a configurable -home that the web layer does not know; showing the
// default location is what is useful, and the scanner section below reports what
// was actually walked.
var knownSources = []struct {
	Agent string
	// Hint is where this agent keeps its session logs, relative to $HOME.
	Hint string
	// Note is why a miss here is usually not a dashd bug.
	Note string
}{
	{model.AgentPi, "~/.pi/agent/sessions", "pi writes one JSONL file per session."},
	{model.AgentClaude, "~/.claude/projects", "One directory per project, one JSONL file per session."},
	{model.AgentCodex, "~/.codex/sessions", "JSONL, sharded by date."},
	{model.AgentGemini, "~/.gemini", "JSONL under the agent's own directory."},
	{model.AgentAgy, "~/.gemini/antigravity/conversations", "SQLite rather than JSONL, so a missing file simply yields no rows."},
	{model.AgentOpencode, "~/.local/share/opencode/opencode.db", "A single SQLite database holds every session."},
}

// sourceView is one agent's detection result.
type sourceView struct {
	Agent string
	// Hint is the default log location, resolved to an absolute path so it can
	// be pasted into a shell.
	Hint string
	// State is a slug used for the row's CSS class: found, empty, error or
	// not-scanned. It is a slug rather than a phrase because the template puts
	// it straight into a class attribute, where "not scanned" is two classes
	// and one that matches nothing.
	State string
	// Status is the same thing written out for the reader.
	Status string
	// Detail explains the state in a sentence.
	Detail string
	// LastScanAgo is how long ago the source was walked; empty if never.
	LastScanAgo string
	// FilesSeen and CallsAdded are the scan's own counters, which is what makes
	// "walked and found nothing" distinguishable from "found nothing because it
	// failed".
	FilesSeen  int
	CallsAdded int
	Error      string
	// Attention marks a source the reader can act on: it failed, or it was
	// walked and nothing was there. It is a field rather than a method because
	// the template reads it.
	Attention bool
}

// sourceViews pairs the known agent list against what the last scan recorded.
//
// A known agent with no scan_status row has never been walked, which is only
// true before the first scan or if the scanner was not given that source; the
// page says so rather than implying its logs are missing.
func sourceViews(statuses []scanStatusView) []sourceView {
	byAgent := map[string]scanStatusView{}
	for _, st := range statuses {
		byAgent[st.Agent] = st
	}
	anyScan := len(statuses) > 0

	out := make([]sourceView, 0, len(knownSources))
	for _, k := range knownSources {
		v := sourceView{Agent: k.Agent, Hint: homePath(k.Hint)}
		st, ok := byAgent[k.Agent]
		if !ok {
			v.State = "not-scanned"
			v.Status = "not scanned"
			if anyScan {
				v.Detail = "Not part of the last scan."
			} else {
				v.Detail = "No scan has run yet, so nothing has been looked for."
			}
			out = append(out, v)
			continue
		}
		v.FilesSeen = st.FilesSeen
		v.CallsAdded = st.CallsAdded
		v.Error = st.Error
		v.LastScanAgo = st.LastScanAgo
		switch {
		case st.Error != "":
			v.State = "error"
			v.Status = "error"
			v.Detail = "The scan failed: " + st.Error
		case st.FilesSeen > 0:
			v.State = "found"
			v.Status = "found"
			v.Detail = fmt.Sprintf("%s log files seen, %s calls added by the last scan.",
				groupInt64(int64(st.FilesSeen)), groupInt64(int64(st.CallsAdded)))
		default:
			v.State = "empty"
			v.Status = "no logs"
			v.Detail = "No log files found under " + k.Hint + ". " + k.Note
		}
		v.Attention = v.State == "error" || v.State == "empty"
		out = append(out, v)
	}
	return out
}

// sourceSummary is the one-line verdict above the table.
func sourceSummary(views []sourceView) string {
	if len(views) == 0 {
		return "No sources are configured."
	}
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
	parts := []string{fmt.Sprintf("%d of %d sources found", found, len(views))}
	if empty > 0 {
		parts = append(parts, fmt.Sprintf("%d with no logs", empty))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failing", failed))
	}
	if unscanned > 0 {
		parts = append(parts, fmt.Sprintf("%d not scanned", unscanned))
	}
	return strings.Join(parts, " · ")
}

// anyNeedsAttention reports whether the panel has anything actionable in it,
// which is what decides whether the panel is worth showing at all.
func anyNeedsAttention(views []sourceView) bool {
	for _, v := range views {
		if v.Attention {
			return true
		}
	}
	return false
}

// homePath renders a `~`-prefixed hint as an absolute path so it can be pasted
// into a shell. A home directory that cannot be resolved falls back to the hint,
// which is not worth failing a page render over.
func homePath(hint string) string {
	if !strings.HasPrefix(hint, "~/") {
		return hint
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return hint
	}
	return filepath.Join(home, strings.TrimPrefix(hint, "~/"))
}
