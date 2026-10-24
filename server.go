package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web/page.html web/style.css web/app.js web/favicon.ico
var frontend embed.FS

type Server struct {
	store      Store
	meta       Meta
	page       *template.Template
	assets     http.Handler
	stylesheet template.CSS
}

func NewLiveServer(store Store) *Server {
	return newServer(store, Meta{DataMode: "live", Fabricated: false, AsOf: time.Now().UTC(), Disclaimer: "Live passive monitoring of GET /supported declarations only. This does not verify payments, /verify, /settle, or uninterrupted availability."})
}

func newServer(store Store, meta Meta) *Server {
	assets, err := fs.Sub(frontend, "web")
	if err != nil {
		panic(err)
	}
	stylesheet, err := fs.ReadFile(frontend, "web/style.css")
	if err != nil {
		panic(err)
	}
	funcs := template.FuncMap{
		"stamp": func(t time.Time) string {
			if t.IsZero() {
				return "Not yet observed"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"reachable": func(v *bool) string {
			if v == nil {
				return "Not checked"
			}
			if *v {
				return "HTTP response received"
			}
			return "No HTTP response"
		},
		"day": func(t *time.Time) string {
			if t == nil {
				return "Unknown"
			}
			return t.UTC().Format("2006-01-02")
		},
		"ms": func(v *int) string {
			if v == nil {
				return "Unavailable"
			}
			return fmtInt(*v) + " ms"
		},
		"json":               func(v any) string { b, _ := json.MarshalIndent(v, "", "  "); return string(b) },
		"upper":              strings.ToUpper,
		"declarationSummary": summarizeDeclaration,
		"schemeState":        schemeStateLabel,
	}
	return &Server{
		store:      store,
		meta:       meta,
		page:       template.Must(template.New("page.html").Funcs(funcs).ParseFS(frontend, "web/page.html")),
		assets:     http.FileServer(http.FS(assets)),
		stylesheet: template.CSS(stylesheet),
	}
}

// Labels describe mapped tuples in the displayed window, not all current kinds.
func schemeStateLabel(state string) string {
	switch state {
	case "present":
		return "Advertised · window tuples"
	case "stale":
		return "Stale · window tuples"
	case "not_advertised":
		return "Not advertised · window tuples"
	case "mixed tuple states":
		return "Mixed tuple states · window"
	default:
		return "Uncertain · window tuples"
	}
}

// declarationSummary is display-only: independent lists are not a tuple product.
// Use the last valid snapshot, not historical capabilities omitted since then.
type declarationSummary struct {
	Versions string
	Schemes  string
	Networks []string
}

func summarizeDeclaration(s Supported) declarationSummary {
	versions := map[int]bool{}
	schemes, networks := map[string]bool{}, map[string]bool{}
	for _, k := range s.Kinds {
		versions[k.X402Version] = true
		schemes[k.Scheme] = true
		networks[k.Network] = true
	}
	vs := make([]int, 0, len(versions))
	for v := range versions {
		vs = append(vs, v)
	}
	sort.Ints(vs)
	labels := make([]string, 0, len(vs))
	for _, v := range vs {
		labels = append(labels, "v"+strconv.Itoa(v))
	}
	list := func(values map[string]bool) []string {
		list := make([]string, 0, len(values))
		for value := range values {
			list = append(list, value)
		}
		sort.Strings(list)
		return list
	}
	return declarationSummary{strings.Join(labels, ", "), strings.Join(list(schemes), ", "), list(networks)}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Request-local metadata keeps as_of current without mutating shared state.
	copy := *s
	if copy.meta.DataMode == "live" {
		copy.meta.AsOf = time.Now().UTC()
	}
	copy.serveHTTP(w, r)
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'; img-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	api := r.URL.Path == "/health" || strings.HasPrefix(r.URL.Path, "/api/")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		if api {
			s.apiError(w, 405, "method_not_allowed", "Only GET is supported.")
		} else {
			s.htmlError(w, 405, "Method not allowed", "Only GET is supported.")
		}
		return
	}
	if api {
		s.api(w, r)
		return
	}
	switch r.URL.Path {
	case "/favicon.ico":
		w.Header().Set("Content-Type", "image/x-icon")
		s.assets.ServeHTTP(w, r)
	case "/assets/style.css", "/assets/app.js":
		r2 := r.Clone(r.Context())
		u := *r.URL
		r2.URL = &u
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/assets")
		s.assets.ServeHTTP(w, r2)
	case "/":
		s.overview(w, r)
	case "/standards":
		s.standards(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/facilitators/") {
			id := strings.TrimPrefix(r.URL.Path, "/facilitators/")
			if validID(id) {
				s.detail(w, r, id)
				return
			}
		}
		s.htmlError(w, 404, "Page not found", "This page does not exist.")
	}
}

