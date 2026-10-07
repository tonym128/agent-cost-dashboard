// Package web serves the dashboard.
//
// The handler is read-only with respect to the store and holds no scanner state,
// so a scan in progress is invisible to a page load: WAL gives each request a
// consistent snapshot while the writer continues. That is what keeps the
// website responsive while the background half of the process is busy, without
// either half needing to know about the other.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

//go:embed assets/dashboard.css assets/dashboard.js
var assets embed.FS

//go:embed templates/*.html
var templateFS embed.FS

// Server serves the dashboard from a store.
type Server struct {
	store *store.Store
	log   *slog.Logger
	tmpl  *template.Template
	// Generated is injectable so a page can show a stable time in tests.
	Generated func() time.Time
	// ScanInfo reports scanner state for the status line. Optional.
	ScanInfo func() (lastRun time.Time, running bool)
	// HealthCacheTTL is how long /healthz reuses its aggregate. Defaults to
	// healthCacheTTL; zero queries the store on every request, which is what a
	// test wants and what a network-exposed instance does not.
	HealthCacheTTL time.Duration

	health healthCache
}

// New builds a Server.
func New(st *store.Store, log *slog.Logger) (*Server, error) {
	tmpl, err := template.New("").Funcs(Funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Server{store: st, log: log, tmpl: tmpl, Generated: time.Now,
		HealthCacheTTL: healthCacheTTL}, nil
}

// Handler returns the HTTP handler for the dashboard.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/assets/", s.handleAsset)
	mux.HandleFunc("/api/activity", s.handleActivity)
	mux.Handle("/session", http.HandlerFunc(s.handleSession))
	// Outermost, so the headers are on every response the dashboard produces —
	// including the assets, which is where a policy that only covered documents
	// would be one bypass away from useless.
	return withSecurityHeaders(logRequests(s.log, mux))
}

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/" {
			log.Debug("request", "method", r.Method, "path", r.URL.Path,
				"duration", time.Since(start).Round(time.Millisecond))
		}
	})
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	switch name {
	case "dashboard.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case "dashboard.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
		return
	}
	data, err := assets.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// The browser is not the bottleneck here and a stale stylesheet is a
	// confusing bug report, so revalidation beats caching.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

// handleHealth answers the unauthenticated liveness probe.
//
// Everything here is either a count or a timestamp. That is not incidental: the
// endpoint is open on purpose so a container healthcheck does not need the auth
// token in its command, and the reason that is defensible is that there is
// nothing here to disclose. No paths, no models, no titles.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	status := map[string]any{"ok": true}
	// The aggregate is cached, because this endpoint is unauthenticated and
	// would otherwise let anyone who can reach the port run a full scan of the
	// call table as often as they like. See healthCacheTTL.
	if counts, ok := s.healthCountsFor(ctx); ok {
		status["calls"] = counts.calls
		status["sessions"] = counts.sessions
	} else {
		status["ok"] = false
	}
	if s.ScanInfo != nil {
		lastRun, running := s.ScanInfo()
		if lastRun.IsZero() {
			// No pass has completed yet. The zero time would otherwise render
			// as 0001-01-01T00:00:00Z, which parses as a valid timestamp two
			// thousand years in the past.
			status["last_scan"] = nil
		} else {
			status["last_scan"] = lastRun.Format(time.RFC3339)
		}
		status["scan_running"] = running
	}
	writeJSON(w, status)
}

// ---------------------------------------------------------------- index

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	ctx := r.Context()
	filter, _, err := parseFilter(r.URL.Query())
	if err != nil {
		s.errorPage(w, http.StatusBadRequest, "Bad filter", err)
		return
	}

	data, err := s.buildPayload(ctx, filter, r.URL.Query())
	if err != nil {
		s.log.Error("could not build dashboard", "error", err)
		s.errorPage(w, http.StatusInternalServerError, "Could not read the database",
			fmt.Errorf("the scan may still be populating it; try again in a moment"))
		return
	}
	// The payload goes into two inline <script> blocks, so it needs the nonce
	// from withSecurityHeaders: the CSP permits those blocks and nothing else.
	data.Nonce = nonceFrom(ctx)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		s.log.Error("template failed", "error", err)
	}
}

func (s *Server) errorPage(w http.ResponseWriter, code int, title string, err error) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	s.tmpl.ExecuteTemplate(w, "error.html", map[string]any{
		"Title": title, "Error": err.Error(), "Code": code,
	})
}

