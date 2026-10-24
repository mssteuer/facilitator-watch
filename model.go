package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"
)

var ErrNotFound = errors.New("facilitator not found")

var ErrInvalidHistoryCursor = errors.New("invalid history cursor")

// Store is the read-only boundary; the collector writes outside the HTTP API.
type Store interface {
	List(context.Context) ([]Facilitator, error)
	Get(context.Context, string) (Facilitator, error)
	History(context.Context, string) ([]Event, error)
	Standards(context.Context) ([]Standard, error)
}

type Meta struct {
	DataMode   string    `json:"data_mode"`
	Fabricated bool      `json:"fabricated"`
	AsOf       time.Time `json:"as_of"`
	Disclaimer string    `json:"disclaimer"`
}

type Response[T any] struct {
	Meta
	Data T `json:"data"`
}

type HistoryPage struct {
	Events     []Event   `json:"events"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	Limit      int       `json:"limit"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// SupportedKind preserves the exact protocol spelling and tuple associations.
type SupportedKind struct {
	X402Version int             `json:"x402Version"`
	Scheme      string          `json:"scheme"`
	Network     string          `json:"network"`
	Extra       json.RawMessage `json:"extra,omitempty"`
}

// Optional source fields are never synthesized; Evidence.Raw retains all fields.
type Supported struct {
	Kinds      []SupportedKind      `json:"kinds"`
	Extensions []string             `json:"extensions,omitempty"`
	Signers    *map[string][]string `json:"signers,omitempty"`
}

type Evidence struct {
	CheckID    string          `json:"check_id,omitempty"`
	Source     string          `json:"source"`
	SourceType string          `json:"source_type"`
	CapturedAt time.Time       `json:"captured_at"`
	Raw        json.RawMessage `json:"raw"`
	Note       string          `json:"note"`
	Fabricated bool            `json:"fabricated"`
}

type Check struct {
	Status     string    `json:"status"`
	CheckedAt  time.Time `json:"checked_at"`
	ResponseMS *int      `json:"response_ms"`
	Reachable  *bool     `json:"reachable"`
	HTTPStatus *int      `json:"http_status"`
	Evidence   Evidence  `json:"evidence"`
}

func optionalTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// Unknown capture/check times are JSON null, never fictitious year-one dates.
func (e Evidence) MarshalJSON() ([]byte, error) {
	type plain Evidence
	return json.Marshal(struct {
		plain
		CapturedAt *time.Time `json:"captured_at"`
	}{plain(e), optionalTime(e.CapturedAt)})
}

func (c Check) MarshalJSON() ([]byte, error) {
	type plain Check
	return json.Marshal(struct {
		plain
		CheckedAt *time.Time `json:"checked_at"`
	}{plain(c), optionalTime(c.CheckedAt)})
}

type Facet struct {
	ID       string `json:"id"`
	Category string `json:"category"` // live: kind tuple or extension
	Value    string `json:"value"`
	Scope    string `json:"scope"`
}

// Basis is declared, observed, or unknown. Change is first_seen, confirmed,
// removed, unknown, not_advertised, snapshot or probe_failed.
// A probe failure has no facet or support claim.
type Event struct {
	ID         string    `json:"id"`
	At         time.Time `json:"at"`
	Facet      *Facet    `json:"facet"`
	Basis      string    `json:"basis"`
	Change     string    `json:"change"`
	Evidence   Evidence  `json:"evidence"`
	DataMode   string    `json:"data_mode"`
	Fabricated bool      `json:"fabricated"`
}

type Capability struct {
	Facet         Facet      `json:"facet"`
	Basis         string     `json:"basis"`
	State         string     `json:"state"` // present, stale, not_advertised, removed, unknown
	FirstSeen     *time.Time `json:"first_seen"`
	LastConfirmed *time.Time `json:"last_confirmed"`
	RemovedAt     *time.Time `json:"removed_at"`
	Evidence      Evidence   `json:"evidence"`
}

type Facilitator struct {
	ID                string       `json:"id"`
	Name              string       `json:"name"`
	Endpoint          string       `json:"endpoint"`
	ConfigurationNote string       `json:"configuration_note,omitempty"`
	DataMode          string       `json:"data_mode"`
	Fabricated        bool         `json:"fabricated"`
	Check             Check        `json:"check"`
	Supported         Supported    `json:"supported"`
	SupportedEvidence Evidence     `json:"supported_evidence"`
	Capabilities      []Capability `json:"capabilities"`
}

type Standard struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Category  string `json:"category"`
	Reference string `json:"reference"`
	Note      string `json:"note"`
}

