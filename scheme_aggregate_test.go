package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

const exactOnlySnapshot = `{"kinds":[{"x402Version":2,"scheme":"exact","network":"eip155:8453"}],"extensions":[],"signers":{}}`

func failedResult(at time.Time) pollResult {
	return pollResult{At: at, MS: 5, Error: "transport error"}
}

func TestSchemeTimelineAggregatesChecksWithoutInferringSupport(t *testing.T) {
	s := testStore(t)
	now := date("2026-10-24T12:00:00Z")
	day := func(d, h int) time.Time {
		return now.Add(-time.Duration(d)*24*time.Hour + time.Duration(h)*time.Hour - 12*time.Hour)
	}
	// Day -3: exact only. Day -2: exact + upto first seen. Day -1: failure, then exact only (upto omitted).
	recordTest(t, s, validResult(day(3, 1), exactOnlySnapshot))
	recordTest(t, s, validResult(day(2, 1), v2Snapshot))
	recordTest(t, s, validResult(day(2, 2), v2Snapshot))
	recordTest(t, s, failedResult(day(1, 1)))
	recordTest(t, s, validResult(day(1, 2), exactOnlySnapshot))
	got, err := s.SchemeTimeline(context.Background(), targets()[0].ID, SchemeQuery{Range: "7d"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.StepSeconds != 3*3600 {
		t.Fatalf("7d step = %d", got.StepSeconds)
	}
	if len(got.Lanes) != 2 || got.Lanes[0].Scheme != "exact" || got.Lanes[1].Scheme != "upto" {
		t.Fatalf("lanes = %+v", got.Lanes)
	}
	sum := func(l SchemeLane) (a, o, f, b int) {
		for _, x := range l.Buckets {
			a, o, f, b = a+x.Advertised, o+x.Omitted, f+x.Failed, b+x.Before
		}
		return
	}
	checks, failed := 0, 0
	for _, b := range got.Buckets {
		checks, failed = checks+b.Checks, failed+b.Failed
	}
	if checks != 5 || failed != 1 {
		t.Fatalf("checks=%d failed=%d", checks, failed)
	}
	if a, o, f, b := sum(got.Lanes[0]); a != 4 || o != 0 || f != 1 || b != 0 {
		t.Fatalf("exact a=%d o=%d f=%d before=%d", a, o, f, b)
	}
	// upto: one check before first seen; the failure and the later omission count after it.
	if a, o, f, b := sum(got.Lanes[1]); a != 2 || o != 1 || f != 1 || b != 1 {
		t.Fatalf("upto a=%d o=%d f=%d before=%d", a, o, f, b)
	}
	marks := 0
	for _, x := range got.Lanes[1].Buckets {
		if x.FirstSeen {
			marks++
		}
	}
	if marks != 1 || !got.Lanes[1].FirstSeen.Equal(day(2, 1)) {
		t.Fatalf("first seen marks=%d at %s", marks, got.Lanes[1].FirstSeen)
	}
}

func TestSchemeTimelineIndexIsIncrementalAndRangesChooseBoundedBuckets(t *testing.T) {
	s := testStore(t)
	now := date("2026-10-24T12:00:00Z")
	recordTest(t, s, validResult(now.Add(-200*24*time.Hour), exactOnlySnapshot))
	if got, err := s.SchemeTimeline(context.Background(), targets()[0].ID, SchemeQuery{Range: "24h"}, now); err != nil || len(got.Lanes) != 1 || len(got.Buckets) < 24 || len(got.Buckets) > 25 {
		t.Fatalf("24h lanes/buckets = %+v err=%v", got, err)
	}
	recordTest(t, s, validResult(now.Add(-time.Hour), exactOnlySnapshot))
	for _, rng := range []string{"24h", "7d", "30d", "90d", "all"} {
		got, err := s.SchemeTimeline(context.Background(), targets()[0].ID, SchemeQuery{Range: rng}, now)
		if err != nil || len(got.Buckets) < 1 || len(got.Buckets) > maxSchemeBuckets+1 {
			t.Fatalf("%s buckets=%d err=%v", rng, len(got.Buckets), err)
		}
		for _, l := range got.Lanes {
			if len(l.Buckets) != len(got.Buckets) {
				t.Fatalf("%s lane/bucket length mismatch", rng)
			}
		}
		if rng == "24h" {
			n := 0
			for _, b := range got.Buckets {
				n += b.Checks
			}
			if n != 1 {
				t.Fatalf("24h counted %d checks; the new check was not indexed", n)
			}
		}
	}
	if got, _ := s.SchemeTimeline(context.Background(), targets()[0].ID, SchemeQuery{Range: "all"}, now); got.StepSeconds != 7*24*3600 || got.FirstCheck == nil {
		t.Fatalf("all step=%d first=%v", got.StepSeconds, got.FirstCheck)
	}
}

func TestSchemeTimelineEmptyAndUnknown(t *testing.T) {
	s := testStore(t)
	got, err := s.SchemeTimeline(context.Background(), targets()[0].ID, SchemeQuery{Range: "all"}, date("2026-10-24T12:00:00Z"))
	if err != nil || got.FirstCheck != nil || len(got.Lanes) != 0 || len(got.Buckets) == 0 {
		t.Fatalf("empty = %+v err=%v", got, err)
	}
	if _, err = s.SchemeTimeline(context.Background(), "nope", SchemeQuery{Range: "7d"}, time.Now()); err != ErrNotFound {
		t.Fatalf("unknown err = %v", err)
	}
}

func TestSchemeTimelineAPIEnvelopeAndQueryValidation(t *testing.T) {
	s := testStore(t)
	recordTest(t, s, validResult(time.Now().UTC().Add(-time.Hour), exactOnlySnapshot))
	server := NewLiveServer(s)
	id := targets()[0].ID
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		server.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	w := get("/api/v1/facilitators/" + id + "/scheme-timeline?range=7d")
	var env struct {
		Fabricated bool
		Data       SchemeTimeline
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &env) != nil || len(env.Data.Lanes) != 1 || env.Fabricated {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w = get("/api/v1/facilitators/" + id + "/scheme-timeline"); w.Code != 200 {
		t.Fatalf("default range %d", w.Code)
	}
	now := time.Now().UTC()
	win := "start=" + now.Add(-48*time.Hour).Format(time.RFC3339) + "&end=" + now.Format(time.RFC3339)
	if w = get("/api/v1/facilitators/" + id + "/scheme-timeline?" + win); w.Code != 200 {
		t.Fatalf("window %d %s", w.Code, w.Body)
	}
	for _, bad := range []string{"range=1y", "range=7d&start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z", "start=2026-01-01T00:00:00Z", "start=2026-01-02T00:00:00Z&end=2026-01-01T00:00:00Z", "start=2000-01-01T00:00:00Z&end=2026-01-01T00:00:00Z", "range=7d&range=30d", "limit=5", "range="} {
		if w = get("/api/v1/facilitators/" + id + "/scheme-timeline?" + bad); w.Code != 400 {
			t.Fatalf("%q = %d", bad, w.Code)
		}
	}
	if w = get("/api/v1/facilitators/nope/scheme-timeline?range=7d"); w.Code != 404 {
		t.Fatalf("unknown %d", w.Code)
	}
	if w = get("/api/v1/facilitators/" + id + "/other"); w.Code != 404 {
		t.Fatalf("other %d", w.Code)
	}
	if w = get("/api/v1/facilitators/" + id + "?range=7d"); w.Code != 400 {
		t.Fatalf("detail query %d", w.Code)
	}
}