func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, c := range id {
		if c != '-' && (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" && !strings.HasSuffix(r.URL.Path, "/history") && !strings.HasSuffix(r.URL.Path, "/scheme-timeline") {
		s.apiError(w, 400, "unsupported_query", "Query parameters are supported only for history and scheme-timeline.")
		return
	}
	switch r.URL.Path {
	case "/health":
		s.writeJSON(w, 200, Response[any]{Meta: s.meta, Data: struct {
			Status string `json:"status"`
			Scope  string `json:"scope"`
		}{"ok", "Process health only; not database, collector or facilitator health."}})
	case "/api/v1/facilitators":
		items, err := s.store.List(r.Context())
		if s.storeError(w, err, true) {
			return
		}
		s.writeJSON(w, 200, Response[[]Facilitator]{s.meta, items})
	case "/api/v1/standards":
		items, err := s.store.Standards(r.Context())
		if s.storeError(w, err, true) {
			return
		}
		s.writeJSON(w, 200, Response[[]Standard]{s.meta, items})
	case "/api/v1/summary":
		items, err := s.store.List(r.Context())
		if s.storeError(w, err, true) {
			return
		}
		s.writeJSON(w, 200, Response[Summary]{s.meta, summarize(items)})
	default:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/facilitators/"), "/")
		if !strings.HasPrefix(r.URL.Path, "/api/v1/facilitators/") || len(parts) > 2 || !validID(parts[0]) || (len(parts) == 2 && parts[1] != "history" && parts[1] != "scheme-timeline") {
			s.apiError(w, 404, "not_found", "API route not found.")
			return
		}
		if len(parts) == 2 && parts[1] == "scheme-timeline" {
			q, err := parseSchemeQuery(r, s.meta.AsOf)
			if err != nil {
				s.apiError(w, 400, "invalid_scheme_timeline_query", err.Error())
				return
			}
			agg, ok := s.store.(interface {
				SchemeTimeline(context.Context, string, SchemeQuery, time.Time) (SchemeTimeline, error)
			})
			if !ok {
				s.apiError(w, 500, "store_error", "Scheme timeline is unavailable.")
				return
			}
			item, err := agg.SchemeTimeline(r.Context(), parts[0], q, s.meta.AsOf)
			if s.storeError(w, err, true) {
				return
			}
			s.writeJSON(w, 200, Response[SchemeTimeline]{s.meta, item})
			return
		}
		if len(parts) == 2 {
			start, end, limit, cursor, err := parseHistoryQuery(r)
			if err != nil {
				s.apiError(w, 400, "invalid_history_query", err.Error())
				return
			}
			pager, ok := s.store.(interface {
				HistoryPage(context.Context, string, time.Time, time.Time, int, string) (HistoryPage, error)
			})
			if !ok {
				s.apiError(w, 500, "store_error", "History paging is unavailable.")
				return
			}
			items, err := pager.HistoryPage(r.Context(), parts[0], start, end, limit, cursor)
			if s.storeError(w, err, true) {
				return
			}
			s.writeJSON(w, 200, Response[HistoryPage]{s.meta, items})
			return
		}
		item, err := s.store.Get(r.Context(), parts[0])
		if s.storeError(w, err, true) {
			return
		}
		s.writeJSON(w, 200, Response[Facilitator]{s.meta, item})
	}
}

