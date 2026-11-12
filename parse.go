package main

import (
	"strconv"
	"strings"
	"time"
)

// Helpers that turn what people type ("f1", "delhi", "3 dec") into the server's names.

// parseVisa maps loose input to one of VisaTypes. "" means not recognised.
func parseVisa(s string) string {
	k := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, strings.ToLower(s))
	switch k {
	case "b1b2", "b1", "b2", "b", "visitor", "tourist", "business":
		return "B1/B2"
	case "f1m1", "f1", "m1", "f", "student":
		return "F1/M1"
	case "j1", "j", "exchange":
		return "J1"
	case "h1b", "h1", "h":
		return "H1B"
	case "l1", "l":
		return "L1"
	case "o1", "o":
		return "O1"
	case "c1d", "c1", "d", "crew", "transit":
		return "C1/D"
	}
	return ""
}

// findCountry matches a country code ("IN") or name ("india", "united king").
func findCountry(s string) (cc, name string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	if n := countryName(strings.ToUpper(s)); n != "" && len(s) == 2 {
		return strings.ToUpper(s), n
	}
	q := normCity(s)
	aliases := map[string]string{"uk": "GB", "usa": "", "uae": "AE", "britain": "GB", "england": "GB", "korea": "KR", "south korea": "KR"}
	if c, ok := aliases[q]; ok && c != "" {
		return c, countryName(c)
	}
	var best, bestName string
	for code, ps := range postsByCC {
		n := normCity(ps[0].Country)
		if n == q {
			return code, ps[0].Country
		}
		if strings.HasPrefix(n, q) && (best == "" || len(n) < len(normCity(bestName))) {
			best, bestName = code, ps[0].Country
		}
	}
	return best, bestName
}

// findPost finds a post by city name in any country. It returns several matches
// when the name is ambiguous.
func findPost(city, cc string) []Post {
	if cc != "" {
		if c := matchCity(cc, city); c != "" {
			return []Post{{CC: cc, Country: countryName(cc), City: c}}
		}
		return nil
	}
	var out []Post
	for code := range postsByCC {
		if c := matchCity(code, city); c != "" {
			out = append(out, Post{CC: code, Country: countryName(code), City: c})
		}
	}
	return out
}

var monthNames = map[string]time.Month{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7,
	"aug": 8, "sep": 9, "sept": 9, "oct": 10, "nov": 11, "dec": 12}

// parseDate reads "2026-12-03", "3 Dec", "Dec 3", "3 Dec 2026", "03/12/2026" (day first)
// or "03/12". Without a year it picks the next such date from today.
func parseDate(s string, today time.Time) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(s, ",", " ")))
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format("2006-01-02"), true
	}
	var day, year int
	var mon time.Month
	if strings.ContainsAny(s, "/.-") {
		p := strings.FieldsFunc(s, func(r rune) bool { return r == '/' || r == '.' || r == '-' })
		if len(p) < 2 || len(p) > 3 {
			return "", false
		}
		d, e1 := strconv.Atoi(p[0])
		m, e2 := strconv.Atoi(p[1])
		if e1 != nil || e2 != nil {
			return "", false
		}
		day, mon = d, time.Month(m)
		if len(p) == 3 {
			year, _ = strconv.Atoi(p[2])
		}
	} else {
		for _, f := range strings.Fields(s) {
			f = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(f, "st"), "nd"), "rd"), "th")
			if n, err := strconv.Atoi(f); err == nil {
				if n > 31 {
					year = n
				} else {
					day = n
				}
				continue
			}
			if len(f) >= 3 {
				if m, ok := monthNames[f[:min(len(f), 4)]]; ok {
					mon = m
				} else if m, ok := monthNames[f[:3]]; ok {
					mon = m
				}
			}
		}
	}
	if day < 1 || day > 31 || mon < 1 || mon > 12 {
		return "", false
	}
	if year > 0 && year < 100 {
		year += 2000
	}
	y := year
	if y == 0 {
		y = today.Year()
	}
	t := time.Date(y, mon, day, 0, 0, 0, 0, time.UTC)
	if t.Day() != day {
		return "", false
	}
	if year == 0 && t.Before(today.Truncate(24*time.Hour)) {
		t = t.AddDate(1, 0, 0)
	}
	return t.Format("2006-01-02"), true
}

// prettyDate turns 2026-12-03 into "Thu 3 Dec" (and adds the year when it isn't this year).
func prettyDate(d string) string {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return d
	}
	if t.Year() != time.Now().Year() {
		return t.Format("Mon 2 Jan 2006")
	}
	return t.Format("Mon 2 Jan")
}

func daysUntil(d string) int {
	t, err := time.Parse("2006-01-02", d)
	if err != nil {
		return 0
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return int(t.Sub(today).Hours() / 24)
}

func agoText(ms int64) string {
	s := int(time.Since(time.UnixMilli(ms)).Seconds())
	switch {
	case s < 60:
		return "just now"
	case s < 3600:
		return strconv.Itoa(s/60) + " min ago"
	default:
		return strconv.Itoa(s/3600) + " h ago"
	}
}
