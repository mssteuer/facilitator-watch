package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func request(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestLiveAPIRoutesAndEmptyState(t *testing.T) {
	store := testStore(t)
	s := NewLiveServer(store)
	for _, path := range []string{"/health", "/api/v1/facilitators", "/api/v1/facilitators/payai", "/api/v1/facilitators/payai/history", "/api/v1/standards", "/api/v1/summary"} {
		w := request(s, "GET", path)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		var envelope struct {
			Meta
			Data json.RawMessage
		}
		if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.DataMode != "live" || envelope.Fabricated || time.Since(envelope.AsOf) > time.Minute || len(envelope.Data) == 0 || !strings.Contains(envelope.Disclaimer, "does not verify payments") {
			t.Fatalf("invalid envelope: %+v", envelope)
		}
	}
	items, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != len(targets()) {
		t.Fatal("fixed target count")
	}
	for _, f := range items {
		if f.Check.Status != "pending" || f.Check.ResponseMS != nil || f.Check.Reachable != nil || len(f.Supported.Kinds) != 0 || f.SupportedEvidence.Raw != nil || len(f.Capabilities) != 0 {
			t.Fatalf("invented evidence: %+v", f)
		}
	}
	if got := request(s, "GET", "/api/v1/facilitators/payai/history").Body.String(); !strings.Contains(got, `"events":[]`) {
		t.Fatal(got)
	}
	if got := request(s, "GET", "/api/v1/facilitators/payai").Body.String(); !strings.Contains(got, `"checked_at":null`) || !strings.Contains(got, `"captured_at":null`) || strings.Contains(got, "0001-01-01") {
		t.Fatal("unknown times must be null", got)
	}
	if got := summarize(items); got.Pending != len(targets()) || got.Operational != 0 || got.MedianResponseMS != nil {
		t.Fatalf("%+v", got)
	}
}

func TestErrorsAndMethods(t *testing.T) {
	s := NewLiveServer(testStore(t))
	for _, path := range []string{"/api/v1/facilitators/missing", "/api/v1/facilitators/missing/history", "/api/v1/facilitators/payai/nope", "/api/v1/facilitators/payai/history/extra", "/api/v1/facilitators/", "/api/v1/unknown", "/api/v1/facilitators/payai/"} {
		w := request(s, "GET", path)
		if w.Code != 404 || !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), `"code":"not_found"`) {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/health", "/api/v1/facilitators", "/api/v1/facilitators/payai", "/api/v1/facilitators/payai/history", "/api/v1/standards", "/api/v1/summary", "/", "/facilitators/payai", "/assets/style.css", "/favicon.ico"} {
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
			w := request(s, method, path)
			if w.Code != 405 || w.Header().Get("Allow") != "GET" {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/api/v1/summary?range=7d", "/api/v1/facilitators/payai/history?url=https://other.invalid", "/api/v1/facilitators/payai/history?start=2026-01-01T00:00:00Z", "/api/v1/facilitators/payai/history?limit=101", "/api/v1/facilitators/payai/history?start=2026-01-01T00:00:00Z&end=2026-01-09T00:00:00Z", "/?status=invalid"} {
		if w := request(s, "GET", path); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestHistoryQueryParsing(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/v1/facilitators/payai/history?start=2026-01-01T00:00:00Z&end=2026-01-08T00:00:00Z&limit=1", nil)
	start, end, limit, _, err := parseHistoryQuery(r)
	if err != nil || !start.Equal(date("2026-01-01T00:00:00Z")) || !end.Equal(date("2026-01-08T00:00:00Z")) || limit != 1 {
		t.Fatalf("parsed %v %v %d %v", start, end, limit, err)
	}
	r = httptest.NewRequest("GET", "/api/v1/facilitators/payai/history", nil)
	start, end, limit, _, err = parseHistoryQuery(r)
	if err != nil || end.Sub(start) != 7*24*time.Hour || limit != 100 {
		t.Fatalf("default bounds: %v %v %d %v", start, end, limit, err)
	}
}

func TestLiveHTMLAndAssets(t *testing.T) {
	store := testStore(t)
	s := NewLiveServer(store)
	for _, populated := range []bool{false, true} {
		if populated {
			recordTest(t, store, validResult(time.Now().Add(-time.Minute), v2Snapshot))
		}
		for _, path := range []string{"/", "/?status=pending", "/standards", "/facilitators/payai", "/facilitators/mogami"} {
			w := request(s, "GET", path)
			if w.Code != 200 {
				t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, wanted := range []string{"Declarations only", "We never test payments", "<style>", "--ink: #141414;", "NO PAYMENT OR SETTLEMENT TESTING"} {
				if !strings.Contains(body, wanted) {
					t.Fatalf("missing %s", wanted)
				}
			}
			if strings.Contains(body, `rel="stylesheet"`) {
				t.Fatalf("%s: page still requests a separate stylesheet", path)
			}
			for _, forbidden := range []string{"SIMULATED", "FABRICATED", "example.invalid", "ZgotmplZ", "<no value>"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("%s: leaked %s", path, forbidden)
				}
			}
			if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing headers")
			}
		}
	}
	w := request(s, "GET", "/?status=pending")
	if strings.Contains(w.Body.String(), `data-href="/facilitators/payai"`) || !strings.Contains(w.Body.String(), `data-href="/facilitators/mogami"`) {
		t.Fatal("filter not applied")
	}
	for _, path := range []string{"/assets/style.css", "/assets/app.js"} {
		if w := request(s, "GET", path); w.Code != 200 || w.Body.Len() == 0 {
			t.Fatalf("asset %s", path)
		}
	}
	for _, path := range []string{"/missing", "/facilitators/missing", "/facilitators/payai/", "/assets/page.html", "/assets/favicon.ico", "/assets/missing.ico", "/favicon.ico/"} {
		if w := request(s, "GET", path); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

type failingStore struct{ Store }

func (failingStore) List(context.Context) ([]Facilitator, error) {
	return nil, errors.New("sensitive implementation detail")
}

func TestStoreFailure(t *testing.T) {
	s := NewLiveServer(failingStore{})
	for _, path := range []string{"/", "/api/v1/facilitators", "/api/v1/summary"} {
		w := request(s, "GET", path)
		if w.Code != 500 || strings.Contains(w.Body.String(), "sensitive implementation detail") {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
}
