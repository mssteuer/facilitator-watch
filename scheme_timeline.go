package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// BuildSchemeChart is display-only. Resolve tuple IDs from actual declarations,
// never by splitting human-readable facet labels or crossing independent lists.
// Points mean at least one tuple was advertised, not all networks or versions.
func BuildSchemeChart(events []Event, supported Supported, asOf time.Time) Chart {
	ordered := []Event{}
	for _, e := range events {
		if !e.At.After(asOf) {
			ordered = append(ordered, e)
		}
	}
	sortEvents(ordered)
	// A recent observation chart needs hour-level context, not a month-wide axis.
	// Keep at least an hour of elapsed time so a single scan stays a point.
	from := asOf.Add(-time.Hour)
	for _, e := range ordered {
		if e.At.Before(from) {
			from = e.At
		}
	}
	from = from.Truncate(time.Hour)
	chart := Chart{From: from, To: asOf, Ticks: []Tick{}, Lanes: []Lane{}}
	for i, edge := range []string{"first", "", "last"} {
		at := from.Add(asOf.Sub(from) * time.Duration(i) / 2)
		chart.Ticks = append(chart.Ticks, Tick{at.UTC().Format("02 Jan 15:04"), float64(i) * 50, edge})
	}
	position := func(at time.Time) float64 {
		return at.Sub(chart.From).Seconds() / chart.To.Sub(chart.From).Seconds() * 100
	}
	schemes := map[string]string{}
	remember := func(s Supported) {
		for _, k := range s.Kinds {
			for _, f := range supportedFacets(Supported{Kinds: []SupportedKind{k}}) {
				schemes[f.ID] = k.Scheme
			}
		}
	}
	remember(supported)
	for _, e := range ordered {
		if e.Change == "snapshot" {
			var s Supported
			if json.Unmarshal(e.Evidence.Raw, &s) == nil {
				remember(s)
			}
		}
	}
	grouped := map[string][]Event{}
	for _, e := range ordered {
		if e.Facet != nil && e.Facet.Category == "kind" && e.Basis == "declared" {
			if scheme, ok := schemes[e.Facet.ID]; ok {
				grouped[scheme] = append(grouped[scheme], e)
			}
		}
	}
	// Snapshot points also survive a page ending before its per-tuple events.
	for _, e := range ordered {
		if e.Change != "snapshot" {
			continue
		}
		var s Supported
		if json.Unmarshal(e.Evidence.Raw, &s) != nil {
			continue
		}
		for _, f := range supportedFacets(s) {
			if scheme, ok := schemes[f.ID]; ok {
				f := f
				point := e
				point.Facet, point.Basis, point.Change = &f, "declared", "confirmed"
				grouped[scheme] = append(grouped[scheme], point)
			}
		}
	}
	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		laneEvents := grouped[name]
		for _, e := range ordered {
			if e.Change == "probe_failed" {
				laneEvents = append(laneEvents, e)
			}
		}
		// Current labels describe only mapped tuples in this bounded window.
		states := map[string]bool{}
		for _, c := range ProjectBySequence(laneEvents) {
			states[c.State] = true
		}
		state := "unknown"
		for s := range states {
			state = s
		}
		if len(states) > 1 {
			state = "mixed tuple states"
		}
		lane := Lane{Capability: Capability{Facet: Facet{Category: "scheme", Value: name}, Basis: "declared", State: state}}
		sortEvents(laneEvents)
		seen := map[string]bool{}
		for _, e := range laneEvents {
			class, label := "uncertain", "Tuple evidence uncertain or omitted; not proof of unsupported behavior"
			switch e.Change {
			case "first_seen", "confirmed":
				class, label = "kind", "At least one tuple advertised this scheme; not all networks or versions"
			case "probe_failed":
				label = "Declaration check failed; no new declaration confirmation"
			case "unknown", "not_advertised":
			default:
				continue
			}
			key := e.At.Format(time.RFC3339Nano) + class
			if seen[key] {
				continue
			}
			seen[key] = true
			lane.Bars = append(lane.Bars, Bar{position(e.At), 0, class, fmt.Sprintf("%s · %s", e.At.UTC().Format(time.RFC3339), label)})
		}
		chart.Lanes = append(chart.Lanes, lane)
	}
	return chart
}
