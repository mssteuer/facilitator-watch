package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func syntheticEvent(f *Facet, basis, change, at string) Event {
	t := date(at)
	return Event{At: t, Facet: f, Basis: basis, Change: change, Evidence: fabricatedEvidence("test", change, t, map[string]string{"change": change}, "Synthetic test evidence."), DataMode: DataMode, Fabricated: true}
}

func capability(t *testing.T, items []Capability, id, basis string) Capability {
	t.Helper()
	for _, c := range items {
		if c.Facet.ID == id && c.Basis == basis {
			return c
		}
	}
	t.Fatalf("capability missing: %s %s", id, basis)
	return Capability{}
}

func TestFailureAndUnknownNeverRemoveSupport(t *testing.T) {
	f := Facet{ID: "exact", Category: "scheme", Value: "exact", Scope: "x402 v2 · eip155:8453"}
	first := syntheticEvent(&f, "declared", "first_seen", "2026-06-01T00:00:00Z")
	confirmed := syntheticEvent(&f, "declared", "confirmed", "2026-07-01T00:00:00Z")
	for _, inconclusive := range []Event{syntheticEvent(nil, "unknown", "probe_failed", "2026-08-01T00:00:00Z"), syntheticEvent(&f, "declared", "unknown", "2026-08-01T00:00:00Z")} {
		c := capability(t, Project([]Event{inconclusive, confirmed, first}), "exact", "declared")
		if c.State != "stale" || c.RemovedAt != nil || c.FirstSeen == nil || !c.FirstSeen.Equal(first.At) || c.LastConfirmed == nil || !c.LastConfirmed.Equal(confirmed.At) {
			t.Fatalf("failed check changed known support: %+v", c)
		}
		if c.Evidence.Source != confirmed.Evidence.Source {
			t.Fatal("positive evidence source was lost")
		}
	}
	unknown := capability(t, Project([]Event{syntheticEvent(&f, "observed", "unknown", "2026-08-01T00:00:00Z")}), "exact", "observed")
	if unknown.State != "unknown" || unknown.FirstSeen != nil || unknown.LastConfirmed != nil || unknown.RemovedAt != nil {
		t.Fatalf("unknown invented support: %+v", unknown)
	}
}

func TestIndependentChannelsAndExplicitRemoval(t *testing.T) {
	f := Facet{ID: "exact", Category: "scheme", Value: "exact"}
	events := []Event{
		syntheticEvent(&f, "declared", "first_seen", "2026-06-01T00:00:00Z"),
		syntheticEvent(&f, "observed", "first_seen", "2026-07-01T00:00:00Z"),
		syntheticEvent(&f, "declared", "removed", "2026-08-01T00:00:00Z"),
	}
	states := Project(events)
	d := capability(t, states, "exact", "declared")
	o := capability(t, states, "exact", "observed")
	if d.State != "removed" || d.RemovedAt == nil || !d.RemovedAt.Equal(events[2].At) || o.State != "present" || o.RemovedAt != nil || !o.FirstSeen.Equal(events[1].At) {
		t.Fatalf("channels conflated: %+v", states)
	}
	events = append(events, syntheticEvent(&f, "declared", "first_seen", "2026-09-01T00:00:00Z"))
	d = capability(t, Project(events), "exact", "declared")
	if d.State != "present" || d.RemovedAt != nil || !d.FirstSeen.Equal(events[0].At) {
		t.Fatal("reappearance must preserve earliest retained first_seen")
	}
	chart := BuildChart(events, date("2026-10-01T00:00:00Z"))
	var colored, removals int
	for _, lane := range chart.Lanes {
		if lane.Basis != "declared" {
			continue
		}
		for _, bar := range lane.Bars {
			if bar.Class == "scheme" {
				colored++
			}
			if bar.Class == "removal" {
				removals++
			}
		}
	}
	if colored != 2 || removals != 1 {
		t.Fatalf("separate reappearance intervals missing: %d, %d", colored, removals)
	}
}

func TestFixtureHistorySemantics(t *testing.T) {
	store := fixtureStore{}
	items, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range items {
		events, err := store.History(context.Background(), f.ID)
		if err != nil {
			t.Fatal(err)
		}
		var previous time.Time
		ids := map[string]bool{}
		for _, event := range events {
			if event.At.Before(previous) || event.At.After(fixtureTime) {
				t.Fatalf("unordered/out-of-snapshot history for %s", f.ID)
			}
			previous = event.At
			if ids[event.ID] || event.ID == "" {
				t.Fatal("nonunique event IDs")
			}
			ids[event.ID] = true
			if !event.Fabricated || event.DataMode != "simulated" || !event.Evidence.Fabricated || !json.Valid(event.Evidence.Raw) || event.Evidence.SourceType != "fabricated_fixture" || !strings.HasPrefix(event.Evidence.Source, "fixture://") || !event.Evidence.CapturedAt.Equal(event.At) {
				t.Fatalf("provenance missing: %+v", event)
			}
			if event.Change == "probe_failed" && (event.Facet != nil || event.Basis != "unknown") {
				t.Fatal("failure must not assert support")
			}
		}
	}
	dirtroad, _ := store.Get(context.Background(), "dirtroad")
	for _, c := range dirtroad.Capabilities {
		if c.Basis == "declared" && c.FirstSeen != nil && (c.State != "stale" || c.RemovedAt != nil) {
			t.Fatalf("timeout inferred removal: %+v", c)
		}
	}
	payai, _ := store.Get(context.Background(), "payai")
	if c := capability(t, payai.Capabilities, "network-legacy-example", "declared"); c.State != "removed" || c.RemovedAt == nil {
		t.Fatal("missing explicit removal example")
	}
	// Every read returns independent fixture values, suitable for concurrent handlers.
	items[0].Supported.Kinds[0].Scheme = "mutated"
	again, _ := store.Get(context.Background(), "payai")
	if again.Supported.Kinds[0].Scheme != "exact" {
		t.Fatal("store leaked mutable fixture state")
	}
}

