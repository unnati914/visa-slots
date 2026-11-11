package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ReportsSource holds slot data sent in by the browser agent (extension). Each agent
// runs inside a real user's logged-in visa account, so these are actual slots.
// It plugs into the Agent like any other Source.
type ReportsSource struct {
	ttl  time.Duration
	mu   sync.Mutex
	rows map[string]Row

	limMu sync.Mutex
	lim   map[string]*bucket
	kick  chan struct{}
}

func (s *ReportsSource) Kick() <-chan struct{} { return s.kick }

type bucket struct {
	tokens float64
	last   time.Time
}

func NewReportsSource(ttl time.Duration) *ReportsSource {
	return &ReportsSource{ttl: ttl, rows: map[string]Row{}, lim: map[string]*bucket{}, kick: make(chan struct{}, 1)}
}

func (s *ReportsSource) Name() string            { return "browser-agent" }
func (s *ReportsSource) Interval() time.Duration { return 10 * time.Second } // also kicked on every report

// Fetch returns rows reported recently. Rows nobody has re-checked within the TTL
// are dropped, because an old "slot available" is worse than no data.
func (s *ReportsSource) Fetch(ctx context.Context) ([]Row, error) {
	cutoff := time.Now().Add(-s.ttl).UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Row, 0, len(s.rows))
	for k, r := range s.rows {
		if r.CheckedAt < cutoff {
			delete(s.rows, k)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Report is what the extension POSTs after each check.
type Report struct {
	Agent     string `json:"agent"`
	System    string `json:"system"` // "ais" or "cgi"
	CC        string `json:"cc"`
	Visa      string `json:"visa"`
	Category  string `json:"cat"`
	CheckedAt int64  `json:"checkedAt"`
	Posts     []struct {
		City  string   `json:"city"`
		Dates []string `json:"dates"`
	} `json:"posts"`
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

type reportResult struct {
	Accepted []string `json:"accepted"`
	Rejected []string `json:"rejected,omitempty"`
}

func (s *ReportsSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if fwd := r.Header.Get("Fly-Client-IP"); fwd != "" {
		host = fwd
	}
	if !s.allow(host) {
		http.Error(w, `{"error":"too many reports, slow down"}`, http.StatusTooManyRequests)
		return
	}
	var rep Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&rep); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	rep.CC = strings.ToUpper(strings.TrimSpace(rep.CC))
	country := countryName(rep.CC)
	if country == "" || !contains(VisaTypes, rep.Visa) || !contains(Categories, rep.Category) {
		http.Error(w, `{"error":"need a known cc, visa and cat"}`, http.StatusBadRequest)
		return
	}

	now := time.Now()
	today := now.UTC().Format("2006-01-02")
	limit := now.AddDate(2, 0, 0).Format("2006-01-02")
	res := reportResult{Accepted: []string{}}
	s.mu.Lock()
	for _, p := range rep.Posts {
		city := matchCity(rep.CC, p.City)
		if city == "" {
			res.Rejected = append(res.Rejected, p.City)
			continue
		}
		seen := map[string]bool{}
		dates := []string{}
		for _, d := range p.Dates {
			if len(d) >= 10 {
				d = d[:10]
			}
			if dateRe.MatchString(d) && d >= today && d <= limit && !seen[d] {
				seen[d] = true
				dates = append(dates, d)
			}
		}
		sort.Strings(dates)
		row := Row{Key: rowKey(rep.CC, city, rep.Visa, rep.Category), CC: rep.CC, Country: country, City: city,
			Visa: rep.Visa, Category: rep.Category, Open: len(dates), CheckedAt: now.UnixMilli()}
		if len(dates) > 0 {
			row.Earliest = dates[0]
			row.Dates = dates[:min(len(dates), 5)]
		}
		s.rows[row.Key] = row
		res.Accepted = append(res.Accepted, city)
	}
	s.mu.Unlock()
	select { // publish now
	case s.kick <- struct{}{}:
	default:
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// allow is a per-IP token bucket: 20 reports burst, refilling at 1 every 6 s.
func (s *ReportsSource) allow(ip string) bool {
	s.limMu.Lock()
	defer s.limMu.Unlock()
	now := time.Now()
	b := s.lim[ip]
	if b == nil {
		b = &bucket{tokens: 20, last: now}
		s.lim[ip] = b
	}
	b.tokens = min(20, b.tokens+now.Sub(b.last).Seconds()/6)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func cors(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

var postsByCC = func() map[string][]Post {
	m := map[string][]Post{}
	for _, p := range AllPosts() {
		m[p.CC] = append(m[p.CC], p)
	}
	return m
}()

func countryName(cc string) string {
	if ps := postsByCC[cc]; len(ps) > 0 {
		return ps[0].Country
	}
	return ""
}

// Booking sites label posts differently ("NEW DELHI VAC", "Bogotá", "Mexico City
// Consular Section"), so normalise and match against the known post list.
var cityAliases = map[string]string{"delhi": "new delhi", "bombay": "mumbai", "madras": "chennai", "calcutta": "kolkata",
	"ho chi minh": "ho chi minh city", "saigon": "ho chi minh city", "juarez": "ciudad juarez", "bogota dc": "bogota"}

func normCity(s string) string {
	s = norm.NFD.String(strings.ToLower(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r):
		case unicode.IsLetter(r) || r == ' ':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	words := []string{}
	for _, w := range strings.Fields(b.String()) {
		switch w {
		case "vac", "ofc", "consular", "section", "consulate", "embassy", "us", "u", "s", "general", "the", "of", "appointment", "interview":
		default:
			words = append(words, w)
		}
	}
	return strings.Join(words, " ")
}

func matchCity(cc, label string) string {
	n := normCity(label)
	if a, ok := cityAliases[n]; ok {
		n = a
	}
	if n == "" {
		return ""
	}
	var best string
	for _, p := range postsByCC[cc] {
		c := normCity(p.City)
		if c == n {
			return p.City
		}
		if strings.Contains(n, c) || strings.Contains(c, n) {
			if len(c) > len(normCity(best)) {
				best = p.City
			}
		}
	}
	return best
}
