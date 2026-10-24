package main

import (
	"fmt"
	"sort"
	"strconv"
	"time"
)

func fmtInt(n int) string { return strconv.Itoa(n) }

type Tick struct {
	Label    string
	Position float64
	Edge     string
}
type Bar struct {
	Left  float64
	Width float64
	Class string
	Label string
}
type Lane struct {
	Capability
	Bars []Bar
}
type Chart struct {
	Ticks []Tick
	Lanes []Lane
	From  time.Time
	To    time.Time
}

// BuildChart shades first-seen to last-confirmed as an observation window, not
// a guarantee of continuous support. Unconfirmed time is hatched. Explicit
// removals have their own marker; reappearance starts a separate window.
func BuildChart(events []Event, asOf time.Time) Chart {
	var ordered []Event
	for _, e := range events {
		if !e.At.After(asOf) {
			ordered = append(ordered, e)
		}
	}
	sortEvents(ordered)
	from := asOf
	for _, e := range ordered {
		if e.At.Before(from) {
			from = e.At
		}
	}
	from = time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC)
	if !from.Before(asOf) {
		from = asOf.Add(-24 * time.Hour)
	}
	chart := Chart{From: from, To: asOf, Ticks: []Tick{}, Lanes: []Lane{}}
	pct := func(t time.Time) float64 {
		p := t.Sub(from).Seconds() / asOf.Sub(from).Seconds() * 100
		if p < 0 {
			return 0
		}
		if p > 100 {
			return 100
		}
		return p
	}
	for t := from; t.Before(asOf); t = t.AddDate(0, 1, 0) {
		edge := ""
		if t.Equal(from) {
			edge = "first"
		}
		chart.Ticks = append(chart.Ticks, Tick{t.Format("02 Jan 2006"), pct(t), edge})
	}
	chart.Ticks = append(chart.Ticks, Tick{asOf.Format("02 Jan 2006"), 100, "last"})
	for _, c := range Project(ordered) {
		lane := Lane{Capability: c, Bars: []Bar{}}
		var start, last, gap *time.Time
		flush := func(end time.Time, removed bool) {
			if start != nil && last != nil {
				lane.Bars = append(lane.Bars, Bar{pct(*start), pct(*last) - pct(*start), c.Facet.Category, fmt.Sprintf("%s observation window: %s to %s; does not establish continuous support", c.Basis, start.Format("2006-01-02"), last.Format("2006-01-02"))})
				if end.After(*last) {
					lane.Bars = append(lane.Bars, Bar{pct(*last), pct(end) - pct(*last), "uncertain", "Unconfirmed interval; no support or removal inference"})
				}
			}
			if removed {
				lane.Bars = append(lane.Bars, Bar{pct(end), 0, "removal", "Explicit withdrawal of this evidence channel on " + end.Format("2006-01-02")})
			}
			start, last = nil, nil
		}
		for _, e := range ordered {
			if e.Change == "probe_failed" && start != nil {
				flush(e.At, false)
				at := e.At
				gap = &at
				continue
			}
			if e.Facet == nil || e.Facet.ID != c.Facet.ID || e.Basis != c.Basis {
				continue
			}
			switch e.Change {
			case "first_seen", "confirmed":
				if gap != nil {
					lane.Bars = append(lane.Bars, Bar{pct(*gap), pct(e.At) - pct(*gap), "uncertain", "Unconfirmed interval; no support inference"})
					gap = nil
				}
				at := e.At
				if start == nil {
					first := at
					start = &first
				}
				last = &at
			case "removed":
				flush(e.At, true)
				gap = nil
			case "unknown", "not_advertised":
				flush(e.At, false)
				if gap == nil {
					at := e.At
					gap = &at
				}
			}
		}
		flush(asOf, false)
		if gap != nil && asOf.After(*gap) {
			lane.Bars = append(lane.Bars, Bar{pct(*gap), pct(asOf) - pct(*gap), "uncertain", "Unconfirmed interval; no support inference"})
		}
		if len(lane.Bars) == 0 {
			lane.Bars = append(lane.Bars, Bar{0, 100, "uncertain", "Unknown — no evidence window; no first-seen date"})
		}
		chart.Lanes = append(chart.Lanes, lane)
	}
	rank := map[string]int{"kind": 0, "protocol": 0, "scheme": 1, "network": 2, "extension": 3}
	sort.SliceStable(chart.Lanes, func(i, j int) bool { return rank[chart.Lanes[i].Facet.Category] < rank[chart.Lanes[j].Facet.Category] })
	return chart
}