func TestTimelineUsesElapsedTimeAndUnconfirmedTail(t *testing.T) {
	f := Facet{ID: "network", Category: "network", Value: "eip155:8453"}
	first := syntheticEvent(&f, "declared", "first_seen", "2026-06-15T00:00:00Z")
	last := syntheticEvent(&f, "declared", "confirmed", "2026-07-15T00:00:00Z")
	asOf := date("2026-08-01T00:00:00Z")
	chart := BuildChart([]Event{first, last, syntheticEvent(nil, "unknown", "probe_failed", "2026-08-01T00:00:00Z")}, asOf)
	if len(chart.Lanes) != 1 || chart.Lanes[0].State != "stale" || len(chart.Lanes[0].Bars) != 2 {
		t.Fatalf("bad chart: %+v", chart)
	}
	bar := chart.Lanes[0].Bars[0]
	want := first.At.Sub(chart.From).Seconds() / asOf.Sub(chart.From).Seconds() * 100
	if bar.Left != want || chart.Lanes[0].Bars[1].Class != "uncertain" {
		t.Fatal("timeline is not elapsed-time scaled or lacks unknown tail")
	}
	for _, tick := range chart.Ticks {
		if tick.Position < 0 || tick.Position > 100 {
			t.Fatal("tick outside chart")
		}
	}
	if chart.Ticks[1].Position == 50 {
		t.Fatal("June and July must use elapsed days, not equal month widths")
	}
	if got := BuildChart(nil, asOf); len(got.Lanes) != 0 || !got.From.Before(got.To) {
		t.Fatal("empty history should have no invented support")
	}
}

func TestBuildChartIgnoresFutureEvents(t *testing.T) {
	f := Facet{ID: "network", Category: "network", Value: "eip155:8453"}
	first := syntheticEvent(&f, "declared", "first_seen", "2026-06-15T00:00:00Z")
	last := syntheticEvent(&f, "declared", "confirmed", "2026-07-15T00:00:00Z")
	asOf := date("2026-08-01T00:00:00Z")
	future := []Event{
		syntheticEvent(&f, "declared", "confirmed", "2026-09-15T00:00:00Z"),
		syntheticEvent(&f, "declared", "removed", "2026-10-01T00:00:00Z"),
	}
	for _, tt := range []struct {
		name    string
		history []Event
		state   string
	}{
		{"known support", []Event{first, last}, "present"},
		{"snapshot failure", []Event{first, last, syntheticEvent(nil, "unknown", "probe_failed", "2026-08-01T00:00:00Z")}, "stale"},
		{"future only", nil, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			events := append(append([]Event(nil), future...), tt.history...)
			got := BuildChart(events, asOf)
			want := BuildChart(tt.history, asOf)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("future events changed snapshot range, ticks, lanes or bars:\ngot: %+v\nwant: %+v", got, want)
			}
			if len(tt.history) == 0 {
				if len(got.Lanes) != 0 {
					t.Fatal("future-only history invented support")
				}
				return
			}
			if len(got.Lanes) != 1 || got.Lanes[0].State != tt.state || got.Lanes[0].RemovedAt != nil || got.Lanes[0].LastConfirmed == nil || !got.Lanes[0].LastConfirmed.Equal(last.At) {
				t.Fatalf("lane does not reflect snapshot evidence: %+v", got.Lanes)
			}
		})
	}
}

func TestRawSnapshotsDoNotPredateFirstSeen(t *testing.T) {
	store := fixtureStore{}
	f, err := store.Get(context.Background(), "payai")
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.History(context.Background(), "payai")
	if err != nil {
		t.Fatal(err)
	}
	exact := capability(t, f.Capabilities, "scheme-exact", "declared")
	upto := capability(t, f.Capabilities, "scheme-upto", "declared")
	bazaar := capability(t, f.Capabilities, "extension-bazaar", "declared")
	for _, e := range events {
		var raw Supported
		if err := json.Unmarshal(e.Evidence.Raw, &raw); err != nil {
			t.Fatal(err)
		}
		for _, k := range raw.Kinds {
			first := exact.FirstSeen
			if k.Scheme == "upto" {
				first = upto.FirstSeen
			}
			if first == nil || e.At.Before(*first) {
				t.Fatalf("raw scheme %s predates projected first_seen", k.Scheme)
			}
		}
		for _, extension := range raw.Extensions {
			if extension == "bazaar" && (bazaar.FirstSeen == nil || e.At.Before(*bazaar.FirstSeen)) {
				t.Fatal("raw extension predates projected first_seen")
			}
		}
	}
}
