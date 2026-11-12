package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func future(days int) string { return time.Now().UTC().AddDate(0, 0, days).Format("2006-01-02") }

func rep(cc, visa, city string, dates ...string) Report {
	r := Report{CC: cc, Visa: visa, Category: "Regular"}
	r.Posts = append(r.Posts, struct {
		City  string   `json:"city"`
		Dates []string `json:"dates"`
	}{city, dates})
	return r
}

func TestTypedReportsNeedTwoPeople(t *testing.T) {
	s := NewReportsSource(time.Hour)
	d := future(20)
	res, err := s.Ingest(rep("IN", "F1/M1", "Mumbai", d), "tg:1", "telegram")
	if err != nil || res.Confirmed || res.Reports != 1 {
		t.Fatalf("first typed report: %+v %v, want unconfirmed", res, err)
	}
	// Same person again does not confirm it.
	res, _ = s.Ingest(rep("IN", "F1/M1", "Mumbai", d), "tg:1", "telegram")
	if res.Confirmed {
		t.Fatal("one person reporting twice must not confirm")
	}
	res, _ = s.Ingest(rep("IN", "F1/M1", "Mumbai", d), "app:1.2.3.4", "app")
	if !res.Confirmed || res.Reports != 2 {
		t.Fatalf("second person: %+v, want confirmed with 2 reports", res)
	}
}

