package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

var fixtureTime = date("2026-10-18T12:00:00Z")

// Fabricated data is compiled into tests only, never the production server.
const DataMode = "simulated"

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

type fixtureStore struct{}

func (fixtureStore) List(ctx context.Context) ([]Facilitator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items := make([]Facilitator, 0, 5)
	for i := 0; i < 5; i++ {
		f, _ := fixture(i)
		items = append(items, f)
	}
	return items, nil
}

func (s fixtureStore) Get(ctx context.Context, id string) (Facilitator, error) {
	items, err := s.List(ctx)
	if err != nil {
		return Facilitator{}, err
	}
	for _, f := range items {
		if f.ID == id {
			return f, nil
		}
	}
	return Facilitator{}, ErrNotFound
}

func (s fixtureStore) History(ctx context.Context, id string) ([]Event, error) {
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	for i := 0; i < 5; i++ {
		f, events := fixture(i)
		if f.ID == id {
			return events, nil
		}
	}
	return nil, ErrNotFound
}

func (s fixtureStore) HistoryPage(ctx context.Context, id string, start, end time.Time, limit int, cursor string) (HistoryPage, error) {
	events, err := s.History(ctx, id)
	if err != nil {
		return HistoryPage{}, err
	}
	return HistoryPage{Events: events, Start: start, End: end, Limit: limit}, nil
}

func fabricatedEvidence(id, kind string, at time.Time, raw any, note string) Evidence {
	b, err := json.Marshal(raw)
	if err != nil {
		panic(err)
	}
	return Evidence{Source: "fixture://" + id + "/" + kind, SourceType: "fabricated_fixture", CapturedAt: at, Raw: b, Note: "FABRICATED: " + note, Fabricated: true}
}