type Summary struct {
	Total            int    `json:"total"`
	Operational      int    `json:"operational"`
	Unavailable      int    `json:"unavailable"`
	Pending          int    `json:"pending"`
	Reachable        int    `json:"reachable"`
	MedianResponseMS *int   `json:"median_response_ms"`
	Scope            string `json:"scope"`
}

func summarize(items []Facilitator) Summary {
	s := Summary{Total: len(items), Scope: "Latest GET /supported checks only. Unconfigured targets count as pending, excluding retained historical checks from operational, reachable and latency counts. Operational means a valid declaration response; reachable means an HTTP response was received. Median covers valid latest responses. No payment, settlement or uptime verification."}
	values := []int{}
	for _, item := range items {
		if item.ConfigurationNote != "" {
			s.Pending++
			continue
		}
		switch item.Check.Status {
		case "operational":
			s.Operational++
		case "unavailable":
			s.Unavailable++
		case "pending":
			s.Pending++
		}
		if item.Check.Reachable != nil && *item.Check.Reachable {
			s.Reachable++
		}
		if item.Check.Status != "unavailable" && item.Check.ResponseMS != nil {
			values = append(values, *item.Check.ResponseMS)
		}
	}
	sort.Ints(values)
	if n := len(values); n > 0 {
		v := values[n/2]
		if n%2 == 0 {
			v = (values[n/2-1] + v) / 2
		}
		s.MedianResponseMS = &v
	}
	return s
}

// Project preserves independent declaration/behavior channels. Unknown/failure
// makes positive evidence stale; it never proves removal. Explicit removal only
// changes the named channel. Input ordering is normalized without mutating it.
func Project(events []Event) []Capability {
	ordered := append([]Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].At.Before(ordered[j].At) })
	return projectOrdered(ordered)
}

// ProjectBySequence projects persisted live events in durable insertion order.
// Wall-clock capture time is retained for display, but never controls current
// state when the host clock moves backwards.
func ProjectBySequence(events []Event) []Capability {
	ordered := append([]Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, _ := strconv.ParseInt(ordered[i].ID, 10, 64)
		b, _ := strconv.ParseInt(ordered[j].ID, 10, 64)
		return a < b
	})
	return projectOrdered(ordered)
}

func projectOrdered(ordered []Event) []Capability {
	states := map[string]*Capability{}
	for _, e := range ordered {
		if e.Change == "probe_failed" {
			for _, c := range states {
				if c.State == "present" {
					c.State = "stale"
				}
			}
			continue
		}
		if e.Facet == nil {
			continue
		}
		key := e.Facet.ID + ":" + e.Basis
		c, ok := states[key]
		if !ok {
			c = &Capability{Facet: *e.Facet, Basis: e.Basis, State: "unknown"}
			states[key] = c
		}
		switch e.Change {
		case "first_seen", "confirmed":
			if c.FirstSeen == nil {
				at := e.At
				c.FirstSeen = &at
			}
			at := e.At
			c.LastConfirmed = &at
			c.RemovedAt = nil
			c.State = "present"
			c.Evidence = e.Evidence
		case "removed":
			at := e.At
			c.RemovedAt = &at
			c.State = "removed"
			c.Evidence = e.Evidence
		case "not_advertised":
			c.State = "not_advertised"
		case "unknown":
			if c.State == "present" || c.State == "stale" {
				c.State = "stale"
			} else if c.State != "removed" && c.State != "not_advertised" {
				c.State = "unknown"
			}
			// Preserve the source of the positive claim; history retains unknown evidence.
			if c.FirstSeen == nil {
				c.Evidence = e.Evidence
			}
		}
	}
	result := make([]Capability, 0, len(states))
	for _, c := range states {
		result = append(result, *c)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Facet.ID == result[j].Facet.ID {
			return result[i].Basis < result[j].Basis
		}
		return result[i].Facet.ID < result[j].Facet.ID
	})
	return result
}

func sortEvents(events []Event) {
	sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
}
