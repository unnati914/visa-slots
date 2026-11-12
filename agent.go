package main

import (
	"context"
	"encoding/json"
	"log"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// Source is one place slot data comes from. To add a real website, implement this
// interface and register it in main.go. Fetch returns every row the source currently
// knows about. Rows it stops returning are removed, so a full picture is required each time.
type Source interface {
	Name() string
	Interval() time.Duration
	Fetch(ctx context.Context) ([]Row, error)
}

type SourceStatus struct {
	Name        string `json:"name"`
	Rows        int    `json:"rows"`
	LastOK      int64  `json:"lastOk,omitempty"`
	LastError   string `json:"lastError,omitempty"`
	LastErrorAt int64  `json:"lastErrorAt,omitempty"`
	Failures    int    `json:"consecutiveFailures"`
	FetchMs     int64  `json:"fetchMs"`
	Healthy     bool   `json:"healthy"`
}

// Kicker is an optional Source extra: a source that receives pushed data (like the
// browser agent's reports) signals here so it is published at once, not on the next tick.
type Kicker interface{ Kick() <-chan struct{} }

type fetchResult struct {
	source string
	rows   []Row
	err    error
	took   time.Duration
}

// Agent polls every source on its own schedule, merges results, and publishes snapshots.
// The merge runs in a single goroutine, so it needs no locks.
type Agent struct {
	sources []Source
	store   *Store
	hub     *Hub
	results chan fetchResult

	bySource map[string]map[string]Row // source -> key -> row
	merged   map[string]Row
	status   map[string]*SourceStatus
	version  uint64
	statusMu sync.Mutex

	// OnChange, if set, gets every row whose availability changed (removed rows come
	// with Open 0). It runs on the merge goroutine, so it must not block.
	OnChange func([]Row)
}

func NewAgent(store *Store, hub *Hub, sources ...Source) *Agent {
	a := &Agent{sources: sources, store: store, hub: hub,
		results:  make(chan fetchResult, 64),
		bySource: map[string]map[string]Row{}, merged: map[string]Row{},
		status: map[string]*SourceStatus{}}
	for _, s := range sources {
		a.status[s.Name()] = &SourceStatus{Name: s.Name()}
	}
	return a
}

func (a *Agent) Run(ctx context.Context) {
	// Publish an empty snapshot so the API answers immediately.
	a.store.cur.Store(buildSnapshot(0, nil, a.statusList()))
	for _, s := range a.sources {
		go a.poll(ctx, s)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-a.results:
			batch := []fetchResult{r}
			// Coalesce results that arrive together into one snapshot rebuild.
		drain:
			for {
				select {
				case more := <-a.results:
					batch = append(batch, more)
				default:
					break drain
				}
			}
			a.apply(batch)
		}
	}
}

// poll fetches one source forever, backing off exponentially (with jitter) on errors.
func (a *Agent) poll(ctx context.Context, s Source) {
	failures := 0
	for {
		fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		start := time.Now()
		rows, err := s.Fetch(fctx)
		cancel()
		select {
		case a.results <- fetchResult{s.Name(), rows, err, time.Since(start)}:
		case <-ctx.Done():
			return
		}
		wait := s.Interval()
		if err != nil {
			failures++
			wait = min(s.Interval()<<min(failures, 8), 5*time.Minute)
		} else {
			failures = 0
		}
		wait += time.Duration(rand.Int64N(int64(wait)/5 + 1)) // up to 20% jitter
		var kick <-chan struct{}
		if k, ok := s.(Kicker); ok {
			kick = k.Kick()
		}
		select {
		case <-time.After(wait):
		case <-kick:
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) apply(batch []fetchResult) {
	now := time.Now().UnixMilli()
	a.statusMu.Lock()
	for _, r := range batch {
		st := a.status[r.source]
		st.FetchMs = r.took.Milliseconds()
		if r.err != nil {
			st.Failures++
			st.LastError, st.LastErrorAt = r.err.Error(), now
			st.Healthy = st.Failures < 3
			log.Printf("source %s: fetch failed (%d in a row): %v", r.source, st.Failures, r.err)
			continue // keep the last good data from this source
		}
		st.Failures, st.LastOK, st.Healthy, st.Rows = 0, now, true, len(r.rows)
		m := make(map[string]Row, len(r.rows))
		for _, row := range r.rows {
			row.Source = r.source
			m[row.Key] = row
		}
		a.bySource[r.source] = m
	}
	a.statusMu.Unlock()

	// Merge: when several sources report the same row, trust the freshest check.
	next := make(map[string]Row, len(a.merged))
	for _, rows := range a.bySource {
		for k, row := range rows {
			if cur, ok := next[k]; !ok || row.CheckedAt > cur.CheckedAt ||
				(row.CheckedAt == cur.CheckedAt && row.Earliest != "" && (cur.Earliest == "" || row.Earliest < cur.Earliest)) {
				next[k] = row
			}
		}
	}

	var changed []Row
	var removed []string
	for k, row := range next {
		prev, ok := a.merged[k]
		if !ok || !sameAvailability(prev, row) {
			row.ChangedAt = now
			changed = append(changed, row)
		} else {
			row.ChangedAt = prev.ChangedAt
		}
		next[k] = row
	}
	for k := range a.merged {
		if _, ok := next[k]; !ok {
			removed = append(removed, k)
		}
	}
	a.merged = next

	a.version++
	rows := make([]Row, 0, len(next))
	for _, r := range next {
		rows = append(rows, r)
	}
	a.store.cur.Store(buildSnapshot(a.version, rows, a.statusList()))

	// Broadcast per country: a viewer gets only its own country's changes. Everyone
	// else (including the worldwide view) gets a tiny version tick.
	type diff struct {
		Version uint64   `json:"v"`
		Changed []Row    `json:"changed"`
		Removed []string `json:"removed,omitempty"`
	}
	per := map[string]*diff{}
	get := func(cc string) *diff {
		if per[cc] == nil {
			per[cc] = &diff{Version: a.version, Changed: []Row{}}
		}
		return per[cc]
	}
	for _, r := range changed {
		d := get(r.CC)
		d.Changed = append(d.Changed, r)
	}
	for _, k := range removed {
		cc, _, _ := strings.Cut(k, "|")
		d := get(cc)
		d.Removed = append(d.Removed, k)
	}
	frames := make(map[string][]byte, len(per))
	for cc, d := range per {
		frames[cc], _ = json.Marshal(d)
	}
	tick, _ := json.Marshal(diff{Version: a.version, Changed: []Row{}})
	a.hub.Broadcast("diff", frames, tick)

	if a.OnChange != nil && (len(changed) > 0 || len(removed) > 0) {
		out := append([]Row(nil), changed...)
		for _, k := range removed {
			p := strings.SplitN(k, "|", 4)
			if len(p) == 4 {
				out = append(out, Row{Key: k, CC: p[0], City: p[1], Visa: p[2], Category: p[3]})
			}
		}
		a.OnChange(out)
	}
}

func (a *Agent) statusList() []SourceStatus {
	out := make([]SourceStatus, 0, len(a.sources))
	for _, s := range a.sources {
		out = append(out, *a.status[s.Name()])
	}
	return out
}
