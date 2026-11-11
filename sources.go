package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// MockSource simulates a booking website: slots open and get taken over time,
// fetches take 50-300 ms, and about 3% of fetches fail. Replace it with real
// adapters once you have a data source you are allowed to use.
// ---------------------------------------------------------------------------

type mockState struct {
	earliestDay int // days from today; -1 = no slots
	open        int
}

type MockSource struct {
	name     string
	interval time.Duration
	posts    []Post
	rng      *rand.Rand
	mu       sync.Mutex
	state    map[string]*mockState
}

func NewMockSource(name string, interval time.Duration, posts []Post, seed uint64) *MockSource {
	m := &MockSource{name: name, interval: interval, posts: posts,
		rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b9)), state: map[string]*mockState{}}
	for _, p := range posts {
		for _, v := range VisaTypes {
			for _, c := range Categories {
				st := &mockState{earliestDay: -1}
				if m.rng.Float64() < 0.55 {
					st.earliestDay = 3 + m.rng.IntN(420)
					st.open = 1 + m.rng.IntN(12)
				}
				m.state[rowKey(p.CC, p.City, v, c)] = st
			}
		}
	}
	return m
}

func (m *MockSource) Name() string            { return m.name }
func (m *MockSource) Interval() time.Duration { return m.interval }

func (m *MockSource) Fetch(ctx context.Context) ([]Row, error) {
	m.mu.Lock()
	delay := time.Duration(50+m.rng.IntN(250)) * time.Millisecond
	fail := m.rng.Float64() < 0.03
	m.mu.Unlock()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if fail {
		return nil, errors.New("simulated: upstream returned 503")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	rows := make([]Row, 0, len(m.state))
	for _, p := range m.posts {
		churn := 0.01
		if hotCountries[p.CC] {
			churn = 0.04
		}
		for _, v := range VisaTypes {
			for _, c := range Categories {
				k := rowKey(p.CC, p.City, v, c)
				st := m.state[k]
				if m.rng.Float64() < churn {
					switch {
					case st.earliestDay < 0 || m.rng.Float64() < 0.5: // a slot opens up
						d := 2 + m.rng.IntN(120)
						if st.earliestDay < 0 || d < st.earliestDay {
							st.earliestDay = d
						}
						st.open++
					default: // someone books it
						st.open--
						if st.open <= 0 {
							st.open, st.earliestDay = 0, -1
						} else {
							st.earliestDay += 1 + m.rng.IntN(30)
						}
					}
				}
				r := Row{Key: k, CC: p.CC, Country: p.Country, City: p.City, Visa: v, Category: c,
					Open: st.open, CheckedAt: now.UnixMilli()}
				if st.earliestDay >= 0 && st.open > 0 {
					for i := 0; i < min(st.open, 5); i++ {
						r.Dates = append(r.Dates, today.AddDate(0, 0, st.earliestDay+i*(1+i%3)).Format("2006-01-02"))
					}
					r.Earliest = r.Dates[0]
				}
				rows = append(rows, r)
			}
		}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// HTTPJSONSource reads rows from any URL that returns {"rows":[...]} in the Row
// format. Use it to plug in a separate scraper, a partner feed, or a crowdsourced
// collector without changing this server.
// ---------------------------------------------------------------------------

type HTTPJSONSource struct {
	name, url string
	interval  time.Duration
	client    *http.Client
}

func NewHTTPJSONSource(name, url string, interval time.Duration) *HTTPJSONSource {
	return &HTTPJSONSource{name: name, url: url, interval: interval, client: &http.Client{Timeout: 20 * time.Second}}
}

func (h *HTTPJSONSource) Name() string            { return h.name }
func (h *HTTPJSONSource) Interval() time.Duration { return h.interval }

func (h *HTTPJSONSource) Fetch(ctx context.Context) ([]Row, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", h.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "visa-slots-agent/1.0")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s returned %s", h.url, resp.Status)
	}
	var body struct {
		Rows []Row `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	now := time.Now().UnixMilli()
	for i := range body.Rows {
		r := &body.Rows[i]
		r.Key = rowKey(r.CC, r.City, r.Visa, r.Category)
		sort.Strings(r.Dates)
		if len(r.Dates) > 0 {
			r.Earliest = r.Dates[0]
		}
		if r.CheckedAt == 0 {
			r.CheckedAt = now
		}
	}
	return body.Rows, nil
}