func parseHistoryQuery(r *http.Request) (time.Time, time.Time, int, string, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return time.Time{}, time.Time{}, 0, "", errors.New("malformed history query")
	}
	for k := range q {
		if k != "start" && k != "end" && k != "limit" && k != "cursor" {
			return time.Time{}, time.Time{}, 0, "", errors.New("only start, end, limit and cursor are supported")
		}
		if len(q[k]) != 1 || q.Get(k) == "" {
			return time.Time{}, time.Time{}, 0, "", errors.New("history parameters must appear once and be nonempty")
		}
	}
	startRaw, endRaw := q.Get("start"), q.Get("end")
	if (startRaw == "") != (endRaw == "") {
		return time.Time{}, time.Time{}, 0, "", errors.New("start and end must be supplied together")
	}
	now := time.Now().UTC()
	start, end := now.Add(-7*24*time.Hour), now
	if q.Get("cursor") != "" && startRaw == "" {
		c, err := decodeHistoryCursor(q.Get("cursor"))
		if err != nil {
			return start, end, 0, "", errors.New("invalid history cursor")
		}
		start, err = time.Parse(time.RFC3339Nano, c.Start)
		if err != nil {
			return start, end, 0, "", errors.New("invalid history cursor")
		}
		end, err = time.Parse(time.RFC3339Nano, c.End)
		if err != nil {
			return start, end, 0, "", errors.New("invalid history cursor")
		}
	}
	if startRaw != "" {
		var err error
		start, err = time.Parse(time.RFC3339, startRaw)
		if err != nil {
			return start, end, 0, "", errors.New("start and end must be RFC3339")
		}
		end, err = time.Parse(time.RFC3339, endRaw)
		if err != nil {
			return start, end, 0, "", errors.New("start and end must be RFC3339")
		}
	}
	if !end.After(start) || end.Sub(start) > 7*24*time.Hour {
		return start, end, 0, "", errors.New("history range must be at most 7 days")
	}
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			return start, end, 0, "", errors.New("limit must be between 1 and 100")
		}
		limit = n
	}
	return start.UTC(), end.UTC(), limit, q.Get("cursor"), nil
}