func TestExtensionReportIsTrusted(t *testing.T) {
	s := NewReportsSource(time.Hour)
	res, _ := s.Ingest(rep("MX", "B1/B2", "Ciudad Juárez", future(9)), "extension:1.1.1.1", "extension")
	if !res.Confirmed {
		t.Fatal("extension reads the official site, should be confirmed alone")
	}
	rows, _ := s.Fetch(context.Background())
	if len(rows) != 1 || rows[0].City != "Ciudad Juarez" || rows[0].Via != "extension" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestBadReports(t *testing.T) {
	s := NewReportsSource(time.Hour)
	if _, err := s.Ingest(rep("ZZ", "F1/M1", "Mumbai", future(3)), "x", "app"); err == nil {
		t.Fatal("unknown country accepted")
	}
	res, _ := s.Ingest(rep("IN", "F1/M1", "Paris", future(3)), "x", "app")
	if len(res.Rejected) != 1 {
		t.Fatal("unknown city accepted")
	}
	res, _ = s.Ingest(rep("IN", "F1/M1", "Mumbai", "2001-01-01", "garbage"), "x", "app")
	rows, _ := s.Fetch(context.Background())
	if len(rows) != 1 || rows[0].Open != 0 {
		t.Fatalf("past/garbage dates should be dropped, got %+v", rows)
	}
}

type fakeNotifier struct {
	mu   sync.Mutex
	got  map[string][]Row
	hits chan string
}

func (f *fakeNotifier) Notify(owner string, w *Watch, rows []Row) {
	f.mu.Lock()
	f.got[owner] = append(f.got[owner], rows...)
	f.mu.Unlock()
	f.hits <- owner
}

func TestAlertMatching(t *testing.T) {
	a := NewAlerts(OpenKV(t.TempDir()))
	n := &fakeNotifier{got: map[string][]Row{}, hits: make(chan string, 10)}
	a.Register("tg", n)
	a.Add(Watch{Owner: "tg:1", CC: "IN", Visa: "F1/M1", City: "Mumbai"})
	a.Add(Watch{Owner: "tg:2", CC: "IN", Before: future(10)})
	a.Add(Watch{Owner: "tg:3", CC: "MX"})

	row := Row{Key: rowKey("IN", "Mumbai", "F1/M1", "Regular"), CC: "IN", City: "Mumbai", Visa: "F1/M1", Open: 1, Earliest: future(20), Confirmed: true}
	a.match([]Row{row})
	if len(n.got["tg:1"]) != 1 || len(n.got["tg:2"]) != 0 || len(n.got["tg:3"]) != 0 {
		t.Fatalf("got %v", n.got)
	}
	// Same date again: no repeat. Earlier date: alert.
	a.match([]Row{row})
	row.Earliest = future(5)
	a.match([]Row{row})
	if len(n.got["tg:1"]) != 2 || len(n.got["tg:2"]) != 1 {
		t.Fatalf("after earlier date: %v", n.got)
	}
	// Unconfirmed rows never alert.
	u := row
	u.Key, u.City, u.Confirmed = rowKey("IN", "Chennai", "F1/M1", "Regular"), "Chennai", false
	a.match([]Row{u})
	if len(n.got["tg:2"]) != 1 {
		t.Fatal("unconfirmed row alerted")
	}
	// Slot gone, then back: alert again.
	gone := row
	gone.Open, gone.Earliest = 0, ""
	a.match([]Row{gone})
	a.match([]Row{row})
	if len(n.got["tg:1"]) != 3 {
		t.Fatalf("reopened slot should alert again, got %d", len(n.got["tg:1"]))
	}
	// Watches survive a restart.
	a.save()
	b := NewAlerts(&FileKV{dir: a.kv.(*FileKV).dir, cache: map[string]map[string]json.RawMessage{}})
	if b.Count() != 3 {
		t.Fatalf("reloaded %d watches", b.Count())
	}
}

func TestParsing(t *testing.T) {
	today := time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)
	for in, want := range map[string]string{"2026-12-03": "2026-12-03", "3 Dec": "2026-12-03", "Dec 3": "2026-12-03",
		"03/12/2026": "2026-12-03", "5/1": "2027-01-05", "1st march": "2027-03-01", "3 dec 2027": "2027-12-03", "31/02": ""} {
		got, ok := parseDate(in, today)
		if (want == "" && ok) || got != want {
			t.Errorf("parseDate(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"f1": "F1/M1", "B1/B2": "F1/M1"[:0] + "B1/B2", "h1b": "H1B", "student": "F1/M1", "mumbai": ""} {
		if got := parseVisa(in); got != want {
			t.Errorf("parseVisa(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{"india": "IN", "IN": "IN", "uk": "GB", "united kingdom": "GB", "philip": "PH", "narnia": ""} {
		if got, _ := findCountry(in); got != want {
			t.Errorf("findCountry(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeTelegram records what the bot sends and lets the test inject updates.
type fakeTelegram struct {
	mu      sync.Mutex
	sent    []map[string]any
	updates chan tgUpdate
	srv     *httptest.Server
}

func newFakeTelegram() *fakeTelegram {
	f := &fakeTelegram{updates: make(chan tgUpdate, 10)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(body, &m)
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"username": "test_slots_bot"}
		case "getUpdates":
			select {
			case u := <-f.updates:
				result = []tgUpdate{u}
			case <-time.After(200 * time.Millisecond):
				result = []tgUpdate{}
			}
		case "sendMessage", "editMessageText":
			f.mu.Lock()
			f.sent = append(f.sent, m)
			f.mu.Unlock()
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	return f
}

func (f *fakeTelegram) say(id int64, chat int64, from int64, text string) {
	var u tgUpdate
	raw, _ := json.Marshal(map[string]any{"update_id": id, "message": map[string]any{"message_id": id,
		"chat": map[string]any{"id": chat, "type": "private"}, "from": map[string]any{"id": from}, "text": text}})
	json.Unmarshal(raw, &u)
	f.updates <- u
}

func (f *fakeTelegram) waitFor(t *testing.T, chat int64, substr string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		for _, m := range f.sent {
			if int64(m["chat_id"].(float64)) == chat && strings.Contains(m["text"].(string), substr) {
				f.mu.Unlock()
				return m["text"].(string)
			}
		}
		f.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	t.Fatalf("chat %d never got %q; sent: %v", chat, substr, f.sent)
	return ""
}

// Full loop: one person sets an alert, two others report a slot, the first gets alerted.
func TestTelegramEndToEnd(t *testing.T) {
	ft := newFakeTelegram()
	defer ft.srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reports := NewReportsSource(time.Hour)
	store, hub := &Store{}, NewHub()
	agent := NewAgent(store, hub, reports)
	alerts := NewAlerts(OpenKV(t.TempDir()))
	agent.OnChange = alerts.OnChange
	go alerts.Run(ctx.Done())
	bot := NewBot("TEST", alerts, reports, store, "https://example.test")
	bot.api = ft.srv.URL + "/botTEST/"
	if err := bot.Run(ctx); err != nil || bot.Username != "test_slots_bot" {
		t.Fatalf("run: %v %q", err, bot.Username)
	}
	go agent.Run(ctx)

	ft.say(1, 100, 100, "/watch India F1 Mumbai")
	ft.waitFor(t, 100, "Alert on.")

	ft.say(2, 200, 200, "/report Mumbai F1 3 Dec")
	ft.waitFor(t, 200, "one more person")
	ft.say(3, 300, 300, "/report mumbai student 3 dec")
	ft.waitFor(t, 300, "confirmed")

	alert := ft.waitFor(t, 100, "Slot open")
	if !strings.Contains(alert, "Mumbai") || !strings.Contains(alert, "Dec") {
		t.Fatalf("alert text: %s", alert)
	}
	ft.say(4, 100, 100, "/slots india")
	ft.waitFor(t, 100, "✅ <b>Mumbai</b>")
	ft.say(5, 400, 400, "/start w_IN_any_New_Delhi")
	ft.waitFor(t, 400, "New Delhi, India")
	ft.say(6, 100, 100, "/stop")
	ft.waitFor(t, 100, "1 alert(s) turned off")
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 49.36.1.2")
	if got := clientIP(r); got != "49.36.1.2" {
		t.Errorf("behind local proxy: %s", got)
	}
	r.RemoteAddr = "8.8.8.8:5000" // direct hit: headers can be forged, ignore them
	r.Header.Set("Fly-Client-IP", "1.1.1.1")
	if got := clientIP(r); got != "8.8.8.8" {
		t.Errorf("direct: %s", got)
	}
}
