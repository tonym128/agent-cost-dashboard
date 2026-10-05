package web

import (
	"fmt"
	"html"
	"math"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

// The JSON below is the contract the front end reads. Field names match what
// assets/dashboard.js expects, so the page is unchanged from the previous
// implementation apart from being served from a database rather than a scan.

func totalsJSON(t store.Totals) map[string]any {
	return map[string]any{
		"totalCost":         t.Cost,
		"totalTokens":       t.TotalTokens,
		"totalInputTokens":  t.InputTokens,
		"totalOutputTokens": t.OutputTokens,
		"totalCacheRead":    t.CacheReadTokens,
		"totalCacheWrite":   t.CacheWriteTokens,
		"totalReasoning":    t.ReasoningTokens,
		"totalMessages":     t.Calls,
		"totalSessions":     t.Sessions,
		"totalProjects":     t.Projects,
		"totalLLMTime":      t.LLMSeconds,
		"totalToolTime":     t.ToolSeconds,
		"unpricedCalls":     t.UnpricedCalls,
		"firstActivity":     t.FirstTS,
		"lastActivity":      t.LastTS,
		// Tokens per second over the whole recorded history, weighted by the
		// time actually spent waiting on responses rather than averaged per call.
		"avgTokensPerSec": ratio(float64(t.OutputTokens), t.LLMSeconds),
	}
}

func dailyJSON(days []store.DayBucket) []map[string]any {
	out := make([]map[string]any, 0, len(days))
	for _, d := range days {
		out = append(out, map[string]any{
			"day":    d.Day,
			"cost":   d.Cost,
			"models": d.Models,
		})
	}
	return out
}

// activityJSON emits only non-empty buckets. The page fills the gaps itself, so
// shipping zeros would cost bytes proportional to the window rather than to the
// activity in it.
func activityJSON(buckets []store.ActivityBucket) []map[string]any {
	out := make([]map[string]any, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, map[string]any{
			"t":                  b.Start,
			"messages":           b.Calls,
			"input_tokens":       b.InputTokens,
			"output_tokens":      b.OutputTokens,
			"cache_read_tokens":  b.CacheReadTokens,
			"cache_write_tokens": b.CacheWriteTokens,
			"reasoning_tokens":   b.ReasoningTokens,
			"total_tokens":       b.TotalTokens,
			"llm_seconds":        b.LLMSeconds,
			"cost":               b.Cost,
			"unpriced":           b.Unpriced,
		})
	}
	return out
}

func modelsJSON(models []store.ModelStat, totalCost float64) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		// The share bar is the model's slice of the whole view, so it has to be
		// computed against the same filtered total the table is showing. An
		// all-local-model view costs nothing, and dividing by that would give
		// every row NaN.
		pct := 0.0
		if totalCost > 0 {
			pct = m.Cost / totalCost * 100
		}
		out = append(out, map[string]any{
			// The models table reads `name` and `tokens`; the store calls them
			// `model` and `total_tokens`. Both spellings ship so the table needs no
			// translation layer, and a duplicated string key is a few bytes.
			"name":               m.Model,
			"tokens":             m.TotalTokens,
			"model":              m.Model,
			"cost":               m.Cost,
			"messages":           m.Calls,
			"total_tokens":       m.TotalTokens,
			"input_tokens":       m.InputTokens,
			"output_tokens":      m.OutputTokens,
			"cache_read_tokens":  m.CacheReadTokens,
			"cache_write_tokens": m.CacheWriteTokens,
			"reasoning_tokens":   m.ReasoningTokens,
			"llm_time":           m.LLMSeconds,
			"avg_tps":            ratio(float64(m.OutputTokens), m.LLMSeconds),
			"first_ts":           m.FirstTS,
			"last_ts":            m.LastTS,
			"unpriced":           m.Unpriced,
			"pct":                pct,
		})
	}
	return out
}