// parseSchemeQuery accepts either range=24h|7d|30d|90d|all or start and end
// together (RFC3339), never both. The window is clamped to now.
func parseSchemeQuery(r *http.Request, now time.Time) (SchemeQuery, error) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return SchemeQuery{}, errors.New("malformed scheme timeline query")
	}
	for k := range q {
		if k != "range" && k != "start" && k != "end" {
			return SchemeQuery{}, errors.New("only range, start and end are supported")
		}
		if len(q[k]) != 1 || q.Get(k) == "" {
			return SchemeQuery{}, errors.New("scheme timeline parameters must appear once and be nonempty")
		}
	}
	rng, startRaw, endRaw := q.Get("range"), q.Get("start"), q.Get("end")
	if rng != "" {
		if startRaw != "" || endRaw != "" {
			return SchemeQuery{}, errors.New("use range or start and end, not both")
		}
		if _, ok := schemeRanges[rng]; !ok && rng != "all" {
			return SchemeQuery{}, errors.New("range must be 24h, 7d, 30d, 90d or all")
		}
		return SchemeQuery{Range: rng}, nil
	}
	if startRaw == "" && endRaw == "" {
		return SchemeQuery{Range: "30d"}, nil
	}
	if startRaw == "" || endRaw == "" {
		return SchemeQuery{}, errors.New("start and end must be supplied together")
	}
	start, err1 := time.Parse(time.RFC3339, startRaw)
	end, err2 := time.Parse(time.RFC3339, endRaw)
	if err1 != nil || err2 != nil {
		return SchemeQuery{}, errors.New("start and end must be RFC3339")
	}
	if end.After(now) {
		end = now
	}
	if !end.After(start) || end.Sub(start) > maxSchemeSpan {
		return SchemeQuery{}, errors.New("window must be positive and at most 3650 days")
	}
	return SchemeQuery{Range: "custom", From: start.UTC(), To: end.UTC()}, nil
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) apiError(w http.ResponseWriter, status int, code, message string) {
	s.writeJSON(w, status, struct {
		Meta
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Meta: s.meta, Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
}

func (s *Server) storeError(w http.ResponseWriter, err error, api bool) bool {
	if err == nil {
		return false
	}
	status, code, message := 500, "store_error", "The evidence store is unavailable."
	if errors.Is(err, ErrNotFound) {
		status, code, message = 404, "not_found", "Facilitator not found."
	}
	if errors.Is(err, ErrInvalidHistoryCursor) {
		status, code, message = 400, "invalid_history_query", "Invalid or expired history cursor."
	}
	if api {
		s.apiError(w, status, code, message)
	} else {
		s.htmlError(w, status, message, "Return to the overview to browse monitored endpoints.")
	}
	return true
}

type pageData struct {
	Meta
	Stylesheet  template.CSS
	Title       string
	View        string
	Items       []Facilitator
	Summary     Summary
	Detail      Facilitator
	Events      []Event
	Standards   []Standard
	Chart       Chart
	SchemeChart Chart
	Filter      string
	Message     string
}

func (s *Server) render(w http.ResponseWriter, status int, data pageData) {
	data.Meta = s.meta
	data.Stylesheet = s.stylesheet
	var b bytes.Buffer
	if err := s.page.ExecuteTemplate(&b, "page.html", data); err != nil {
		http.Error(w, "Page rendering failed", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b.Bytes())
}

func (s *Server) htmlError(w http.ResponseWriter, status int, title, message string) {
	s.render(w, status, pageData{Title: title, View: "error", Message: message})
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.List(r.Context())
	if s.storeError(w, err, false) {
		return
	}
	summary := summarize(items)
	filter := r.URL.Query().Get("status")
	if filter != "" && filter != "operational" && filter != "pending" && filter != "unavailable" {
		s.htmlError(w, 400, "Unknown filter", "Use operational, pending or unavailable.")
		return
	}
	if filter != "" {
		filtered := []Facilitator{}
		for _, f := range items {
			if f.Check.Status == filter {
				filtered = append(filtered, f)
			}
		}
		items = filtered
	}
	s.render(w, 200, pageData{Title: "Facilitator status", View: "overview", Items: items, Summary: summary, Filter: filter})
}

func (s *Server) detail(w http.ResponseWriter, r *http.Request, id string) {
	var f Facilitator
	var events []Event
	var err error
	if store, ok := s.store.(interface {
		Detail(context.Context, string) (Facilitator, []Event, error)
	}); ok {
		f, events, err = store.Detail(r.Context(), id)
	} else {
		f, err = s.store.Get(r.Context(), id)
		if err == nil {
			events, err = s.store.History(r.Context(), id)
		}
	}
	if s.storeError(w, err, false) {
		return
	}
	if s.meta.DataMode == "live" {
		s.meta.AsOf = time.Now().UTC()
	}
	s.render(w, 200, pageData{Title: f.Name + " · declarations", View: "detail", Detail: f, Events: events, Chart: BuildChart(events, s.meta.AsOf), SchemeChart: BuildSchemeChart(events, f.Supported, s.meta.AsOf)})
}

func (s *Server) standards(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.Standards(r.Context())
	if s.storeError(w, err, false) {
		return
	}
	s.render(w, 200, pageData{Title: "Standards & evidence", View: "standards", Standards: items})
}
