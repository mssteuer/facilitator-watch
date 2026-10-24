package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDeclarationSummaryOnlyAdvertisedValues(t *testing.T) {
	s := Supported{Kinds: []SupportedKind{
		{X402Version: 2, Scheme: "upto", Network: "eip155:1"},
		{X402Version: 1, Scheme: "exact", Network: "base"},
		{X402Version: 2, Scheme: "batch-settlement", Network: "eip155:8453"},
		{X402Version: 2, Scheme: "exact", Network: "eip155:8453"},
		{X402Version: 2, Scheme: "exact", Network: "eip155:8453"},
		{X402Version: 10, Scheme: "future-scheme", Network: "future:network"},
	}}
	before, _ := json.Marshal(s)
	want := declarationSummary{"v1, v2, v10", "batch-settlement, exact, future-scheme, upto", []string{"base", "eip155:1", "eip155:8453", "future:network"}}
	if got := summarizeDeclaration(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary: %+v", got)
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("summary mutated tuples")
	}
	if got := summarizeDeclaration(Supported{}); got.Versions != "" || got.Schemes != "" || len(got.Networks) != 0 {
		t.Fatalf("invented values: %+v", got)
	}
}

func TestCompactProfileAndAdvancedDisclosure(t *testing.T) {
	store := testStore(t)
	s := NewLiveServer(store)
	compact := func() string {
		t.Helper()
		w := request(s, "GET", "/facilitators/payai")
		if w.Code != 200 {
			t.Fatalf("profile: %d %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		parts := strings.Split(body, `<details class="advanced-evidence">`)
		if len(parts) != 2 || !strings.Contains(parts[1], "<summary>Advanced evidence") || strings.Contains(body, "<details open") {
			t.Fatal("missing closed native advanced disclosure")
		}
		for _, heading := range []string{"Declaration evidence over time", "Current capability evidence", "Raw last successful /supported", "Evidence ledger"} {
			if strings.Contains(parts[0], heading) || !strings.Contains(parts[1], heading) {
				t.Fatalf("%s must be inside advanced evidence", heading)
			}
		}
		if !strings.Contains(parts[0], "Scheme timeline") || strings.Contains(parts[1], "Scheme timeline") || strings.Count(body, `class="support-chart scheme-chart"`) != 1 {
			t.Fatal("one compact scheme timeline must be visible outside advanced evidence")
		}
		if !strings.Contains(parts[0], `id="scheme-timeline" role="region" aria-labelledby="scheme-timeline-title"`) || !strings.Contains(parts[0], `data-api="/api/v1/facilitators/payai/scheme-timeline"`) {
			t.Fatal("scheme timeline container or data-api path missing")
		}
		chartAt := strings.Index(parts[0], `class="support-chart scheme-chart"`)
		if open, shut := strings.Index(parts[0], "<noscript>"), strings.Index(parts[0], "</noscript>"); open < 0 || open > chartAt || shut < chartAt {
			t.Fatal("server-rendered scheme chart must live in the noscript fallback")
		}
		for _, warning := range []string{"at least one network", "not verified payments or continuous support", "Network-specific detail is in Advanced evidence", "shares of checks, not uptime", "failed check never proves removal", "Gaps with no checks stay blank", "not a launch date"} {
			if !strings.Contains(parts[0], warning) {
				t.Fatalf("missing compact timeline caveat: %s", warning)
			}
		}
		if !strings.Contains(parts[0], "Independent lists, not every possible combination") {
			t.Fatal("missing tuple-association warning")
		}
		if !strings.Contains(parts[0], `aria-describedby="scheme-timeline-description scheme-timeline-note"`) {
			t.Fatal("timeline region must reference its evidence caveats")
		}
		if strings.Contains(parts[0], "Latest check:</b>") || !strings.Contains(parts[1], "Latest check:</b>") {
			t.Fatal("technical check note must remain available in advanced evidence")
		}
		return parts[0]
	}
	if body := compact(); !strings.Contains(body, "No successful declaration evidence yet.") || strings.Contains(body, "<dt>Networks</dt>") || strings.Contains(body, `class="axis-track"`) || strings.Contains(body, `class="timeline-legend`) {
		t.Fatal("empty state invented declaration values")
	}
	at := time.Now().UTC().Add(-time.Hour)
	recordTest(t, store, validResult(at, v2Snapshot))
	body := compact()
	if !strings.Contains(body, `class="timeline-legend scheme-legend"`) || !strings.Contains(body, "Advertised · window tuples") {
		t.Fatal("observed schemes need a marker legend and scoped readable state")
	}
	for _, want := range []string{"<dd>v2</dd>", "<dd>exact, upto</dd>", "<summary>2 networks</summary>", "<li>eip155:1</li>", "<li>eip155:8453</li>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing compact value: %s", want)
		}
	}
	var envelope Response[Facilitator]
	if err := json.Unmarshal(request(s, "GET", "/api/v1/facilitators/payai").Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(envelope.Data.Supported.Kinds, mustParse(t, v2Snapshot).Kinds) {
		t.Fatal("API lost exact tuple associations")
	}
	recordTest(t, store, pollResult{At: at.Add(time.Minute), Error: "timeout"})
	if body := compact(); !strings.Contains(body, "Stale evidence:") || !strings.Contains(body, "<dd>exact, upto</dd>") {
		t.Fatal("failure must retain values with stale warning")
	}
	for i := 2; i <= 4; i++ {
		recordTest(t, store, validResult(at.Add(time.Duration(i)*time.Minute), emptySnapshot))
		body := compact()
		if !strings.Contains(body, "advertised no kinds") || strings.Contains(body, "<dd>exact, upto</dd>") || strings.Contains(body, "Stale evidence:") {
			t.Fatal("omitted values leaked from historical capabilities into summary")
		}
	}
	if body := request(s, "GET", "/facilitators/payai").Body.String(); !strings.Contains(body, "not_advertised") {
		t.Fatal("historical omission states must remain available")
	}
}

func TestSchemeStateLabelsStayScopedToWindowTuples(t *testing.T) {
	for state, want := range map[string]string{
		"present":            "Advertised · window tuples",
		"stale":              "Stale · window tuples",
		"not_advertised":     "Not advertised · window tuples",
		"mixed tuple states": "Mixed tuple states · window",
		"unknown":            "Uncertain · window tuples",
		"future-state":       "Uncertain · window tuples",
	} {
		if got := schemeStateLabel(state); got != want {
			t.Errorf("%s: %s", state, got)
		}
	}
}

func TestEditorialLayoutAcrossPages(t *testing.T) {
	s := NewLiveServer(testStore(t))
	for _, path := range []string{"/", "/?status=pending", "/facilitators/payai", "/standards", "/missing"} {
		body := request(s, "GET", path).Body.String()
		for _, want := range []string{`class="masthead"`, `aria-label="Main navigation"`, `href="#content"`, `id="content"`, `<link rel="icon" href="/favicon.ico" sizes="16x16 32x32 48x48" type="image/x-icon">`, "Declarations only", "not verified payments, settlement or uninterrupted availability."} {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing shared layout element %s", path, want)
			}
		}
		if strings.Contains(body, "<aside>") || strings.Contains(body, `class="side-bottom"`) || strings.Contains(body, "Design mockup") {
			t.Errorf("%s contains old sidebar or mockup content", path)
		}
	}
}