// projectsJSON nests each project's sessions, models and tools.
//
// The page renders a project as a row that expands into that breakdown, so the
// three are one thing to read together rather than three separate queries the
// browser has to correlate.
//
// store.Projects() groups by (project, agent), so a directory used with two
// agents arrives as two rows. The page shows one row per project, so they are
// merged here: without that the table lists more projects than the headline
// "Projects" card counts, which is the same figure from two different queries.
func projectsJSON(
	projects []store.ProjectStat,
	sessions []store.SessionRow,
	projectModels []store.ProjectModel,
	projectTools []store.ProjectTool,
) []map[string]any {
	sessionsBy := map[string][]map[string]any{}
	for _, s := range sessions {
		sessionsBy[s.Project] = append(sessionsBy[s.Project], sessionsJSON([]store.SessionRow{s})[0])
	}
	modelsBy := map[string][]map[string]any{}
	for _, m := range projectModels {
		modelsBy[m.Project] = append(modelsBy[m.Project], map[string]any{
			"name":             m.Model,
			"cost":             m.Cost,
			"messages":         m.Calls,
			"tokens":           m.TotalTokens,
			"output_tokens":    m.OutputTokens,
			"avg_tps":          ratio(float64(m.OutputTokens), m.LLMSeconds),
			"llm_time_display": humanDuration(m.LLMSeconds),
		})
	}
	toolsBy := map[string][]map[string]any{}
	for _, t := range projectTools {
		avg := 0.0
		if t.Calls > 0 {
			avg = t.Seconds / float64(t.Calls)
		}
		toolsBy[t.Project] = append(toolsBy[t.Project], map[string]any{
			"name":             t.Tool,
			"calls":            t.Calls,
			"time":             t.Seconds,
			"errors":           t.Errors,
			"time_display":     humanDuration(t.Seconds),
			"avg_time_display": humanDuration(avg),
		})
	}

	// A map lookup miss is nil, and nil marshals to JSON null, which the page's
	// renderers then call .map on. Every list a project carries is therefore
	// guaranteed non-nil.
	orEmpty := func(v []map[string]any) []map[string]any {
		if v == nil {
			return []map[string]any{}
		}
		return v
	}

	out := make([]map[string]any, 0, len(projects))
	index := map[string]int{}
	for _, p := range projects {
		llm := 0.0
		tool := 0.0
		tokens := int64(0)
		for _, s := range sessions {
			if s.Project != p.Project {
				continue
			}
			llm += s.LLMSeconds
			tool += s.ToolSeconds
			tokens += s.TotalTokens
		}
		row := map[string]any{
			"name":                  p.Project,
			"agent_cmd":             p.Agent,
			"agents":                []string{p.Agent},
			"cost":                  p.Cost,
			"sessions":              p.Sessions,
			"messages":              p.Calls,
			"tokens":                tokens,
			"avg_tps":               projectThroughput(projectModels, p.Project),
			"llm_time":              llm,
			"tool_time":             tool,
			"llm_time_display":      humanDuration(llm),
			"tool_time_display":     humanDuration(tool),
			"last_activity":         p.LastTS,
			"last_activity_display": displayTime(p.LastTS),
			"sessions_list":         orEmpty(sessionsBy[p.Project]),
			"models":                orEmpty(modelsBy[p.Project]),
			"tools":                 orEmpty(toolsBy[p.Project]),
		}
		if i, ok := index[p.Project]; ok {
			// A second agent in the same directory folds into the row already
			// emitted. The session-derived figures are keyed on the project
			// alone and so are already counted once; only the per-agent sums
			// are added.
			mergeProjectRow(out[i], p, llm, tool)
			continue
		}
		index[p.Project] = len(out)
		out = append(out, row)
	}
	return out
}

// mergeProjectRow folds another agent's row for the same project into the row
// the page already has.
func mergeProjectRow(row map[string]any, p store.ProjectStat, llm, tool float64) {
	row["cost"] = row["cost"].(float64) + p.Cost
	row["sessions"] = row["sessions"].(int) + p.Sessions
	row["messages"] = row["messages"].(int) + p.Calls
	row["llm_time"] = row["llm_time"].(float64) + llm
	row["tool_time"] = row["tool_time"].(float64) + tool
	row["llm_time_display"] = humanDuration(row["llm_time"].(float64))
	row["tool_time_display"] = humanDuration(row["tool_time"].(float64))
	if p.LastTS > row["last_activity"].(int64) {
		row["last_activity"] = p.LastTS
		row["last_activity_display"] = displayTime(p.LastTS)
	}
	agents := row["agents"].([]string)
	for _, a := range agents {
		if a == p.Agent {
			return
		}
	}
	row["agents"] = append(agents, p.Agent)
}

