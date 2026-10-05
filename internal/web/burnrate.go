package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/tonym128/agent-cost-dashboard/internal/store"
)

// The burn-rate headline.
//
// Lifetime total cost is a number that only goes up and that nobody acts on: it
// cannot answer "is this week worse than last week". These four windows can, and
// each is shown against the equivalent prior window so the direction is visible
// without doing arithmetic.
//
// The data comes from store.Daily, which already buckets cost by local calendar
// day, so no new query is needed. A day with no calls contributes nothing, which
// is what "no spend that day" means — the windows are sums over buckets that
// exist, not over a dense series.

// burnWindow is one headline period and the period it is compared with.
type burnWindow struct {
	Label string
	// Cost is the spend in the window.
	Cost float64
	// PriorCost is the spend in the equivalent preceding window.
	PriorCost float64
	// PriorLabel names that window, so the delta is readable without the note.
	PriorLabel string
	// Days is the length of the window, used for the per-day figure.
	Days int
}

// delta is the change against the prior window, as a signed percentage.
//
// A prior window with no spend has no percentage to compare against, so it
// reports "no prior spend" rather than an invented +100%.
func (w burnWindow) delta() (pct float64, ok bool) {
	if w.PriorCost <= 0 {
		return 0, false
	}
	return (w.Cost - w.PriorCost) / w.PriorCost * 100, true
}

// dayCost is a calendar day of cost, keyed by the store's YYYY-MM-DD day string.
type dayCost struct {
	cost float64
}

// burnWindows rolls the daily buckets up into the four headline periods.
//
// now is the page's generated time rather than time.Now, so a test's fixed clock
// produces the same answer every run. Windows are anchored on the local
// calendar day, matching the `day` column the buckets are grouped by.
func burnWindows(daily []store.DayBucket, now time.Time) []burnWindow {
	byDay := make(map[string]dayCost, len(daily))
	for _, d := range daily {
		byDay[d.Day] = dayCost{cost: d.Cost}
	}

	today := localDay(now)
	sum := func(from, to time.Time) float64 {
		var cost float64
		for day, v := range byDay {
			t, err := time.ParseInLocation("2006-01-02", day, time.Local)
			if err != nil || t.Before(from) || !t.Before(to) {
				continue
			}
			cost += v.cost
		}
		return cost
	}
	// window builds a period of n days ending today, compared with the n days
	// before it.
	window := func(label string, n int) burnWindow {
		to := today.AddDate(0, 0, 1)
		from := to.AddDate(0, 0, -n)
		return burnWindow{
			Label:      label,
			Cost:       sum(from, to),
			PriorCost:  sum(from.AddDate(0, 0, -n), from),
			PriorLabel: priorSpanLabel(n),
			Days:       n,
		}
	}

	// Month to date cannot be a fixed number of days: it is whatever has elapsed
	// of this month, compared with the same day-span of the previous month so a
	// shorter month is not compared against a longer one.
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.Local)
	prevStart := monthStart.AddDate(0, -1, 0)
	mtdEnd := today.AddDate(0, 0, 1)
	mtd := burnWindow{
		Label:      "Month to date",
		Cost:       sum(monthStart, mtdEnd),
		PriorCost:  sum(prevStart, prevStart.AddDate(0, 0, today.Day())),
		PriorLabel: fmt.Sprintf("the first %d days of last month", today.Day()),
		Days:       today.Day(),
	}

	return []burnWindow{
		window("Today", 1),
		window("Last 7 days", 7),
		window("Last 30 days", 30),
		mtd,
	}
}

func priorSpanLabel(n int) string {
	switch n {
	case 1:
		return "Yesterday"
	case 7:
		return "the 7 days before"
	default:
		return fmt.Sprintf("the %d days before", n)
	}
}

func localDay(t time.Time) time.Time {
	y, m, d := t.Local().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.Local)
}

// burnCard is the template-facing view of one burn window.
type burnCard struct {
	Label string
	// Value is the window's cost, pre-formatted.
	Value string
	// Detail is the second line: the spend per day and the prior-window cost.
	Detail string
	// Delta is a signed percentage, or a plain statement when there is no
	// prior window to compare against.
	Delta string
	// DeltaClass tints it: rising spend is amber, falling is green, and an
	// unknown delta is left in the neutral text colour.
	DeltaClass string
}

func burnCards(ws []burnWindow) []burnCard {
	out := make([]burnCard, 0, len(ws))
	for _, w := range ws {
		perDay := 0.0
		if w.Days > 0 {
			perDay = w.Cost / float64(w.Days)
		}
		card := burnCard{
			Label:  w.Label,
			Value:  fmt.Sprintf("$%.2f", w.Cost),
			Detail: fmt.Sprintf("$%.2f/day · %s: $%.2f", perDay, w.PriorLabel, w.PriorCost),
		}
		if pct, ok := w.delta(); ok {
			card.Delta = fmt.Sprintf("%+.0f%%", pct)
			switch {
			case pct > 0:
				card.DeltaClass = "burn-up"
			case pct < 0:
				card.DeltaClass = "burn-down"
			default:
				card.DeltaClass = "burn-flat"
			}
		} else {
			card.Delta = "no prior spend"
			card.DeltaClass = "burn-flat"
		}
		out = append(out, card)
	}
	return out
}

// burnNote spells out what each delta is measured against, so the headline does
// not have to carry four priors in its own labels.
func burnNote(ws []burnWindow) string {
	if len(ws) == 0 {
		return ""
	}
	labels := make([]string, 0, len(ws))
	for _, w := range ws {
		labels = append(labels, fmt.Sprintf("%s vs %s", w.Label, w.PriorLabel))
	}
	return "Each period is shown against the one before it (" +
		strings.Join(labels, "; ") + "). Deltas are cost."
}
