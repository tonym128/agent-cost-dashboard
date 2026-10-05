package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The activity chart is an API rather than a slice of the page payload.
//
// That is a direct consequence of the data living in a database rather than in a
// rescan: any window can be computed exactly, so there is no reason to send a
// fixed horizon's worth of fine buckets and re-roll them in the browser. Asking
// for what you want is also what makes "last March" work — those rows are still
// here even though the log that produced them is not.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	filter, _, err := parseFilter(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	oldest, newest, err := s.store.HistoryRange(ctx, filter)
	if err != nil {
		s.log.Error("activity history range failed", "error", err)
		http.Error(w, "could not read history", http.StatusInternalServerError)
		return
	}
	if oldest.IsZero() {
		writeJSON(w, map[string]any{"buckets": []any{}, "oldest": 0, "newest": 0})
		return
	}

	// Anchor on the newest recorded call rather than on wall-clock time. A
	// database whose data ends last Tuesday should chart last Tuesday; anchoring
	// on today would show an empty week and read as "you did nothing".
	to := newest
	if !s.Generated().Before(to.Add(24 * time.Hour)) {
		to = s.Generated()
	}

	rangeSecs := int64(atoiDefault(r.URL.Query().Get("range"), 86400))
	from := oldest
	if rangeSecs > 0 {
		candidate := to.Add(-time.Duration(rangeSecs) * time.Second)
		if candidate.After(oldest) {
			from = candidate
		}
	}

	// An explicit step is validated rather than passed through. A one-second step
	// over a year of history is tens of millions of rows from a local process,
	// and the page only ever offers the values below, so anything else is either
	// a mistake or an attempt to make the server do unbounded work.
	step := int64(atoiDefault(r.URL.Query().Get("step"), 0))
	if step > 0 && !isAllowedStep(step) {
		http.Error(w, fmt.Sprintf(
			"step must be one of %s seconds", allowedStepsLabel()), http.StatusBadRequest)
		return
	}
	if step <= 0 {
		step = autoStep(from, to)
	}

	buckets, err := s.store.Activity(ctx, filter, from, to, step)
	if err != nil {
		s.log.Error("activity query failed", "error", err)
		http.Error(w, "could not read activity", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]any{
		"buckets": activityJSON(buckets),
		"step":    step,
		"from":    from.Unix(),
		"to":      to.Unix(),
		"oldest":  oldest.Unix(),
		"newest":  newest.Unix(),
		// The server, not the browser, decides the bucket boundaries, so what the
		// axis labels say is what the rows actually mean.
		"autoStep": autoStep(from, to),
	})
}

// autoStep picks a resolution that keeps the bar count readable.
//
// Tuned against bar count rather than derived from a target, because the two
// things a reader wants pull in opposite directions: minute-by-minute detail
// while watching a session unfold, and one bar per hour when asking how much
// got done today.
func autoStep(from, to time.Time) int64 {
	span := to.Sub(from)
	switch {
	case span <= 6*time.Hour:
		return 300
	case span <= 12*time.Hour:
		return 900
	case span <= 7*24*time.Hour:
		return 3600
	case span <= 90*24*time.Hour:
		return 6 * 3600
	default:
		return 86400
	}
}

// Steps accepted for an explicit resolution override: 5min, 15min, hourly,
// 6-hourly, daily.
//
// A superset of what the page's Resolution control offers — the control lists
// 5min, 15min, hourly and daily, and not the 6-hourly one. Accepting more than
// is offered is deliberate: ?step= is a public query parameter, so the set that
// matters is the set of resolutions the query path can ask for, not the four
// buttons on the page. What must not happen is the reverse: an offered value
// that this list rejects is a dead control returning HTTP 400, and that is
// asserted.
var allowedSteps = []int64{300, 900, 3600, 6 * 3600, 86400}

func isAllowedStep(step int64) bool {
	for _, s := range allowedSteps {
		if s == step {
			return true
		}
	}
	return false
}

// allowedStepsLabel names the accepted resolutions in the rejection message, so
// a caller who guessed wrong is told what to use instead.
func allowedStepsLabel() string {
	parts := make([]string, 0, len(allowedSteps))
	for _, s := range allowedSteps {
		parts = append(parts, strconv.FormatInt(s, 10))
	}
	return strings.Join(parts, ", ")
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}