func TestIndexSummaryKeepsEmptyStaleAndOmittedDistinct(t *testing.T) {
	store := testStore(t)
	s := NewLiveServer(store)
	index := func() string {
		t.Helper()
		w := request(s, "GET", "/")
		if w.Code != 200 {
			t.Fatalf("index: %d", w.Code)
		}
		return w.Body.String()
	}
	if body := index(); strings.Contains(body, `class="index-facts"`) || strings.Contains(body, "Stale / latest check failed") {
		t.Fatal("pending index invented evidence")
	}
	at := time.Now().UTC().Add(-time.Hour)
	recordTest(t, store, pollResult{At: at, Error: "timeout"})
	if body := index(); strings.Contains(body, `class="index-facts"`) || strings.Contains(body, "Stale / latest check failed") {
		t.Fatal("failure before first success invented stale declaration")
	}
	recordTest(t, store, validResult(at.Add(time.Minute), v2Snapshot))
	for _, want := range []string{`class="index-facts"`, "<dd>v2</dd>", "<dd>exact, upto</dd>", "<summary>2 networks</summary>", "<li>eip155:1</li>", "<li>eip155:8453</li>"} {
		if !strings.Contains(index(), want) {
			t.Errorf("index missing advertised summary: %s", want)
		}
	}
	recordTest(t, store, pollResult{At: at.Add(2 * time.Minute), Error: "timeout"})
	if body := index(); !strings.Contains(body, "Stale / latest check failed") || !strings.Contains(body, "<dd>exact, upto</dd>") {
		t.Fatal("failed check lost stale summary")
	}
	recordTest(t, store, validResult(at.Add(3*time.Minute), emptySnapshot))
	if body := index(); !strings.Contains(body, "Advertised no kinds") || strings.Contains(body, `class="index-facts"`) || strings.Contains(body, "Stale / latest check failed") {
		t.Fatal("empty success must not use historical tuples or stale warning")
	}
}