// projectThroughput weights a project's token rate by the time actually spent
// waiting, matching how the global figure is computed.
func projectThroughput(models []store.ProjectModel, project string) float64 {
	var output, seconds float64
	for _, m := range models {
		if m.Project == project {
			output += float64(m.OutputTokens)
			seconds += m.LLMSeconds
		}
	}
	return ratio(output, seconds)
}

// displayTime renders an epoch for the table, falling back to a dash rather than
// 1970 for a session that never recorded a time.
func displayTime(ts int64) string {
	if ts == 0 {
		return "unknown"
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04")
}

func toolsJSON(tools []store.ToolStat, totalSeconds float64) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		avg := 0.0
		if t.Calls > 0 {
			avg = t.Seconds / float64(t.Calls)
		}
		pct := 0.0
		if totalSeconds > 0 {
			pct = t.Seconds / totalSeconds * 100
		}
		out = append(out, map[string]any{
			"name":             t.Tool,
			"tool":             t.Tool,
			"calls":            t.Calls,
			"time":             t.Seconds,
			"errors":           t.Errors,
			"cost":             t.Cost,
			"time_display":     humanDuration(t.Seconds),
			"avg_time_display": humanDuration(avg),
			// avg_seconds is the same figure as a number, so the browser can
			// sort the Avg Time column rather than its formatted string.
			"avg_seconds": avg,
			"pct":         pct,
		})
	}
	return out
}

func sessionsJSON(sessions []store.SessionRow) []map[string]any {
	out := make([]map[string]any, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, map[string]any{
			"uid":                s.UID,
			"agent_cmd":          s.Agent,
			"cwd":                s.Project,
			"path":               s.Path,
			"title":              s.Title,
			"messages":           s.Calls,
			"tokens":             s.TotalTokens,
			"input_tokens":       s.InputTokens,
			"output_tokens":      s.OutputTokens,
			"cache_read_tokens":  s.CacheReadTokens,
			"cache_write_tokens": s.CacheWriteTokens,
			"reasoning_tokens":   s.ReasoningTokens,
			"cost":               s.Cost,
			"llm_time":           s.LLMSeconds,
			"tool_time":          s.ToolSeconds,
			"tool_calls":         s.ToolCalls,
			"tool_errors":        s.ToolErrors,
			"start":              s.FirstTS,
			"end":                s.LastTS,
			"duration":           s.WallSeconds,
			// Sessions that never called a model have no throughput figure;
			// reporting zero would look like an infinitely fast one.
			"avg_tps":           ratio(float64(s.OutputTokens), s.LLMSeconds),
			"start_display":     displayTime(s.FirstTS),
			"end_display":       displayTime(s.LastTS),
			"duration_display":  humanDuration(s.WallSeconds),
			"llm_time_display":  humanDuration(s.LLMSeconds),
			"tool_time_display": humanDuration(s.ToolSeconds),
		})
	}
	return out
}

// ratio divides, returning 0 rather than NaN or infinity when the denominator
// is zero — which happens for every empty window and every session with no
// recorded timings.
func ratio(a, b float64) float64 {
	if b == 0 || math.IsNaN(b) || math.IsInf(b, 0) {
		return 0
	}
	v := a / b
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// ---------------------------------------------------------------- session view

func sessionView(s store.SessionRow) map[string]any {
	return map[string]any{
		"UID":      html.EscapeString(s.UID),
		"Agent":    html.EscapeString(s.Agent),
		"Project":  html.EscapeString(s.Project),
		"Path":     html.EscapeString(s.Path),
		"Title":    html.EscapeString(s.Title),
		"Calls":    s.Calls,
		"Tokens":   s.TotalTokens,
		"Cost":     fmt.Sprintf("$%.2f", s.Cost),
		"LLMTime":  humanDuration(s.LLMSeconds),
		"ToolTime": humanDuration(s.ToolSeconds),
		"Wall":     humanDuration(s.WallSeconds),
		"First":    formatUnix(s.FirstTS),
		"Last":     formatUnix(s.LastTS),
	}
}

func callViews(calls []store.CallRow) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, c := range calls {
		out = append(out, map[string]any{
			"Time":       formatUnix(c.TS),
			"Model":      html.EscapeString(c.Model),
			"Input":      groupInt(c.InputTokens),
			"Output":     groupInt(c.OutputTokens),
			"CacheRead":  groupInt(c.CacheReadTokens),
			"CacheWrite": groupInt(c.CacheWriteTokens),
			"Reasoning":  groupInt(c.ReasoningTokens),
			"Total":      groupInt(c.TotalTokens),
			"LLMTime":    humanDuration(c.LLMSeconds),
			"Cost":       fmt.Sprintf("$%.4f", c.Cost),
			"Priced":     c.Priced,
		})
	}
	return out
}

