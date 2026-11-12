package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// ReportsSource holds slot data sent in by people. It has three channels:
//
//   - extension: the browser extension reads the calendar inside a user's own logged-in
//     visa account. That is the official site's data, so one report is enough.
//   - telegram and app: a person types what they saw. One person can be wrong or
//     spamming, so the row shows as unconfirmed until a second person agrees.
//
// It plugs into the Agent like any other Source.
type ReportsSource struct {
	ttl  time.Duration
	mu   sync.Mutex
	rows map[string]Row
	hist map[string][]entry // row key -> latest report from each reporter

	limMu sync.Mutex
	lim   map[string]*bucket
	kick  chan struct{}
}

type entry struct {
	reporter string
	auto     bool // read from the site by the extension
	open     bool
	at       int64
}

// ConfirmAt is how many different people must report open slots for a typed report to count as confirmed.
const ConfirmAt = 2

func (s *ReportsSource) Kick() <-chan struct{} { return s.kick }

type bucket struct {
	tokens float64
	last   time.Time
}

func NewReportsSource(ttl time.Duration) *ReportsSource {
	return &ReportsSource{ttl: ttl, rows: map[string]Row{}, hist: map[string][]entry{},
		lim: map[string]*bucket{}, kick: make(chan struct{}, 1)}
}

func (s *ReportsSource) Name() string            { return "reports" }
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
			delete(s.hist, k)
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Report is one check of the booking site, from any channel.
type Report struct {
	Agent     string `json:"agent"`
	Via       string `json:"via"`    // "extension" (default) or "app"
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
	Accepted  []string `json:"accepted"`
	Rejected  []string `json:"rejected,omitempty"`
	Confirmed bool     `json:"confirmed"` // the (last) row now counts as confirmed
	Reports   int      `json:"reports"`   // people behind the (last) row
}

var errBadReport = errors.New("need a known country, visa type and category")

// Ingest stores a report. reporter identifies one person (Telegram user, IP address)
// so the same person reporting twice is counted once.
func (s *ReportsSource) Ingest(rep Report, reporter, via string) (reportResult, error) {
	rep.CC = strings.ToUpper(strings.TrimSpace(rep.CC))
	if rep.Category == "" {
		rep.Category = "Regular"
	}
	country := countryName(rep.CC)
	if country == "" || !contains(VisaTypes, rep.Visa) || !contains(Categories, rep.Category) {
		return reportResult{}, errBadReport
	}
	auto := via == "extension"
	now := time.Now()
	nowMs := now.UnixMilli()
	cutoff := now.Add(-s.ttl).UnixMilli()
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
		key := rowKey(rep.CC, city, rep.Visa, rep.Category)

		// Keep one entry per reporter, newest wins, and forget anything past the TTL.
		h := []entry{{reporter: reporter, auto: auto, open: len(dates) > 0, at: nowMs}}
		for _, e := range s.hist[key] {
			if e.reporter != reporter && e.at >= cutoff {
				h = append(h, e)
			}
		}
		s.hist[key] = h
		openBy := 0
		for _, e := range h {
			if e.open {
				openBy++
			}
		}

		row := Row{Key: key, CC: rep.CC, Country: country, City: city, Visa: rep.Visa, Category: rep.Category,
			Open: len(dates), CheckedAt: nowMs, Via: via, Reports: len(h)}
		if len(dates) > 0 {
			row.Earliest = dates[0]
			row.Dates = dates[:min(len(dates), 5)]
			row.Confirmed = auto || openBy >= ConfirmAt
		} else {
			row.Confirmed = true // "nothing open" is the safe answer, no need to wait for a second person
		}
		s.rows[key] = row
		res.Accepted = append(res.Accepted, city)
		res.Confirmed, res.Reports = row.Confirmed, len(h)
	}
	s.mu.Unlock()
	select { // publish now
	case s.kick <- struct{}{}:
	default:
	}
	return res, nil
}

// clientIP is the real visitor's address. Proxy headers are trusted only when they
// can't be forged: on Fly (its edge sets Fly-Client-IP) or when the request came
// through a proxy on this same machine (Caddy sets X-Forwarded-For).
func clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if onFly {
		if fwd := r.Header.Get("Fly-Client-IP"); fwd != "" {
			return fwd
		}
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1]) // the hop our proxy added
		}
	}
	return host
}

var onFly = os.Getenv("FLY_APP_NAME") != ""

func (s *ReportsSource) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cors(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ip := clientIP(r)
	if !s.allow(ip) {
		http.Error(w, `{"error":"too many reports, slow down"}`, http.StatusTooManyRequests)
		return
	}
	var rep Report
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&rep); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	via := "extension"
	if rep.Via == "app" {
		via = "app"
	}
	// One IP is one person. A typed report from the app can't confirm itself by
	// being sent from many browser profiles on the same connection.
	res, err := s.Ingest(rep, via+":"+ip, via)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// allow is a per-key token bucket: 20 reports burst, refilling at 1 every 6 s.
func (s *ReportsSource) allow(key string) bool {
	s.limMu.Lock()
	defer s.limMu.Unlock()
	now := time.Now()
	b := s.lim[key]
	if b == nil {
		if len(s.lim) > 100_000 { // don't grow forever
			clear(s.lim)
		}
		b = &bucket{tokens: 20, last: now}
		s.lim[key] = b
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