// pageData is what the template renders from.
type pageData struct {
	Generated string
	StatCards []statCard
	// Nonce is the per-response CSP nonce, carried to the two inline <script>
	// blocks that hold the payload and the filter state.
	Nonce       string
	PayloadJSON template.JS
	FilterState filterState
	Facets      facets
	Scan        []scanStatusView
	ScanSummary string
	// Burn is the cost headline: Today, 7d, 30d and month-to-date, each with
	// the change against the equivalent prior period.
	Burn     []burnCard
	BurnNote string
	// Sources is the source-detection panel: one row per agent this build can
	// read, and whether its logs were found. SourcesNeedAction decides whether
	// it is shown at all.
	Sources           []sourceView
	SourcesSummary    string
	SourcesNeedAction bool
}

type facets struct {
	Models   []string
	Agents   []string
	Projects []string
	MinDate  string
	MaxDate  string
}

type scanStatusView struct {
	Agent        string
	LastScan     string
	LastScanAgo  string
	FilesSeen    int
	FilesChanged int
	CallsAdded   int
	Error        string
}

func (s *Server) buildPayload(ctx context.Context, filter store.Filter, query url.Values) (pageData, error) {
	var totals store.Totals
	var daily []store.DayBucket
	var models []store.ModelStat
	var projects []store.ProjectStat
	var tools []store.ToolStat
	var sessions []store.SessionRow
	var projectModels []store.ProjectModel
	var projectTools []store.ProjectTool
	var modelNames, agentNames, projectNames []string
	var minTS, maxTS int64

	// The page renders eleven rollups; each must observe the same database
	// state, so they all run inside one read transaction. Under WAL this does
	// not block the writer, and a scan in progress cannot tear the payload.
	if err := s.store.ReadTx(ctx, func(st *store.Store) error {
		var err error
		if totals, err = st.Totals(ctx, filter); err != nil {
			return err
		}
		if daily, err = st.Daily(ctx, filter); err != nil {
			return err
		}
		if models, err = st.Models(ctx, filter); err != nil {
			return err
		}
		if projects, err = st.Projects(ctx, filter); err != nil {
			return err
		}
		if tools, err = st.Tools(ctx, filter); err != nil {
			return err
		}
		if sessions, err = st.Sessions(ctx, filter, 5000); err != nil {
			return err
		}
		if projectModels, err = st.ProjectModels(ctx, filter); err != nil {
			return err
		}
		if projectTools, err = st.ProjectTools(ctx, filter); err != nil {
			return err
		}
		modelNames, agentNames, projectNames, minTS, maxTS, err = st.Facets(ctx)
		return err
	}); err != nil {
		return pageData{}, err
	}

	// The headline figures sit at the top level rather than under a key: they are
	// what the stat cards read, and there is one set of them per page.
	payload := map[string]any{
		"generatedEpoch": s.Generated().Unix(),
		"dailyStats":     dailyJSON(daily),
		"models":         modelsJSON(models, totals.Cost),
		"projects":       projectsJSON(projects, sessions, projectModels, projectTools),
		"tools":          toolsJSON(tools, totals.ToolSeconds),
		"unpricedCalls":  totals.UnpricedCalls,
		"firstActivity":  totals.FirstTS,
		"lastActivity":   totals.LastTS,
	}
	for k, v := range totalsJSON(totals) {
		payload[k] = v
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return pageData{}, fmt.Errorf("encode payload: %w", err)
	}

	view := facets{Models: modelNames, Agents: agentNames, Projects: projectNames}
	if minTS > 0 {
		view.MinDate = time.Unix(minTS, 0).Local().Format("2006-01-02")
	}
	if maxTS > 0 {
		view.MaxDate = time.Unix(maxTS, 0).Local().Format("2006-01-02")
	}

	views, _ := s.scanViews(ctx)
	sources := sourceViews(views)
	now := s.Generated()
	burn := burnWindows(daily, now)
	return pageData{
		Generated:      now.Format("2006-01-02 15:04:05"),
		StatCards:      statCards(totals),
		PayloadJSON:    template.JS(encoded),
		FilterState:    parseFilterState(query),
		Facets:         view,
		Scan:           views,
		ScanSummary:    scanSummary(views),
		Burn:           burnCards(burn),
		BurnNote:       burnNote(burn),
		Sources:        sources,
		SourcesSummary: sourceSummary(sources),
		// Shown whenever a source needs attention, and also when the database
		// holds no calls at all: on a fresh install that panel is the only
		// thing on the page explaining why every figure is zero.
		SourcesNeedAction: anyNeedsAttention(sources) || totals.Calls == 0,
	}, nil
}

