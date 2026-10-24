package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHistoryInvalidCursorsReturn400(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cursor.sqlite")
	s, err := openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	base := time.Now().UTC().Add(-time.Hour)
	recordTest(t, s, validResult(base, v2Snapshot))
	server := NewLiveServer(s)
	request := func(server *Server, id string, q url.Values, want int) string {
		t.Helper()
		w := httptest.NewRecorder()
		server.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/facilitators/"+id+"/history?"+q.Encode(), nil))
		if w.Code != want {
			t.Fatalf("history status=%d want=%d body=%s", w.Code, want, w.Body.String())
		}
		return w.Body.String()
	}
	q := url.Values{"start": {base.Add(-time.Minute).Format(time.RFC3339Nano)}, "end": {base.Add(time.Hour).Format(time.RFC3339Nano)}, "limit": {"1"}}
	var first Response[HistoryPage]
	if err := json.Unmarshal([]byte(request(server, "payai", q, 200)), &first); err != nil || first.Data.NextCursor == "" {
		t.Fatalf("missing cursor: %v", err)
	}
	token := first.Data.NextCursor
	q.Set("cursor", token)
	request(server, "payai", q, 200)
	request(server, "mogami", q, 400)
	q.Set("end", base.Add(2*time.Hour).Format(time.RFC3339Nano))
	request(server, "payai", q, 400)
	q.Set("end", base.Add(time.Hour).Format(time.RFC3339Nano))
	c, err := decodeHistoryCursor(token)
	if err != nil {
		t.Fatal(err)
	}
	c.High++ // Valid encoding and structure, but the original MAC no longer matches.
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	q.Set("cursor", base64.RawURLEncoding.EncodeToString(b))
	request(server, "payai", q, 400)
	q.Del("start") // Default-range cursor parsing must reject the same forgery.
	q.Del("end")
	request(server, "payai", q, 400)
	q.Set("cursor", token)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = openLiveStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	server = NewLiveServer(s)
	request(server, "payai", q, 400) // Reopening rotates the process-local signer.
	q.Del("cursor")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	request(server, "payai", q, 500) // Real DB failures must not become client errors.
}

func TestHistoryRejectsMalformedRawQuery(t *testing.T) {
	server := NewLiveServer(testStore(t))
	for _, query := range []string{"limit=%zz", "unknown=x;y", "limit=1;limit=100"} {
		t.Run(query, func(t *testing.T) {
			w := httptest.NewRecorder()
			server.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/facilitators/payai/history?"+query, nil))
			if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_history_query"`) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestProductionEmbedContainsOnlyRuntimeAssets(t *testing.T) {
	var files []string
	err := fs.WalkDir(frontend, ".", func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"web/app.js", "web/favicon.ico", "web/page.html", "web/style.css"}; !reflect.DeepEqual(files, want) {
		t.Fatalf("unexpected production embed: %v", files)
	}
}

func TestDetailShowsLatestBoundedChronologicalSlice(t *testing.T) {
	for _, large := range []bool{false, true} {
		name := "event budget"
		if large {
			name = "raw byte budget"
		}
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			base := time.Now().UTC().Add(-time.Hour)
			for i := 0; i < 38; i++ {
				raw := v2Snapshot
				if large && i >= 35 {
					raw = strings.TrimSuffix(raw, "}") + `,"padding":"` + strings.Repeat("x", 900000) + `"}`
				}
				recordTest(t, s, validResult(base.Add(time.Duration(i)*time.Minute), raw))
			}
			latest := base.Add(38 * time.Minute)
			recordTest(t, s, pollResult{At: latest, Error: "latest synthetic failure"})
			f, events, err := s.Detail(context.Background(), "payai")
			if err != nil || len(events) == 0 || len(events) > 100 {
				t.Fatalf("detail count=%d err=%v", len(events), err)
			}
			if !large && len(events) != 100 {
				t.Fatalf("expected latest 100 events, got %d", len(events))
			}
			last := events[len(events)-1]
			if last.Change != "probe_failed" || !last.At.Equal(latest) {
				t.Fatal("latest failure missing from detail")
			}
			var rawBytes int
			var observation bool
			for i, e := range events {
				rawBytes += len(e.Evidence.Raw)
				if i > 0 && e.At.Before(events[i-1].At) {
					t.Fatal("detail is not chronological")
				}
				if e.Change == "snapshot" && e.At.Equal(base.Add(37*time.Minute)) {
					observation = true
				}
			}
			if !observation || rawBytes > 2<<20 || (large && rawBytes < 900000) {
				t.Fatalf("latest observation=%v raw bytes=%d", observation, rawBytes)
			}
			for _, c := range f.Capabilities {
				if c.State != "stale" || c.FirstSeen == nil || !c.FirstSeen.Equal(base) || c.LastConfirmed == nil || !c.LastConfirmed.Equal(base.Add(37*time.Minute)) {
					t.Fatal("bounded slice changed independent current projection")
				}
			}
			w := httptest.NewRecorder()
			NewLiveServer(s).ServeHTTP(w, httptest.NewRequest("GET", "/facilitators/payai", nil))
			if w.Code != 200 || !strings.Contains(w.Body.String(), "latest synthetic failure") {
				t.Fatal("latest failure not rendered in ledger")
			}
		})
	}
}
