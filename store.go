package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync/atomic"
	"time"
)

// Row is the availability for one (post, visa type, category) as last seen by a source.
type Row struct {
	Key       string   `json:"key"`
	CC        string   `json:"cc"`
	Country   string   `json:"country"`
	City      string   `json:"city"`
	Visa      string   `json:"visa"`
	Category  string   `json:"cat"`
	Earliest  string   `json:"earliest,omitempty"` // YYYY-MM-DD; empty means no open slots
	Dates     []string `json:"dates,omitempty"`    // next few open dates
	Open      int      `json:"open"`               // number of open dates seen
	Source    string   `json:"source"`
	Via       string   `json:"via,omitempty"`     // extension (read from the official site), telegram or app (typed by a person)
	Reports   int      `json:"reports,omitempty"` // distinct people who reported this row recently
	Confirmed bool     `json:"ok,omitempty"`      // read by the extension, or matched by 2+ people
	CheckedAt int64    `json:"checked"`           // unix ms of the fetch that produced this row
	ChangedAt int64    `json:"changed"`           // unix ms when availability last changed
}

func rowKey(cc, city, visa, cat string) string { return cc + "|" + city + "|" + visa + "|" + cat }

// sameAvailability ignores timestamps and source, so re-checks with no change are not broadcast.
func sameAvailability(a, b Row) bool {
	if a.Earliest != b.Earliest || a.Open != b.Open || len(a.Dates) != len(b.Dates) ||
		a.Confirmed != b.Confirmed || a.Reports != b.Reports {
		return false
	}
	for i := range a.Dates {
		if a.Dates[i] != b.Dates[i] {
			return false
		}
	}
	return true
}

// payload is a response body encoded once, at snapshot build time, so requests only copy bytes.
type payload struct {
	raw, gz []byte
	etag    string
}

func newPayload(v any) *payload {
	raw, _ := json.Marshal(v)
	var buf bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	zw.Write(raw)
	zw.Close()
	sum := sha1.Sum(raw)
	return &payload{raw: raw, gz: buf.Bytes(), etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

type CountryInfo struct {
	CC       string `json:"cc"`
	Country  string `json:"country"`
	Posts    int    `json:"posts"`
	OpenRows int    `json:"openRows"` // (post, visa, category) combos with at least one slot
	Earliest string `json:"earliest,omitempty"`
}

// Snapshot is immutable once published. Readers load it with one atomic pointer read.
type Snapshot struct {
	Version   uint64
	UpdatedAt int64
	all       *payload
	countries *payload
	byCountry map[string]*payload
	earliest  map[string]*payload // visa type, or "" for all types
	status    *payload
}

type Store struct{ cur atomic.Pointer[Snapshot] }

func (s *Store) Load() *Snapshot { return s.cur.Load() }

const earliestLimit = 100

// Build encodes every response variant up front. This runs in the agent, never on the request path.
func buildSnapshot(version uint64, rows []Row, status []SourceStatus) *Snapshot {
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	now := time.Now().UnixMilli()

	type envelope struct {
		Version   uint64 `json:"version"`
		UpdatedAt int64  `json:"updatedAt"`
		Rows      []Row  `json:"rows"`
	}
	snap := &Snapshot{Version: version, UpdatedAt: now,
		byCountry: map[string]*payload{}, earliest: map[string]*payload{}}

	snap.all = newPayload(envelope{version, now, rows})

	// Per country, plus country index.
	groups := map[string][]Row{}
	info := map[string]*CountryInfo{}
	posts := map[string]map[string]bool{}
	for _, r := range rows {
		groups[r.CC] = append(groups[r.CC], r)
		ci := info[r.CC]
		if ci == nil {
			ci = &CountryInfo{CC: r.CC, Country: r.Country}
			info[r.CC] = ci
			posts[r.CC] = map[string]bool{}
		}
		posts[r.CC][r.City] = true
		if r.Open > 0 {
			ci.OpenRows++
			if ci.Earliest == "" || r.Earliest < ci.Earliest {
				ci.Earliest = r.Earliest
			}
		}
	}
	list := make([]CountryInfo, 0, len(info))
	for cc, g := range groups {
		snap.byCountry[cc] = newPayload(envelope{version, now, g})
		info[cc].Posts = len(posts[cc])
		list = append(list, *info[cc])
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Country < list[j].Country })
	snap.countries = newPayload(struct {
		Version   uint64        `json:"version"`
		UpdatedAt int64         `json:"updatedAt"`
		Countries []CountryInfo `json:"countries"`
	}{version, now, list})

	// Earliest open slots worldwide, overall and per visa type.
	open := make([]Row, 0, len(rows))
	for _, r := range rows {
		if r.Open > 0 {
			open = append(open, r)
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return open[i].Earliest < open[j].Earliest })
	pick := func(visa string) []Row {
		out := []Row{}
		for _, r := range open {
			if visa == "" || r.Visa == visa {
				out = append(out, r)
				if len(out) == earliestLimit {
					break
				}
			}
		}
		return out
	}
	snap.earliest[""] = newPayload(envelope{version, now, pick("")})
	for _, v := range VisaTypes {
		snap.earliest[v] = newPayload(envelope{version, now, pick(v)})
	}

	snap.status = newPayload(struct {
		Version uint64         `json:"version"`
		Sources []SourceStatus `json:"sources"`
	}{version, status})
	return snap
}