func (s *Server) scanViews(ctx context.Context) ([]scanStatusView, error) {
	statuses, err := s.store.ScanStatuses()
	if err != nil {
		return nil, err
	}
	now := s.Generated()
	out := make([]scanStatusView, 0, len(statuses))
	for _, st := range statuses {
		views := scanStatusView{
			Agent:        st.Agent,
			FilesSeen:    st.FilesSeen,
			FilesChanged: st.FilesChanged,
			CallsAdded:   st.CallsIngested,
			Error:        st.Error,
		}
		if !st.LastScanAt.IsZero() {
			views.LastScan = st.LastScanAt.Format("15:04:05")
			views.LastScanAgo = humanAge(now.Sub(st.LastScanAt))
		}
		out = append(out, views)
	}
	return out, nil
}

func scanSummary(views []scanStatusView) string {
	if len(views) == 0 {
		return "no scan has run yet"
	}
	newest := views[0].LastScanAgo
	var errs []string
	for _, v := range views {
		if v.Error != "" {
			errs = append(errs, v.Agent)
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return "last scan " + newest + "; failing: " + strings.Join(errs, ", ")
	}
	return "last scan " + newest
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	uid := r.URL.Query().Get("uid")
	if uid == "" {
		http.Error(w, "missing uid", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	row, ok, err := s.store.Session(ctx, uid)
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Could not load session", err)
		return
	}
	if !ok {
		s.errorPage(w, http.StatusNotFound, "Unknown session",
			fmt.Errorf("no session with id %q is stored; it may have been pruned", uid))
		return
	}
	calls, err := s.store.SessionCalls(ctx, uid)
	if err != nil {
		s.errorPage(w, http.StatusInternalServerError, "Could not load session", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.tmpl.ExecuteTemplate(w, "session.html", map[string]any{
		"Generated": s.Generated().Format("2006-01-02 15:04:05"),
		"Session":   sessionView(row),
		"Calls":     callViews(calls),
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		slog.Default().Error("json encode failed", "error", err)
	}
}

// ---------------------------------------------------------------- filtering

type filterState struct {
	Models   []string
	Agents   []string
	Projects []string
	DateFrom string
	DateTo   string
	Active   bool
}

func parseFilter(q url.Values) (store.Filter, filterState, error) {
	multi := func(key string) []string {
		var out []string
		for _, v := range q[key] {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
		return out
	}
	state := filterState{
		Models:   multi("model"),
		Agents:   multi("agent"),
		Projects: multi("project"),
		DateFrom: q.Get("date_from"),
		DateTo:   q.Get("date_to"),
	}
	state.Active = len(state.Models) > 0 || len(state.Agents) > 0 ||
		len(state.Projects) > 0 || state.DateFrom != "" || state.DateTo != ""

	filter := store.Filter{
		Models:   state.Models,
		Agents:   state.Agents,
		Projects: state.Projects,
	}

	// A date is interpreted in the reader's own timezone, and a "to" date
	// covers the whole of that day rather than stopping at midnight.
	if state.DateFrom != "" {
		t, err := time.ParseInLocation("2006-01-02", state.DateFrom, time.Local)
		if err != nil {
			return filter, state, fmt.Errorf("date_from %q is not a date", state.DateFrom)
		}
		filter.DateFrom = &t
	}
	if state.DateTo != "" {
		t, err := time.ParseInLocation("2006-01-02", state.DateTo, time.Local)
		if err != nil {
			return filter, state, fmt.Errorf("date_to %q is not a date", state.DateTo)
		}
		end := t.AddDate(0, 0, 1).Add(-time.Second)
		filter.DateTo = &end
	}
	return filter, state, nil
}

// parseFilterState re-reads the query for the template, which needs the raw
// strings the selects are pre-filled from rather than the parsed form.
func parseFilterState(query url.Values) filterState {
	_, state, err := parseFilter(query)
	if err != nil {
		return filterState{}
	}
	return state
}
