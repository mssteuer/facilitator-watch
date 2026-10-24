package main

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"sync"
	"time"
)

// SchemeTimeline is a display aggregate of retained checks: shares of checks per
// time bucket, never uptime, continuous support or provider withdrawal.
type SchemeTimeline struct {
	Range       string         `json:"range"`
	From        time.Time      `json:"from"`
	To          time.Time      `json:"to"`
	StepSeconds int64          `json:"step_seconds"`
	FirstCheck  *time.Time     `json:"first_check"`
	Buckets     []SchemeBucket `json:"buckets"`
	Lanes       []SchemeLane   `json:"lanes"`
}

type SchemeBucket struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Checks int       `json:"checks"`
	Failed int       `json:"failed"`
}

// SchemeLane counts, per bucket, checks at or after the scheme's first
// observation. Earlier checks are "before" and never count as omissions.
type SchemeLane struct {
	Scheme    string             `json:"scheme"`
	FirstSeen time.Time          `json:"first_seen"`
	Buckets   []SchemeLaneBucket `json:"buckets"`
}

type SchemeLaneBucket struct {
	Advertised int  `json:"advertised"`
	Omitted    int  `json:"omitted"`
	Failed     int  `json:"failed"`
	Before     int  `json:"before"`
	FirstSeen  bool `json:"first_seen"`
}

// SchemeQuery is either a named range or an explicit window.
type SchemeQuery struct {
	Range    string // 24h, 7d, 30d, 90d, all, or "custom" with From/To
	From, To time.Time
}

const (
	maxSchemeBuckets = 130
	maxSchemeSpan    = 3650 * 24 * time.Hour
	schemeBatch      = 50
)

var schemeSteps = []time.Duration{time.Hour, 3 * time.Hour, 24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour}

var schemeRanges = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour}

func chooseSchemeStep(span time.Duration) time.Duration {
	for _, s := range schemeSteps {
		if span <= s*maxSchemeBuckets {
			return s
		}
	}
	return schemeSteps[len(schemeSteps)-1]
}

type schemeCheck struct {
	at      int64 // Unix seconds, display aggregation only.
	ok      bool
	schemes []string
}

// schemeIndex lazily derives, once per check, which schemes a valid snapshot
// declared. Checks and snapshots are immutable after insert, so it only appends.
// Held in memory so no schema migration is needed and older binaries still open
// the database.
type schemeIndex struct {
	mu     sync.Mutex
	lastID map[string]int64
	checks map[string][]schemeCheck
}

