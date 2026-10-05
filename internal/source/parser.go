package source

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tonym128/agent-cost-dashboard/internal/model"
)

// Parser extracts one session's worth of activity from one source file.
//
// The contract is written around incrementality. Parse is handed the cursor from
// the previous scan and returns a new one, and is expected to read only what
// lies beyond it. Call that has already been ingested is a caller error, not
// something the parser guards against.
type Parser interface {
	// Agent is the filter value this source contributes to.
	Agent() string

	// SessionUID identifies the session a file holds. It must be stable across
	// scans for the same file, because it is the key every stored row hangs
	// off; a value derived from a per-scan timestamp or uuid would duplicate
	// the session on every pass.
	SessionUID(path string) string

	// Parse reads the source and returns what it found plus the cursor to
	// resume from. `partial` says the previous scan only consumed part of the
	// file, so the result may be an increment rather than the whole session.
	Parse(path string, prev model.ScanState, pricer *Pricer) (model.SessionWrite, model.ScanState, error)
}

// StableKey builds a deterministic id from a path.
//
// Some logs carry no session id of their own. A fresh uuid per scan would insert
// a duplicate copy of the same session every pass, so the path — stable for as
// long as the file exists — is hashed instead.
func StableKey(path string, agent string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return agent + ":" + shortHash(abs)
}

// sessionBuilder accumulates a session as its lines are read.
type sessionBuilder struct {
	uid     string
	agent   string
	project string
	title   string

	calls     []model.Call
	toolCalls []model.ToolCall

	firstTS, lastTS string // ISO, kept as the first seen

	// ordinal supplies a fallback call key for logs that provide no id.
	ordinal int64

	// prevTotals holds the last running usage total seen. Codex reports usage
	// cumulatively rather than per call, so a call's usage is the difference
	// between consecutive totals; the builder has to carry the previous one.
	prevTotals map[string]any

	// prevTotalsHook recovers prevTotals for a parser resuming mid-file, where
	// the total the increment must be measured against was recorded before the
	// cursor and so is not in memory. It is called at most once, and only if a
	// record actually needs the baseline — an incremental read of an unchanged
	// file pays nothing.
	prevTotalsHook func() map[string]any
}

// baselineTotals returns the running total this record's usage is differenced
// against, recovering it from the log when the read resumed past it.
func (b *sessionBuilder) baselineTotals() map[string]any {
	if len(b.prevTotals) > 0 || b.prevTotalsHook == nil {
		return b.prevTotals
	}
	if recovered := b.prevTotalsHook(); len(recovered) > 0 {
		b.prevTotals = recovered
	}
	return b.prevTotals
}

func newSessionBuilder(uid, agent, project string) *sessionBuilder {
	return &sessionBuilder{uid: uid, agent: agent, project: project}
}

// setProject records the working directory, keeping the first non-empty value:
// every session in a project runs in the same place, and the first is as good a
// source as any.
func (b *sessionBuilder) setProject(p string) {
	if p != "" && b.project == "" {
		b.project = p
	}
}

func (b *sessionBuilder) observeTime(tsISO string) {
	if tsISO == "" {
		return
	}
	if b.firstTS == "" {
		b.firstTS = tsISO
	}
	b.lastTS = tsISO
}

// addCall normalises and stores one call, applying pricing.
//
// Everything a call carries goes through here, which is what keeps the token
// arithmetic identical across six parsers written against six different log
// formats: reasoning is carved out of output rather than added to it, totals are
// computed from the parts, and negatives are impossible by construction.
func (b *sessionBuilder) addCall(c model.Call) {
	if c.TotalTokens == 0 {
		c.TotalTokens = c.InputTokens + c.OutputTokens + c.CacheReadTokens + c.CacheWriteTokens
	}
	if c.CallKey == "" {
		// No id in the log: fall back to position. Ordinals are counted over the
		// whole file, so this key is stable across incremental scans.
		c.CallKey = fmt.Sprintf("n%d", b.ordinal)
	}
	b.ordinal++
	c.SessionUID = b.uid
	c.Agent = b.agent
	c.Project = b.project
	if !c.Time.IsZero() {
		c.Day = c.Time.Local().Format("2006-01-02")
		b.observeTime(c.Time.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
	}
	b.calls = append(b.calls, c)
}

func (b *sessionBuilder) addTool(t model.ToolCall) {
	if t.CallKey == "" {
		t.CallKey = fmt.Sprintf("t%d", len(b.toolCalls))
	}
	t.SessionUID = b.uid
	t.Agent = b.agent
	t.Project = b.project
	b.toolCalls = append(b.toolCalls, t)
}

func (b *sessionBuilder) build(path string) model.SessionWrite {
	sw := model.SessionWrite{
		Session:   model.Session{UID: b.uid, Agent: b.agent, Project: b.project, Title: b.title},
		Path:      path,
		Calls:     b.calls,
		ToolCalls: b.toolCalls,
		FirstTS:   parseTime(b.firstTS),
		LastTS:    parseTime(b.lastTS),
	}
	return sw
}

// price fills in CostUSD and Priced for a call, given the token counts already
// on it. Kept as a free function so every parser applies it identically.
func price(pricer *Pricer, c *model.Call) {
	if pricer == nil {
		return
	}
	c.CostUSD, c.Priced = pricer.Cost(c.Model, c.InputTokens, c.OutputTokens, c.CacheReadTokens, c.CacheWriteTokens)
}

// priceGenerated prices a call whose billable output is larger than the figure
// shown on it.
//
// Antigravity reports one generated count of which reasoning is a slice. The
// dashboard itemises reasoning separately, so `OutputTokens` holds only the
// non-reasoning remainder — but the whole generated count is billed at the
// output rate. Pricing the itemised field would quietly drop the reasoning
// spend, which is the difference between a number that looks plausible and one
// that is right.
func priceGenerated(pricer *Pricer, c *model.Call, generated int) {
	if pricer == nil {
		return
	}
	c.CostUSD, c.Priced = pricer.Cost(c.Model, c.InputTokens, generated, c.CacheReadTokens, c.CacheWriteTokens)
}

// visibleOutput splits a generated count into its reasoning and non-reasoning
// parts.
//
// Several agents report "output" as the total generated, of which reasoning is
// a slice. Billing needs the whole thing at the output rate while the itemised
// display needs the remainder, so both are kept. The subtraction is clamped:
// reasoning exceeding the generated count is a bad blob, and letting it go
// negative would corrupt every total downstream.
func visibleOutput(generated, reasoning int64) (visible int64, reasoningOut int64) {
	if reasoning < 0 {
		reasoning = 0
	}
	if reasoning > generated {
		reasoning = generated
	}
	return generated - reasoning, reasoning
}

// stem returns a file's name without extension, which is the session id for the
// agents that name their files after the conversation.
func stem(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
