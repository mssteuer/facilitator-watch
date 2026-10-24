package main

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSchemeChartGroupingAndSingleObservation(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := Supported{Kinds: []SupportedKind{
		{X402Version: 1, Scheme: "exact", Network: "base"},
		{X402Version: 2, Scheme: "exact", Network: "eip155:1"},
		{X402Version: 2, Scheme: "upto", Network: "eip155:8453"},
		{X402Version: 2, Scheme: "batch-settlement", Network: "eip155:1"},
		{X402Version: 3, Scheme: "future · scheme", Network: "future:chain"},
	}}
	raw, _ := json.Marshal(s)
	events := []Event{{ID: "1", At: at, Change: "snapshot", Evidence: Evidence{Raw: raw}}}
	for i, f := range supportedFacets(s) {
		f := f
		events = append(events, Event{ID: strconv.Itoa(i + 2), At: at, Facet: &f, Basis: "declared", Change: "first_seen"})
	}
	before, _ := json.Marshal(events)
	chart := BuildSchemeChart(events, s, at.Add(time.Hour))
	names := []string{}
	for _, lane := range chart.Lanes {
		names = append(names, lane.Facet.Value)
		if len(lane.Bars) != 1 || lane.Bars[0].Width != 0 || lane.Bars[0].Class != "kind" || !strings.Contains(lane.Bars[0].Label, "At least one tuple") {
			t.Fatalf("single observation fabricated a span or duplicated tuples: %+v", lane)
		}
	}
	if !reflect.DeepEqual(names, []string{"batch-settlement", "exact", "future · scheme", "upto"}) {
		t.Fatalf("schemes not preserved/grouped: %v", names)
	}
	after, _ := json.Marshal(events)
	if string(before) != string(after) {
		t.Fatal("display grouping mutated exact events")
	}
	if got := BuildSchemeChart(nil, s, at); len(got.Lanes) != 0 {
		t.Fatal("latest snapshot invented history outside the bounded events")
	}
	if got := BuildSchemeChart(events, s, at.Add(-time.Second)); len(got.Lanes) != 0 {
		t.Fatal("future observations leaked")
	}
	// A bounded page can contain a snapshot but none of its tuple events.
	if got := BuildSchemeChart(events[:1], Supported{}, at.Add(time.Hour)); len(got.Lanes) != 4 {
		t.Fatal("raw snapshot observations lost at the bounded page edge")
	}
	for _, emptyEvents := range [][]Event{nil, {{ID: "1", At: at, Change: "probe_failed"}}, {{ID: "1", At: at, Change: "snapshot", Evidence: Evidence{Raw: json.RawMessage(`{"kinds":[]}`)}}}} {
		if got := BuildSchemeChart(emptyEvents, Supported{}, at.Add(time.Hour)); len(got.Lanes) != 0 {
			t.Fatal("empty or failed-only history invented scheme evidence")
		}
	}
}

func TestSchemeChartRecentElapsedAxis(t *testing.T) {
	asOf := time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC)
	for _, events := range [][]Event{
		nil,
		{{At: asOf, Change: "snapshot"}},
		{{At: asOf.Add(-3 * 24 * time.Hour)}, {At: asOf.Add(time.Hour)}},
	} {
		chart := BuildSchemeChart(events, Supported{}, asOf)
		if !chart.To.Equal(asOf) || chart.To.Sub(chart.From) < time.Hour || len(chart.Ticks) != 3 {
			t.Fatalf("invalid recent axis: %+v", chart)
		}
		for i, tick := range chart.Ticks {
			at := chart.From.Add(chart.To.Sub(chart.From) * time.Duration(i) / 2)
			if tick.Position != float64(i)*50 || tick.Label != at.UTC().Format("02 Jan 15:04") {
				t.Fatalf("tick must show elapsed UTC date and time: %+v", tick)
			}
		}
		if chart.Ticks[0].Edge != "first" || chart.Ticks[2].Edge != "last" {
			t.Fatal("edge labels must stay inside the chart")
		}
	}
	s := Supported{Kinds: []SupportedKind{{X402Version: 1, Scheme: "exact", Network: "base"}}}
	raw, _ := json.Marshal(s)
	at := asOf.Add(-17 * time.Minute)
	chart := BuildSchemeChart([]Event{{At: at, Change: "snapshot", Evidence: Evidence{Raw: raw}}}, s, asOf)
	point := chart.Lanes[0].Bars[0]
	want := at.Sub(chart.From).Seconds() / chart.To.Sub(chart.From).Seconds() * 100
	if point.Left != want || point.Width != 0 {
		t.Fatalf("point must keep exact elapsed position, not a support span: %+v", point)
	}
}

func TestSchemeChartUncertaintyAndOmission(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	s := Supported{Kinds: []SupportedKind{{X402Version: 2, Scheme: "exact", Network: "eip155:1"}, {X402Version: 2, Scheme: "exact", Network: "eip155:8453"}}}
	facets := supportedFacets(s)
	positive := func(id string, i int, minute int) Event {
		return Event{ID: id, At: at.Add(time.Duration(minute) * time.Minute), Facet: &facets[i], Basis: "declared", Change: "confirmed"}
	}
	events := []Event{positive("1", 0, 0), positive("2", 1, 0)}
	assertState := func(want string) Lane {
		t.Helper()
		chart := BuildSchemeChart(events, s, at.Add(time.Hour))
		if len(chart.Lanes) != 1 || chart.Lanes[0].State != want {
			t.Fatalf("want %s: %+v", want, chart.Lanes)
		}
		for _, bar := range chart.Lanes[0].Bars {
			if bar.Width != 0 {
				t.Fatal("grouped evidence must not imply continuous support")
			}
		}
		return chart.Lanes[0]
	}
	assertState("present")
	events = append(events, Event{ID: "3", At: at.Add(time.Minute), Change: "probe_failed"})
	lane := assertState("stale")
	if len(lane.Bars) != 2 || lane.Bars[1].Class != "uncertain" {
		t.Fatal("failure must remain visible")
	}
	events = append(events, positive("4", 0, 2))
	assertState("mixed tuple states")
	unknown := positive("5", 1, 2)
	unknown.Change = "unknown"
	events = append(events, unknown)
	lane = assertState("mixed tuple states")
	if len(lane.Bars) != 4 || lane.Bars[2].Left != lane.Bars[3].Left || lane.Bars[2].Class == lane.Bars[3].Class {
		t.Fatalf("mixed same-time positive and uncertainty markers lost: %+v", lane.Bars)
	}
	omission := positive("6", 1, 3)
	omission.Change = "not_advertised"
	events = append(events, omission)
	assertState("mixed tuple states")
	omission = positive("7", 0, 3)
	omission.Change = "not_advertised"
	events = append(events, omission)
	assertState("not_advertised")
	events = append(events, Event{ID: "8", At: at.Add(4 * time.Minute), Change: "probe_failed"})
	assertState("not_advertised")
	onlyUnknown := positive("9", 0, 5)
	onlyUnknown.Change = "unknown"
	chart := BuildSchemeChart([]Event{onlyUnknown}, s, at.Add(time.Hour))
	if chart.Lanes[0].State != "unknown" || chart.Lanes[0].Bars[0].Class != "uncertain" {
		t.Fatal("unknown-only window fabricated positive evidence")
	}
	if got := BuildSchemeChart(events, Supported{}, at.Add(time.Hour)); len(got.Lanes) != 0 {
		t.Fatal("unresolvable tuple labels must not be guessed")
	}
}
