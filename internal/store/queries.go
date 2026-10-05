package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Filter is the set of axes the dashboard can narrow by. Empty means no filter.
//
// Values are matched by the same normalisation the parsers apply to model names,
// so a filter typed by hand matches what the page displays.
type Filter struct {
	Models   []string
	Agents   []string
	Projects []string
	DateFrom *time.Time
	DateTo   *time.Time
}

// whereExtra builds the WHERE clause with additional leading bounds, in the
// order given, and returns arguments to match.
//
// The window bounds a query imposes are folded into the same clause list rather
// than spliced onto the front of a finished string. Splicing is how the
// activity query ended up emitting "ts <= ?AND ts >= ?": TrimPrefix removed the
// space along with the keyword, and a filter turned a working query into a
// syntax error.
func (f Filter) whereExtra(colPrefix, tsCol string, bounds ...string) (string, []any) {
	var clauses []string
	var args []any

	p := func(col string) string {
		if colPrefix == "" {
			return col
		}
		return colPrefix + "." + col
	}

	clauses = append(clauses, bounds...)

	if len(f.Models) > 0 {
		clauses = append(clauses, p("model")+" IN ("+inPlaceholders(len(f.Models))+")")
		for _, m := range f.Models {
			args = append(args, m)
		}
	}
	if len(f.Agents) > 0 {
		clauses = append(clauses, p("agent")+" IN ("+inPlaceholders(len(f.Agents))+")")
		for _, a := range f.Agents {
			args = append(args, a)
		}
	}
	if len(f.Projects) > 0 {
		clauses = append(clauses, p("project")+" IN ("+inPlaceholders(len(f.Projects))+")")
		for _, pr := range f.Projects {
			args = append(args, pr)
		}
	}
	// A record with no timestamp is excluded from a date window rather than
	// folded into the earliest day, which would invent activity on a date the
	// log never claimed.
	if tsCol != "" {
		if f.DateFrom != nil {
			clauses = append(clauses, p(tsCol)+" >= ? AND "+p(tsCol)+" > 0")
			args = append(args, f.DateFrom.Unix())
		}
		if f.DateTo != nil {
			clauses = append(clauses, p(tsCol)+" <= ? AND "+p(tsCol)+" > 0")
			args = append(args, f.DateTo.Unix())
		}
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// where builds the shared WHERE clause for a filter.
//
// Every query that reads through a filter goes through here, so a filter cannot
// be accidentally half-applied to one table and not another — which is how a
// "filtered" view ends up disagreeing with its own totals.
//
// tsCol names the timestamp column in the table being filtered. The call table
// stores `ts`; the session table stores `first_ts`/`last_ts` and has no `ts`
// at all, so a date clause built for one and applied to the other is a query
// error rather than a wrong number.
func (f Filter) where(colPrefix, tsCol string) (string, []any) {
	return f.whereExtra(colPrefix, tsCol)
}

// whereSession builds the clause for the session table.
//
// The session table carries agent, project and timestamps but has no model
// column, so a model filter cannot be applied to it directly. It is expressed as
// an EXISTS against the calls the session is built from, which is both the only
// correct translation and the same definition the session's own figures use: a
// session matches if any of its calls did.
func (f Filter) whereSession(colPrefix string) (string, []any) {
	var clauses []string
	var args []any

	p := func(col string) string {
		if colPrefix == "" {
			return col
		}
		return colPrefix + "." + col
	}

	if len(f.Models) > 0 {
		clauses = append(clauses,
			"EXISTS (SELECT 1 FROM call mc WHERE mc.session_uid = "+p("uid")+
				" AND mc.model IN ("+inPlaceholders(len(f.Models))+"))")
		for _, m := range f.Models {
			args = append(args, m)
		}
	}
	if len(f.Agents) > 0 {
		clauses = append(clauses, p("agent")+" IN ("+inPlaceholders(len(f.Agents))+")")
		for _, a := range f.Agents {
			args = append(args, a)
		}
	}
	if len(f.Projects) > 0 {
		clauses = append(clauses, p("project")+" IN ("+inPlaceholders(len(f.Projects))+")")
		for _, pr := range f.Projects {
			args = append(args, pr)
		}
	}
	if f.DateFrom != nil {
		clauses = append(clauses, p("last_ts")+" >= ? AND "+p("last_ts")+" > 0")
		args = append(args, f.DateFrom.Unix())
	}
	if f.DateTo != nil {
		clauses = append(clauses, p("last_ts")+" <= ? AND "+p("last_ts")+" > 0")
		args = append(args, f.DateTo.Unix())
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// Totals is the headline figure set for the whole database or a filtered view.
type Totals struct {
	Cost             float64
	Calls            int
	TotalTokens      int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	LLMSeconds       float64
	ToolSeconds      float64
	Sessions         int
	Projects         int
	Agents           int
	// UnpricedCalls counts calls whose model had no known rate. Surfaced rather
	// than folded into the total, so a $0.00 that means "unknown" is never read
	// as "free".
	UnpricedCalls int
	// UnpricedCost is what those calls would have cost had a rate been known.
	// Zero: only the count is knowable.
	FirstTS int64
	LastTS  int64
}

// Totals computes the headline figures.
func (s *Store) Totals(ctx context.Context, f Filter) (Totals, error) {
	where, args := f.where("", "ts")
	q := `
		SELECT COALESCE(SUM(cost_usd),0), COUNT(*),
		       COALESCE(SUM(total_tokens),0), COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(reasoning_tokens),0),
		       COALESCE(SUM(llm_seconds),0),
		       COALESCE(SUM(CASE WHEN priced = 0 THEN 1 ELSE 0 END),0),
		       COALESCE(MIN(NULLIF(ts,0)),0), COALESCE(MAX(ts),0)
		FROM call` + where
	var t Totals
	err := s.db.QueryRowContext(ctx, q, args...).Scan(
		&t.Cost, &t.Calls, &t.TotalTokens, &t.InputTokens, &t.OutputTokens,
		&t.CacheReadTokens, &t.CacheWriteTokens, &t.ReasoningTokens, &t.LLMSeconds,
		&t.UnpricedCalls, &t.FirstTS, &t.LastTS)
	if err != nil {
		return t, fmt.Errorf("totals: %w", err)
	}

	// Sessions and projects come from the session table, filtered the same way.
	sw, sargs := f.whereSession("s")
	row := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT project), COUNT(DISTINCT agent) FROM session s`+sw, sargs...)
	if err := row.Scan(&t.Sessions, &t.Projects, &t.Agents); err != nil {
		return t, fmt.Errorf("totals sessions: %w", err)
	}
	t.ToolSeconds = s.toolSeconds(ctx, f)
	return t, nil
}

func (s *Store) toolSeconds(ctx context.Context, f Filter) float64 {
	w, args := f.where("", "ts")
	var v sql.NullFloat64
	q := `SELECT SUM(t.seconds) FROM tool_call t` + w
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
		return 0
	}
	return v.Float64
}

// DayBucket is one calendar day of activity.
type DayBucket struct {
	Day  string
	Cost float64
	// Models maps model name to that day's cost, for the stacked bar.
	Models map[string]float64
}

// dailySQL assembles the daily rollup. The filter clause is spliced in before
// the grouping, so the chart is built from exactly the rows the filter selected.
//
// The statement is a named function rather than an inline literal so that the
// index-usage test can EXPLAIN the same text the page runs.
func dailySQL(where string) string {
	return `
		SELECT day, model, COALESCE(SUM(cost_usd),0) AS cost, COUNT(*)
		FROM call` + where + `
		GROUP BY day, model ORDER BY day`
}

// Daily returns one bucket per day with any activity.
//
// The day string is produced by the database from the call's own local date, so
// grouping matches what the page shows rather than being recomputed in Go and
// risking a timezone disagreement.
func (s *Store) Daily(ctx context.Context, f Filter) ([]DayBucket, error) {
	where, args := f.where("", "ts")
	rows, err := s.db.QueryContext(ctx, dailySQL(where), args...)
	if err != nil {
		return nil, fmt.Errorf("daily: %w", err)
	}
	defer rows.Close()

	byDay := map[string]*DayBucket{}
	var order []string
	for rows.Next() {
		var day, model string
		var cost float64
		var calls int
		if err := rows.Scan(&day, &model, &cost, &calls); err != nil {
			return nil, err
		}
		b, ok := byDay[day]
		if !ok {
			b = &DayBucket{Day: day, Models: map[string]float64{}}
			byDay[day] = b
			order = append(order, day)
		}
		b.Cost += cost
		if model != "" {
			b.Models[model] += cost
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]DayBucket, 0, len(order))
	for _, day := range order {
		out = append(out, *byDay[day])
	}
	return out, nil
}

// ActivityBucket is one fixed-width time bucket of activity.
//
// This is the series behind the throughput chart. It is queried from the call
// table rather than accumulated at scan time, which is the whole reason a window
// wider than the retention horizon still works: the underlying rows are kept
// forever, so the query can reach back as far as they do.
type ActivityBucket struct {
	Start            int64
	Calls            int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	TotalTokens      int64
	LLMSeconds       float64
	Cost             float64
	Unpriced         int
}

// HistoryRange returns the span the database actually covers, so a window can
// be clamped to something real rather than to an arbitrary horizon.
func (s *Store) HistoryRange(ctx context.Context, f Filter) (oldest, newest time.Time, err error) {
	w, args := f.where("", "ts")
	var o, n sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MIN(NULLIF(ts,0)), MAX(ts) FROM call"+w, args...).Scan(&o, &n); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if o.Valid {
		oldest = time.Unix(o.Int64, 0)
	}
	if n.Valid {
		newest = time.Unix(n.Int64, 0)
	}
	return oldest, newest, nil
}

// Activity returns buckets of `step` seconds covering [from, to].
//
// Only non-empty buckets are returned. The gap-filling is left to the caller so
// the payload stays proportional to the activity rather than to the window.
//
// The window is expressed as a pair of instants rather than "the last N hours
// from now", because the newest call in the database may be days old: a
// dashboard whose data stops on Tuesday should chart Tuesday, not today.
func (s *Store) Activity(ctx context.Context, f Filter, from, to time.Time, step int64) ([]ActivityBucket, error) {
	if step <= 0 {
		return nil, fmt.Errorf("activity step must be positive, got %d", step)
	}
	// The window bounds are part of the same clause list as the filter, and come
	// first so their arguments lead.
	where, args := f.whereExtra("", "ts", "ts >= ?", "ts <= ?")
	args = append([]any{step, step, from.Unix(), to.Unix()}, args...)
	q := `
		SELECT (ts / ?) * ?                       AS bucket,
		       COUNT(*),
		       COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_write_tokens),0),
		       COALESCE(SUM(reasoning_tokens),0),
		       COALESCE(SUM(total_tokens),0),
		       COALESCE(SUM(llm_seconds),0),
		       COALESCE(SUM(cost_usd),0),
		       COALESCE(SUM(CASE WHEN priced = 0 THEN 1 ELSE 0 END),0)
		FROM call` + where + `
		GROUP BY bucket ORDER BY bucket`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("activity: %w", err)
	}
	defer rows.Close()

	var out []ActivityBucket
	for rows.Next() {
		var b ActivityBucket
		if err := rows.Scan(&b.Start, &b.Calls, &b.InputTokens, &b.OutputTokens,
			&b.CacheReadTokens, &b.CacheWriteTokens, &b.ReasoningTokens,
			&b.TotalTokens, &b.LLMSeconds, &b.Cost, &b.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ModelStat is one model's totals.
type ModelStat struct {
	Model            string
	Cost             float64
	Calls            int
	TotalTokens      int64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	LLMSeconds       float64
	FirstTS          int64
	LastTS           int64
	Unpriced         int
}

// modelsSQL assembles the per-model rollup behind the models table.
func modelsSQL(where string) string {
	return `
		SELECT model, COALESCE(SUM(cost_usd),0), COUNT(*),
		       COALESCE(SUM(total_tokens),0), COALESCE(SUM(input_tokens),0),
		       COALESCE(SUM(output_tokens),0), COALESCE(SUM(cache_read_tokens),0),
		       COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(reasoning_tokens),0),
		       COALESCE(SUM(llm_seconds),0),
		       COALESCE(MIN(NULLIF(ts,0)),0), COALESCE(MAX(ts),0),
		       COALESCE(SUM(CASE WHEN priced = 0 THEN 1 ELSE 0 END),0)
		FROM call` + where + `
		GROUP BY model ORDER BY SUM(cost_usd) DESC, model`
}

// Models returns per-model totals, most expensive first.
func (s *Store) Models(ctx context.Context, f Filter) ([]ModelStat, error) {
	where, args := f.where("", "ts")
	rows, err := s.db.QueryContext(ctx, modelsSQL(where), args...)
	if err != nil {
		return nil, fmt.Errorf("models: %w", err)
	}
	defer rows.Close()
	var out []ModelStat
	for rows.Next() {
		var m ModelStat
		if err := rows.Scan(&m.Model, &m.Cost, &m.Calls, &m.TotalTokens, &m.InputTokens,
			&m.OutputTokens, &m.CacheReadTokens, &m.CacheWriteTokens,
			&m.ReasoningTokens, &m.LLMSeconds, &m.FirstTS, &m.LastTS, &m.Unpriced); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProjectStat is one project's totals.
type ProjectStat struct {
	Project  string
	Agent    string
	Cost     float64
	Calls    int
	Sessions int
	FirstTS  int64
	LastTS   int64
}

// Projects returns per-project totals.
func (s *Store) Projects(ctx context.Context, f Filter) ([]ProjectStat, error) {
	where, args := f.whereSession("s")
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.project, s.agent,
		       COALESCE(SUM(s.cost_usd),0), COALESCE(SUM(s.calls),0),
		       COUNT(*), COALESCE(MIN(NULLIF(s.first_ts,0)),0), COALESCE(MAX(s.last_ts),0)
		FROM session s`+where+`
		GROUP BY s.project, s.agent
		ORDER BY MAX(s.last_ts) DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("projects: %w", err)
	}
	defer rows.Close()
	var out []ProjectStat
	for rows.Next() {
		var p ProjectStat
		if err := rows.Scan(&p.Project, &p.Agent, &p.Cost, &p.Calls, &p.Sessions,
			&p.FirstTS, &p.LastTS); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// toolsSQL assembles the per-tool rollup, including the cost attribution seek.
//
// cost_usd is in call_session_ts so the seek is answered from that index alone;
// without it every tool row was a rescan of its session's calls.
func toolsSQL(where string) string {
	return `
		SELECT t.tool, COUNT(*), COALESCE(SUM(t.seconds),0),
		       COALESCE(SUM(t.is_error),0),
		       COALESCE(SUM((
		           SELECT c.cost_usd FROM call c
		           WHERE c.session_uid = t.session_uid AND c.ts >= t.ts
		           ORDER BY c.ts LIMIT 1
		       )), 0)
		FROM tool_call t` + where + `
		GROUP BY t.tool ORDER BY COUNT(*) DESC`
}

// ToolStat is one tool's totals.
type ToolStat struct {
	Tool    string
	Calls   int
	Seconds float64
	Errors  int
	Cost    float64
}

// Tools returns per-tool totals.
//
// Cost is attributed by joining each tool call to the LLM call it was issued
// around. That is an approximation, and is labelled as such on the page: a tool
// result is not itself billed, so the only honest reading is "tool calls
// associated with calls costing this much".
func (s *Store) Tools(ctx context.Context, f Filter) ([]ToolStat, error) {
	where, args := f.where("t", "ts")
	// Each tool result is attributed to the first LLM call at or after it in the
	// same session: that is the request the tool was issued in the context of.
	// A tool call is not itself billable, so this is an association rather than
	// a price, and the page labels it as such. The seek is an index lookup on
	// (session_uid, ts) rather than a rescan — which is what call_session_ts
	// exists for, and what call_session alone did not provide: the primary key
	// orders a session's calls by call_key, not by ts.
	rows, err := s.db.QueryContext(ctx, toolsSQL(where), args...)
	if err != nil {
		return nil, fmt.Errorf("tools: %w", err)
	}
	defer rows.Close()
	var out []ToolStat
	for rows.Next() {
		var t ToolStat
		if err := rows.Scan(&t.Tool, &t.Calls, &t.Seconds, &t.Errors, &t.Cost); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SessionRow is one session, as listed by the sessions table.
type SessionRow struct {
	UID              string
	Agent            string
	Project          string
	Path             string
	Title            string
	FirstTS          int64
	LastTS           int64
	WallSeconds      float64
	Calls            int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	TotalTokens      int64
	Cost             float64
	LLMSeconds       float64
	ToolSeconds      float64
	ToolCalls        int
	ToolErrors       int
	// Orphaned is true when the session's log is no longer on disk. The figures
	// are still accurate; only the transcript is gone.
	Orphaned bool
}

// Sessions returns sessions, newest first, with a cap.
func (s *Store) Sessions(ctx context.Context, f Filter, limit int) ([]SessionRow, error) {
	where, args := f.whereSession("s")
	if limit <= 0 {
		limit = 5000
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.uid, s.agent, s.project, s.path, s.title, s.first_ts, s.last_ts,
		       s.wall_seconds, s.calls, s.input_tokens, s.output_tokens,
		       s.cache_read_tokens, s.cache_write_tokens, s.reasoning_tokens,
		       s.total_tokens, s.cost_usd, s.llm_seconds, s.tool_seconds,
		       s.tool_calls, s.tool_errors, s.orphaned
		FROM session s`+where+`
		ORDER BY s.last_ts DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("sessions: %w", err)
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var orphaned int
		if err := rows.Scan(&r.UID, &r.Agent, &r.Project, &r.Path, &r.Title,
			&r.FirstTS, &r.LastTS, &r.WallSeconds, &r.Calls, &r.InputTokens,
			&r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.ReasoningTokens, &r.TotalTokens, &r.Cost, &r.LLMSeconds,
			&r.ToolSeconds, &r.ToolCalls, &r.ToolErrors, &orphaned); err != nil {
			return nil, err
		}
		r.Orphaned = orphaned != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// Session looks up one session by id.
func (s *Store) Session(ctx context.Context, uid string) (SessionRow, bool, error) {
	rows, err := s.Sessions(ctx, Filter{}, 0)
	if err != nil {
		return SessionRow{}, false, err
	}
	for _, r := range rows {
		if r.UID == uid {
			return r, true, nil
		}
	}
	return SessionRow{}, false, nil
}

// Facets are every filterable value the database holds.
//
// Collected from the call table rather than from the current filter's results,
// so the dropdowns keep offering values a filter hides — otherwise applying a
// filter makes that axis impossible to widen again.
func (s *Store) Facets(ctx context.Context) (models, agents, projects []string, minTS, maxTS int64, err error) {
	collect := func(col string) ([]string, error) {
		rows, qErr := s.db.QueryContext(ctx,
			"SELECT DISTINCT "+col+" FROM call WHERE "+col+" != '' ORDER BY "+col)
		if qErr != nil {
			return nil, qErr
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	if models, err = collect("model"); err != nil {
		return nil, nil, nil, 0, 0, err
	}
	if agents, err = collect("agent"); err != nil {
		return nil, nil, nil, 0, 0, err
	}
	if projects, err = collect("project"); err != nil {
		return nil, nil, nil, 0, 0, err
	}
	var nullMin, nullMax sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT MIN(NULLIF(ts,0)), MAX(ts) FROM call").Scan(&nullMin, &nullMax); err != nil {
		return nil, nil, nil, 0, 0, err
	}
	return models, agents, projects, nullMin.Int64, nullMax.Int64, nil
}

// Reprice recomputes cost for every stored call from the current price table.
//
// Cost is written at scan time so that serving a page never has to consult the
// price table. The cost of that is that refreshing pricing would otherwise only
// affect calls ingested afterwards — leaving history that exists only in logs
// priced at rates from months ago, and priced at no rate at all for a model
// that was unknown then. Recomputing from the stored token columns makes the
// correction retroactive, which is the point of keeping them.
func (s *Store) Reprice(ctx context.Context, price func(model string, in, out, cacheRead, cacheWrite int64) (float64, bool)) (int, error) {
	type row struct {
		session, key, model             string
		in, out_, cacheRead, cacheWrite int64
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT session_uid, call_key, model, input_tokens, output_tokens,
		        cache_read_tokens, cache_write_tokens FROM call`)
	if err != nil {
		return 0, err
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.session, &r.key, &r.model, &r.in, &r.out_,
			&r.cacheRead, &r.cacheWrite); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	changed := 0
	err = s.InTx(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare(
			`UPDATE call SET cost_usd = ?, priced = ? WHERE session_uid = ? AND call_key = ?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range all {
			cost, priced := price(r.model, r.in, r.out_, r.cacheRead, r.cacheWrite)
			if _, err := stmt.Exec(cost, boolInt(priced), r.session, r.key); err != nil {
				return err
			}
			changed++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("reprice: %w", err)
	}
	// Session summaries carry their own cost column, so they have to follow.
	if err := s.RecomputeAllSessions(); err != nil {
		return 0, fmt.Errorf("reprice summaries: %w", err)
	}
	return changed, nil
}

// CallRow is one stored call, as listed on the session detail page.
type CallRow struct {
	TS               int64
	Model            string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	TotalTokens      int64
	LLMSeconds       float64
	Cost             float64
	Priced           bool
}

// SessionCalls returns a session's calls in time order.
//
// This is the one place the individual call rows are shown rather than rolled
// up, which is the point of storing them: the transcript's cost breakdown
// outlives the log that produced it.
func (s *Store) SessionCalls(ctx context.Context, uid string) ([]CallRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ts, model, input_tokens, output_tokens, cache_read_tokens,
		       cache_write_tokens, reasoning_tokens, total_tokens, llm_seconds,
		       cost_usd, priced
		FROM call WHERE session_uid = ? ORDER BY ts, call_key`, uid)
	if err != nil {
		return nil, fmt.Errorf("session calls %s: %w", uid, err)
	}
	defer rows.Close()
	var out []CallRow
	for rows.Next() {
		var c CallRow
		var priced int
		if err := rows.Scan(&c.TS, &c.Model, &c.InputTokens, &c.OutputTokens,
			&c.CacheReadTokens, &c.CacheWriteTokens, &c.ReasoningTokens,
			&c.TotalTokens, &c.LLMSeconds, &c.Cost, &priced); err != nil {
			return nil, err
		}
		c.Priced = priced != 0
		out = append(out, c)
	}
	return out, rows.Err()
}