func TestNetworkDisclosureKeepsLargeDeclarationsCompactAndExact(t *testing.T) {
	store := testStore(t)
	s := NewLiveServer(store)
	signers := map[string][]string{}
	declaration := Supported{Extensions: []string{"test-extension"}, Signers: &signers}
	// Legacy and CAIP identifiers stay distinct; no guessed aliases or labels.
	for _, network := range []string{"base", "eip155:8453", "solana:mainnet"} {
		declaration.Kinds = append(declaration.Kinds, SupportedKind{X402Version: 2, Scheme: "exact", Network: network})
	}
	for i := 0; i < 60; i++ {
		declaration.Kinds = append(declaration.Kinds, SupportedKind{X402Version: 2, Scheme: "exact", Network: fmt.Sprintf("solana:%s%02d", strings.Repeat("Ab9", 32), i)})
	}
	// Duplicate tuples do not inflate the network count.
	declaration.Kinds = append(declaration.Kinds, declaration.Kinds[0])
	raw, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(-time.Hour)
	recordTest(t, store, validResult(at, string(raw)))
	check := func() {
		t.Helper()
		for _, path := range []string{"/", "/facilitators/payai"} {
			body := request(s, "GET", path).Body.String()
			start := strings.Index(body, `<details class="network-disclosure">`)
			if start < 0 {
				t.Fatalf("%s missing closed native network disclosure", path)
			}
			end := strings.Index(body[start:], "</details>")
			if end < 0 {
				t.Fatal("unclosed network disclosure")
			}
			disclosure := body[start : start+end+len("</details>")]
			styleEnd := strings.Index(body, "</style>")
			if styleEnd < 0 || styleEnd >= start {
				t.Fatal("inline stylesheet missing before page content")
			}
			pageContent := body[styleEnd+len("</style>") : start]
			if !strings.Contains(disclosure, "<summary>63 networks</summary>") || strings.Contains(disclosure, " open") {
				t.Fatalf("%s must default to count only", path)
			}
			for _, kind := range declaration.Kinds {
				if !strings.Contains(disclosure, "<li>"+kind.Network+"</li>") {
					t.Fatalf("%s lost exact network %s", path, kind.Network)
				}
				if strings.Contains(pageContent, kind.Network) {
					t.Fatalf("%s network leaked before disclosure", path)
				}
			}
			if strings.Count(disclosure, "<li>") != 63 || !strings.Contains(disclosure, `tabindex="0"`) {
				t.Fatal("full list must be deduplicated and keyboard-scrollable")
			}
		}
	}
	check()
	recordTest(t, store, pollResult{At: at.Add(time.Minute), Error: "timeout"})
	check()
	var envelope Response[Facilitator]
	if err := json.Unmarshal(request(s, "GET", "/api/v1/facilitators/payai").Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(envelope.Data.Supported.Kinds, declaration.Kinds) || string(envelope.Data.SupportedEvidence.Raw) != string(raw) {
		t.Fatal("disclosure changed API tuples or retained raw evidence")
	}
	css := request(s, "GET", "/assets/style.css").Body.String()
	for _, rule := range []string{".overview-table { table-layout: fixed; }", "width: 34%", "max-height: 16rem", "overflow-y: auto", "overflow-wrap: anywhere", "word-break: break-word", "minmax(min(100%, 18rem), 1fr)"} {
		if !strings.Contains(css, rule) {
			t.Errorf("missing overflow protection: %s", rule)
		}
	}
	js := request(s, "GET", "/assets/app.js").Body.String()
	if !strings.Contains(js, "closest('a, button, details')") {
		t.Fatal("network disclosure interaction must not navigate the provider row")
	}
}

func TestMobileOverviewStacksWithoutDuplicatingData(t *testing.T) {
	s := NewLiveServer(testStore(t))
	body := request(s, "GET", "/").Body.String()
	if strings.Count(body, `class="overview-table"`) != 1 || strings.Count(body, `class="provider-name"`) != len(targets()) {
		t.Fatal("mobile overview must reuse the same table and provider data")
	}
	for _, column := range []struct{ id, label string }{
		{"provider", "Facilitator"},
		{"check", "Latest check"},
		{"capture", "Last successful declaration"},
		{"summary", "Advertised summary"},
	} {
		if !strings.Contains(body, `scope="col" id="overview-`+column.id+`">`+column.label+`</th>`) || strings.Count(body, `headers="overview-`+column.id+`" data-label="`+column.label+`"`) != len(targets()) {
			t.Errorf("missing accessible header association or mobile label: %s", column.label)
		}
	}
	for _, role := range []string{"table", "rowgroup", "row", "columnheader", "cell"} {
		if !strings.Contains(body, `role="`+role+`"`) {
			t.Errorf("missing explicit %s semantics for CSS-reflowed table", role)
		}
	}
	css := request(s, "GET", "/assets/style.css").Body.String()
	_, mobile, ok := strings.Cut(css, "@media (max-width: 760px)")
	if !ok {
		t.Fatal("missing mobile breakpoint")
	}
	for _, rule := range []string{
		".overview-table { display: block; min-width: 0; width: 100%; }",
		".overview-table tbody { display: block; }",
		".overview-table tbody > tr { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr));",
		".overview-table td { display: block; min-width: 0; padding: 0; border: 0; }",
		".overview-table td:first-child, .overview-table td:last-child { grid-column: 1 / -1; }",
		"content: attr(data-label)",
		".overview-table thead { position: absolute; width: 1px; height: 1px;",
	} {
		if !strings.Contains(mobile, rule) {
			t.Errorf("missing mobile no-sideways-scroll contract: %s", rule)
		}
	}
	if strings.Contains(mobile, ".table-card table { min-width:") || strings.Contains(mobile, ".overview-table thead { display: none") {
		t.Fatal("mobile overview must not force a wide table or remove accessible headers")
	}
	if !strings.Contains(css, ".capability-table { min-width: 900px; }") || !strings.Contains(css, ".table-wrap { overflow-x: auto; }") {
		t.Fatal("advanced tables must retain their separate scrolling regions")
	}
}

// The CSP allows img-src 'self' only, so a data: URI background would silently not render.
func TestStylesheetUsesNoDataURIs(t *testing.T) {
	css, err := frontend.ReadFile("web/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(css), "data:") {
		t.Fatal("style.css must not use data: URIs under img-src 'self'")
	}
}