// ---------------------------------------------------------------- formatters

func formatUnix(ts int64) string {
	if ts == 0 {
		return "unknown"
	}
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04:05")
}

func humanDuration(secs float64) string {
	if secs <= 0 {
		return "0s"
	}
	if secs < 1 {
		return fmt.Sprintf("%.0fms", secs*1000)
	}
	total := int64(secs)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

func humanAge(d time.Duration) string {
	switch {
	case d < 0:
		return "just now"
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// groupInt renders a count with thousands separators without pulling in a
// locale-dependent formatter.
func groupInt(n int64) string {
	return groupInt64(n)
}

func groupInt64(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var out []byte
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// ---------------------------------------------------------------- stat cards

// statCard is one headline figure.
type statCard struct {
	Label string
	Value string
	// Detail is an optional second block: the token breakdown under the total, or
	// a caveat beside a figure that is only partly meaningful.
	Detail []statDetail
	// DetailClass picks the layout for Detail. The token breakdown is a
	// two-column grid; a one-line caveat is not.
	DetailClass string
	// Wide makes the card span two columns, which the token breakdown needs to
	// fit its parts side by side.
	Wide bool
	// Class tints the value, used to keep cost green and time purple as before.
	Class string
}

type statDetail struct {
	Label string
	Value string
}

// statCards builds the row of headline figures.
//
// Rendered on the server rather than in the page script because they are a
// function of the whole filtered view: every number here is a total the queries
// already computed, and doing it server-side keeps them consistent with the
// tables below, which are drawn from the same rows.
func statCards(t store.Totals) []statCard {
	cards := []statCard{
		{Label: "Total Cost", Value: fmt.Sprintf("$%.2f", t.Cost), Class: "cost"},
		{Label: "Projects", Value: groupInt64(int64(t.Projects))},
		{Label: "Sessions", Value: groupInt64(int64(t.Sessions))},
		{Label: "LLM Calls", Value: groupInt64(int64(t.Calls))},
		{
			Label:       "Total Tokens",
			Value:       formatCompact(t.TotalTokens),
			Detail:      tokenDetailRows(t),
			DetailClass: "token-breakdown",
			Wide:        true,
		},
		{Label: "LLM Time", Value: humanDuration(t.LLMSeconds), Class: "llm"},
		{Label: "Tool Time", Value: humanDuration(t.ToolSeconds), Class: "tool"},
		{Label: "Avg Tokens/s", Value: fmt.Sprintf("%.1f", ratio(float64(t.OutputTokens), t.LLMSeconds)), Class: "tps"},
	}

	// A view whose cost is missing for most calls would otherwise show a
	// confident $0.00. Saying so is the difference between "free" and "unknown".
	if t.UnpricedCalls > 0 {
		cards = append(cards, statCard{
			Label: "Unpriced Calls",
			Value: groupInt64(int64(t.UnpricedCalls)),
			Detail: []statDetail{{
				Label: "tokens counted, cost $0 — no rate found for these models",
			}},
			DetailClass: "stat-note",
			Class:       "unpriced",
		})
	}
	return cards
}

// tokenDetailRows splits a token total the way the summary table does, so the
// headline figure can be checked against its parts without opening anything.
func tokenDetailRows(t store.Totals) []statDetail {
	rows := []statDetail{
		{"Input", formatCompact(t.InputTokens)},
		{"Output", formatCompact(t.OutputTokens)},
	}
	if t.CacheReadTokens > 0 {
		rows = append(rows, statDetail{"Cache read", formatCompact(t.CacheReadTokens)})
	}
	if t.CacheWriteTokens > 0 {
		rows = append(rows, statDetail{"Cache write", formatCompact(t.CacheWriteTokens)})
	}
	if t.ReasoningTokens > 0 {
		rows = append(rows, statDetail{"Reasoning", formatCompact(t.ReasoningTokens)})
	}
	return rows
}

// formatCompact renders a count the way the tables do: grouped when short,
// abbreviated when long, so a column of nine-figure token counts stays readable.
func formatCompact(n int64) string {
	switch {
	case n < 1000:
		return groupInt64(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	}
}