func (s *liveStore) schemeChecks(ctx context.Context, id string) ([]schemeCheck, error) {
	x := &s.schemes
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.lastID == nil {
		x.lastID, x.checks = map[string]int64{}, map[string][]schemeCheck{}
	}
	for {
		type row struct {
			id     int64
			at     string
			status string
			raw    []byte
		}
		var batch []row
		err := func() error {
			s.mu.Lock()
			defer s.mu.Unlock()
			rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.checked_at,c.status,s.raw FROM checks c LEFT JOIN snapshots s ON s.check_id=c.id AND s.valid=1 WHERE c.facilitator_id=? AND c.id>? ORDER BY c.id LIMIT ?`, id, x.lastID[id], schemeBatch)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var r row
				var raw sql.RawBytes
				if err := rows.Scan(&r.id, &r.at, &r.status, &raw); err != nil {
					return err
				}
				r.raw = append([]byte(nil), raw...)
				batch = append(batch, r)
			}
			return rows.Err()
		}()
		if err != nil {
			return nil, err
		}
		for _, r := range batch {
			at, err := time.Parse(time.RFC3339Nano, r.at)
			if err != nil {
				return nil, err
			}
			c := schemeCheck{at: at.Unix(), ok: r.status == "operational"}
			if c.ok {
				if r.raw == nil {
					return nil, errors.New("operational check has no snapshot")
				}
				sup, err := parseSupported(r.raw)
				if err != nil {
					return nil, err
				}
				seen := map[string]bool{}
				for _, k := range sup.Kinds {
					if !seen[k.Scheme] {
						seen[k.Scheme] = true
						c.schemes = append(c.schemes, k.Scheme)
					}
				}
				sort.Strings(c.schemes)
			}
			x.checks[id] = append(x.checks[id], c)
			x.lastID[id] = r.id
		}
		if len(batch) < schemeBatch {
			return x.checks[id], nil
		}
	}
}

func (s *liveStore) SchemeTimeline(ctx context.Context, id string, q SchemeQuery, now time.Time) (SchemeTimeline, error) {
	if _, ok := targetByID(id); !ok {
		return SchemeTimeline{}, ErrNotFound
	}
	checks, err := s.schemeChecks(ctx, id)
	if err != nil {
		return SchemeTimeline{}, err
	}
	return buildSchemeTimeline(checks, q, now), nil
}

func buildSchemeTimeline(checks []schemeCheck, q SchemeQuery, now time.Time) SchemeTimeline {
	out := SchemeTimeline{Range: q.Range, Buckets: []SchemeBucket{}, Lanes: []SchemeLane{}}
	first := map[string]int64{}
	var earliest int64
	for i, c := range checks {
		if i == 0 || c.at < earliest {
			earliest = c.at
		}
		for _, sc := range c.schemes {
			if f, ok := first[sc]; !ok || c.at < f {
				first[sc] = c.at
			}
		}
	}
	if len(checks) > 0 {
		t := time.Unix(earliest, 0).UTC()
		out.FirstCheck = &t
	}
	to := now.UTC().Truncate(time.Second)
	from := to.Add(-24 * time.Hour)
	switch {
	case q.Range == "custom":
		from, to = q.From.UTC(), q.To.UTC()
	case q.Range == "all":
		if out.FirstCheck != nil {
			from = *out.FirstCheck
		}
	default:
		from = to.Add(-schemeRanges[q.Range])
	}
	step := chooseSchemeStep(to.Sub(from))
	start := from.Truncate(step)
	n := int((to.Sub(start) + step - 1) / step)
	if n < 1 {
		n = 1
	}
	out.From, out.To, out.StepSeconds = start, to, int64(step/time.Second)
	for i := 0; i < n; i++ {
		b := SchemeBucket{Start: start.Add(time.Duration(i) * step), End: start.Add(time.Duration(i+1) * step)}
		if b.End.After(to) {
			b.End = to
		}
		out.Buckets = append(out.Buckets, b)
	}
	names := make([]string, 0, len(first))
	for sc := range first {
		names = append(names, sc)
	}
	sort.Strings(names)
	for _, sc := range names {
		out.Lanes = append(out.Lanes, SchemeLane{Scheme: sc, FirstSeen: time.Unix(first[sc], 0).UTC(), Buckets: make([]SchemeLaneBucket, n)})
	}
	index := func(at int64) int {
		t := time.Unix(at, 0).UTC()
		if t.Before(start) || t.After(to) {
			return -1
		}
		i := int(t.Sub(start) / step)
		if i >= n {
			i = n - 1
		}
		return i
	}
	for li, sc := range names {
		if i := index(first[sc]); i >= 0 {
			out.Lanes[li].Buckets[i].FirstSeen = true
		}
	}
	for _, c := range checks {
		i := index(c.at)
		if i < 0 {
			continue
		}
		out.Buckets[i].Checks++
		if !c.ok {
			out.Buckets[i].Failed++
		}
		for li, sc := range names {
			lb := &out.Lanes[li].Buckets[i]
			switch {
			case c.at < first[sc]:
				lb.Before++
			case !c.ok:
				lb.Failed++
			default:
				advertised := false
				for _, s := range c.schemes {
					if s == sc {
						advertised = true
					}
				}
				if advertised {
					lb.Advertised++
				} else {
					lb.Omitted++
				}
			}
		}
	}
	return out
}