func fixture(i int) (Facilitator, []Event) {
	ids := []string{"payai", "mogami", "daydreams", "mcpay", "dirtroad"}
	names := []string{"PayAI", "Mogami", "Daydreams Systems", "MCPay", "dirtroad.dev"}
	statuses := []string{"operational", "operational", "degraded", "operational", "unavailable"}
	latencies := []int{142, 176, 612, 231, 0}
	id := ids[i]
	version, network := 2, "eip155:8453"
	if i >= 3 {
		version, network = 1, "base"
	}
	snapshot := Supported{Kinds: []SupportedKind{{X402Version: version, Scheme: "exact", Network: network}}}
	if version == 2 {
		snapshot.Extensions = []string{"bazaar"}
		signers := map[string][]string{}
		snapshot.Signers = &signers
	}
	if i == 0 {
		snapshot.Kinds = append(snapshot.Kinds, SupportedKind{X402Version: 2, Scheme: "upto", Network: "eip155:8453", Extra: json.RawMessage(`{"fixture":true}`)})
	}
	last := fixtureTime
	if i == 4 {
		last = date("2026-10-11T12:00:00Z")
	}
	start := date(fmt.Sprintf("2026-06-%02dT12:00:00Z", 3+i*5))
	// Facets are projections of evidence, not a Cartesian product of support.
	facets := []Facet{
		{ID: "protocol", Category: "protocol", Value: fmt.Sprintf("x402 v%d", version), Scope: "Declared kinds[].x402Version"},
		{ID: "scheme-exact", Category: "scheme", Value: "exact", Scope: fmt.Sprintf("x402 v%d · %s", version, network)},
		{ID: "network", Category: "network", Value: network, Scope: fmt.Sprintf("x402 v%d · exact", version)},
	}
	if version == 2 {
		facets = append(facets, Facet{ID: "extension-bazaar", Category: "extension", Value: "Bazaar", Scope: "Declaration only; discovery endpoints and response behavior untested"})
	}
	events := []Event{}
	add := func(facet *Facet, basis, change string, at time.Time, raw any, note string) {
		events = append(events, Event{ID: fmt.Sprintf("%s-%02d", id, len(events)+1), At: at, Facet: facet, Basis: basis, Change: change, Evidence: fabricatedEvidence(id, fmt.Sprintf("event-%02d", len(events)+1), at, raw, note), DataMode: DataMode, Fabricated: true})
	}
	for n := range facets {
		f := facets[n]
		at := start
		// A kinds tuple establishes its protocol, scheme and network facets at
		// the same time. Do not retain an earlier raw snapshot containing a
		// feature whose fictional first-seen date is later.
		var initial any = Supported{Kinds: []SupportedKind{snapshot.Kinds[0]}, Signers: snapshot.Signers}
		if f.Category == "extension" {
			at = start.AddDate(0, 3, 0)
			initial = snapshot
		}
		add(&f, "declared", "first_seen", at, initial, "Simulated /supported declaration first seen; not a release date.")
		add(&f, "declared", "confirmed", last, snapshot, "Simulated latest declaration; no behavioral verification.")
	}
	behavior := facets[1]
	if i == 0 {
		add(&behavior, "observed", "first_seen", date("2026-09-08T12:00:00Z"), map[string]any{"scenario": "synthetic exact verification", "result": "pass", "x402Version": 2, "scheme": "exact", "network": network}, "Simulated behavioral scenario only. No transaction or settlement occurred.")
		add(&behavior, "observed", "confirmed", fixtureTime, map[string]string{"scenario": "synthetic exact verification", "result": "pass"}, "Simulated behavioral confirmation; not settlement assurance.")
	} else {
		add(&behavior, "observed", "unknown", fixtureTime, map[string]string{"reason": "not tested"}, "No behavioral evidence in this fabricated example.")
	}
	if i == 0 {
		f := Facet{ID: "scheme-upto", Category: "scheme", Value: "upto", Scope: "x402 v2 · eip155:8453 · illustrative maturity unverified"}
		beforeExtension := Supported{Kinds: snapshot.Kinds, Signers: snapshot.Signers}
		add(&f, "declared", "first_seen", date("2026-08-14T12:00:00Z"), beforeExtension, "Illustrative declaration; scheme canonicality and maturity require later verification.")
		add(&f, "declared", "confirmed", fixtureTime, snapshot, "Simulated declaration remains present.")
		old := Facet{ID: "network-legacy-example", Category: "network", Value: "eip155:1", Scope: "x402 v2 · exact · explicit removal example"}
		add(&old, "declared", "first_seen", date("2026-07-02T12:00:00Z"), map[string]any{"declared": true}, "Simulated historical declaration.")
		add(&old, "declared", "confirmed", date("2026-08-02T12:00:00Z"), map[string]any{"declared": true}, "Last simulated confirmation before explicit withdrawal.")
		add(&old, "declared", "removed", date("2026-09-02T12:00:00Z"), map[string]string{"statement": "synthetic explicit withdrawal", "scope": "declaration only"}, "Explicit simulated withdrawal of declaration, not inferred from absence or failure.")
	}
	if version == 1 {
		x := Facet{ID: "extension-bazaar", Category: "extension", Value: "Bazaar", Scope: "No extension declaration available in this v1 fixture"}
		add(&x, "declared", "unknown", fixtureTime, map[string]string{"reason": "v1 fixture provides kinds only"}, "Unknown does not mean unsupported.")
	}
	if i == 4 {
		add(nil, "unknown", "probe_failed", fixtureTime, map[string]string{"error": "simulated timeout"}, "Failed check leaves last-known declarations stale; support is not removed.")
	}
	// The public event stream is chronological, stable for events sharing a timestamp.
	sortEvents(events)
	f := Facilitator{ID: id, Name: names[i], Endpoint: "https://" + id + ".example.invalid", DataMode: DataMode, Fabricated: true, Supported: snapshot, SupportedEvidence: fabricatedEvidence(id, "supported", last, snapshot, "Raw simulated /supported response; not fetched."), Capabilities: Project(events)}
	f.Check = Check{Status: statuses[i], CheckedAt: fixtureTime, Evidence: fabricatedEvidence(id, "check", fixtureTime, map[string]string{"status": statuses[i]}, "Synthetic reachability check, not a live service status.")}
	if i != 4 {
		v := latencies[i]
		f.Check.ResponseMS = &v
	}
	return f, events
}

func (fixtureStore) Standards(ctx context.Context) ([]Standard, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []Standard{
		{ID: "v1", Name: "x402 v1", Category: "protocol", Reference: "https://github.com/coinbase/x402/blob/main/specs/x402-specification-v1.md", Note: "Research supplied with task: /supported has kinds[]; v1 uses legacy network names."},
		{ID: "v2", Name: "x402 v2", Category: "protocol", Reference: "https://github.com/coinbase/x402/blob/main/specs/x402-specification-v2.md", Note: "Research supplied with task: kinds[] with x402Version, scheme, network, optional extra; extensions[] and signers. CAIP-2 network IDs. Canonicality and recent scheme maturity need later verification."},
		{ID: "schemes", Name: "Payment schemes", Category: "scheme", Reference: "https://github.com/coinbase/x402/blob/main/specs/x402-specification-v2.md", Note: "Supplied research names exact, upto, batch-settlement, auth-capture. This prototype does not establish maturity or real facilitator support."},
		{ID: "bazaar", Name: "Bazaar", Category: "extension", Reference: "https://github.com/coinbase/x402/blob/main/specs/extensions/bazaar.md", Note: "Declared extension; discovery endpoints optional. Declaration is separate from optional response extension behavior."},
	}, nil
}