// ProjectModel is one model's contribution to one project.
type ProjectModel struct {
	Project      string
	Model        string
	Cost         float64
	Calls        int
	TotalTokens  int64
	OutputTokens int64
	LLMSeconds   float64
}

// projectModelsSQL assembles the per-project, per-model rollup behind the
// expandable project rows.
func projectModelsSQL(where string) string {
	return `
		SELECT project, model, COALESCE(SUM(cost_usd),0), COUNT(*),
		       COALESCE(SUM(total_tokens),0), COALESCE(SUM(output_tokens),0),
		       COALESCE(SUM(llm_seconds),0)
		FROM call` + where + `
		GROUP BY project, model
		ORDER BY project, SUM(cost_usd) DESC`
}

// ProjectModels returns the model breakdown per project, for the expandable
// project rows.
func (s *Store) ProjectModels(ctx context.Context, f Filter) ([]ProjectModel, error) {
	where, args := f.where("", "ts")
	rows, err := s.db.QueryContext(ctx, projectModelsSQL(where), args...)
	if err != nil {
		return nil, fmt.Errorf("project models: %w", err)
	}
	defer rows.Close()
	var out []ProjectModel
	for rows.Next() {
		var m ProjectModel
		if err := rows.Scan(&m.Project, &m.Model, &m.Cost, &m.Calls,
			&m.TotalTokens, &m.OutputTokens, &m.LLMSeconds); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProjectTool is one tool's contribution to one project.
type ProjectTool struct {
	Project string
	Tool    string
	Calls   int
	Seconds float64
	Errors  int
}

// ProjectTools returns the tool breakdown per project.
func (s *Store) ProjectTools(ctx context.Context, f Filter) ([]ProjectTool, error) {
	where, args := f.where("t", "ts")
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.project, t.tool, COUNT(*), COALESCE(SUM(t.seconds),0),
		       COALESCE(SUM(t.is_error),0)
		FROM tool_call t`+where+`
		GROUP BY t.project, t.tool ORDER BY t.project, COUNT(*) DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("project tools: %w", err)
	}
	defer rows.Close()
	var out []ProjectTool
	for rows.Next() {
		var t ProjectTool
		if err := rows.Scan(&t.Project, &t.Tool, &t.Calls, &t.Seconds, &t.Errors); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
