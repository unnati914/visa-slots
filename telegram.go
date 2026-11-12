package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Bot is the Telegram side of the app. People use it to:
//   - get alerts:     /watch  (guided with buttons, or "/watch India F1 Mumbai")
//   - see slots:      /slots India
//   - report a slot:  /report Mumbai F1 3 Dec
//
// It long-polls Telegram, so it needs no public webhook URL and works the same on a laptop and on Fly.
type Bot struct {
	token    string
	api      string
	Username string
	http     *http.Client
	alerts   *Alerts
	reports  *ReportsSource
	store    *Store
	site     string // public URL of the web app, for links

	out   chan tgOut
	mu    sync.Mutex
	flows map[int64]*flow // guided /watch in progress, per chat
}

type flow struct {
	cc, visa string
	at       time.Time
}

type tgOut struct {
	chat  int64
	text  string
	kb    [][]tgButton
	edit  int // message id to edit instead of sending
	owner string
}

type tgButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data,omitempty"`
	URL  string `json:"url,omitempty"`
}

type tgUpdate struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		ID   int `json:"message_id"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From  *struct{ ID int64 } `json:"from"`
		Text  string              `json:"text"`
		Photo []json.RawMessage   `json:"photo"`
	} `json:"message"`
	Callback *struct {
		ID      string `json:"id"`
		From    struct{ ID int64 }
		Data    string `json:"data"`
		Message *struct {
			ID   int                `json:"message_id"`
			Chat struct{ ID int64 } `json:"chat"`
		} `json:"message"`
	} `json:"callback_query"`
}

func NewBot(token string, alerts *Alerts, reports *ReportsSource, store *Store, site string) *Bot {
	b := &Bot{token: token, api: "https://api.telegram.org/bot" + token + "/", http: &http.Client{Timeout: 70 * time.Second},
		alerts: alerts, reports: reports, store: store, site: strings.TrimRight(site, "/"),
		out: make(chan tgOut, 20000), flows: map[int64]*flow{}}
	alerts.Register("tg", b)
	return b
}

func (b *Bot) call(ctx context.Context, method string, body any, out any) error {
	j, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", b.api+method, bytes.NewReader(j))
	req.Header.Set("Content-Type", "application/json")
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(res.Body).Decode(&env); err != nil {
		return err
	}
	if !env.OK {
		return &tgError{env.ErrorCode, env.Description, env.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

type tgError struct {
	Code       int
	Desc       string
	RetryAfter int
}

func (e *tgError) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Desc) }

// Run starts the bot. It returns once the bot name is known (or the token failed).
func (b *Bot) Run(ctx context.Context) error {
	var me struct{ Username string }
	if err := b.call(ctx, "getMe", map[string]any{}, &me); err != nil {
		return err
	}
	b.Username = me.Username
	b.call(ctx, "setMyCommands", map[string]any{"commands": []map[string]string{
		{"command": "watch", "description": "Get an alert when a slot opens"},
		{"command": "slots", "description": "See the earliest slots in a country"},
		{"command": "report", "description": "Share a slot you saw"},
		{"command": "mine", "description": "Your alerts"},
		{"command": "stop", "description": "Turn off all alerts"},
	}}, nil)
	go b.sender(ctx)
	go b.poll(ctx)
	log.Printf("telegram: running as @%s", b.Username)
	return nil
}

func (b *Bot) poll(ctx context.Context) {
	var offset int64
	fails := 0
	for ctx.Err() == nil {
		var ups []tgUpdate
		err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50,
			"allowed_updates": []string{"message", "callback_query"}}, &ups)
		if err != nil {
			fails++
			log.Printf("telegram: getUpdates: %v", err)
			select {
			case <-time.After(time.Duration(min(fails, 30)) * time.Second):
			case <-ctx.Done():
			}
			continue
		}
		fails = 0
		for _, u := range ups {
			offset = u.ID + 1
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("telegram: handler panic: %v", r)
					}
				}()
				b.handle(u)
			}()
		}
	}
}

// sender is the only goroutine that sends. Telegram allows about 30 messages a second
// overall, so alerts to thousands of people are paced at 25/s and retried on 429.
func (b *Bot) sender(ctx context.Context) {
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		var m tgOut
		select {
		case <-ctx.Done():
			return
		case m = <-b.out:
		}
		<-tick.C
		body := map[string]any{"chat_id": m.chat, "text": m.text, "parse_mode": "HTML",
			"link_preview_options": map[string]bool{"is_disabled": true}}
		if m.kb != nil {
			body["reply_markup"] = map[string]any{"inline_keyboard": m.kb}
		}
		method := "sendMessage"
		if m.edit != 0 {
			method = "editMessageText"
			body["message_id"] = m.edit
		}
		for try := 0; try < 3; try++ {
			err := b.call(ctx, method, body, nil)
			if err == nil {
				break
			}
			te, ok := err.(*tgError)
			if ok && te.RetryAfter > 0 {
				time.Sleep(time.Duration(te.RetryAfter) * time.Second)
				continue
			}
			if ok && te.Code == 403 && m.owner != "" { // they blocked the bot
				b.alerts.RemoveOwner(m.owner)
			}
			if !(ok && te.Code == 400 && strings.Contains(te.Desc, "not modified")) {
				log.Printf("telegram: %s to %d: %v", method, m.chat, err)
			}
			break
		}
	}
}

func (b *Bot) send(chat int64, text string, kb [][]tgButton) {
	select {
	case b.out <- tgOut{chat: chat, text: text, kb: kb}:
	default:
		log.Printf("telegram: send queue full")
	}
}

func (b *Bot) edit(chat int64, msg int, text string, kb [][]tgButton) {
	select {
	case b.out <- tgOut{chat: chat, text: text, kb: kb, edit: msg}:
	default:
	}
}

// Notify sends an alert. Called by Alerts, must not block.
func (b *Bot) Notify(owner string, w *Watch, rows []Row) {
	chat, err := strconv.ParseInt(strings.TrimPrefix(owner, "tg:"), 10, 64)
	if err != nil {
		return
	}
	var sb strings.Builder
	sb.WriteString("🟢 <b>Slot open</b>\n")
	for i, r := range rows {
		if i == 5 {
			fmt.Fprintf(&sb, "\n…and %d more", len(rows)-5)
			break
		}
		fmt.Fprintf(&sb, "\n<b>%s</b> · %s%s\n%s  <i>(in %d days)</i>%s\n", esc(r.City), esc(r.Visa), catNote(r.Category),
			prettyDate(r.Earliest), daysUntil(r.Earliest), viaNote(r))
	}
	sb.WriteString("\nBook fast, slots go in minutes.")
	kb := [][]tgButton{{{Text: "Open booking site", URL: bookingURL(w.CC)}}}
	if b.site != "" {
		kb[0] = append(kb[0], tgButton{Text: "See all", URL: b.site + "/?country=" + w.CC})
	}
	select {
	case b.out <- tgOut{chat: chat, text: sb.String(), kb: kb, owner: owner}:
	default:
		log.Printf("telegram: send queue full, alert to %d dropped", chat)
	}
}

func esc(s string) string { return html.EscapeString(s) }

func catNote(c string) string {
	if c == "Dropbox" {
		return " · Dropbox"
	}
	return ""
}

func viaNote(r Row) string {
	if r.Via == "extension" {
		return "\n<i>read from the official site</i>"
	}
	return fmt.Sprintf("\n<i>%d people reported it</i>", r.Reports)
}

// bookingURL is where people book. Most countries use AIS; some use the CGI site.
func bookingURL(cc string) string {
	switch cc {
	case "IN", "CN", "NG", "PH", "VN", "BD", "PK", "JP", "KR", "TW", "HK", "TH", "ID", "MY", "SG", "NP", "LK", "ET", "KE", "GH", "ZA":
		return "https://www.usvisascheduling.com/"
	}
	return "https://ais.usvisa-info.com/"
}

const helpText = `<b>US visa slots, live.</b>
Slots shared by thousands of applicants, checked and sent to you the moment they open.

/watch – get an alert when a slot opens
/slots India – earliest slots right now
/report Mumbai F1 3 Dec – share a slot you saw
/mine – your alerts · /stop – turn them off

Each typed report shows as confirmed once a second person reports the same post.`

func (b *Bot) handle(u tgUpdate) {
	if c := u.Callback; c != nil {
		b.call(context.Background(), "answerCallbackQuery", map[string]any{"callback_query_id": c.ID}, nil)
		if c.Message != nil {
			b.onButton(c.Message.Chat.ID, c.Message.ID, c.From.ID, c.Data)
		}
		return
	}
	m := u.Message
	if m == nil || m.Chat.Type != "private" {
		return
	}
	chat := m.Chat.ID
	if len(m.Photo) > 0 {
		b.send(chat, "Thanks! I can't read screenshots yet. Type what you saw, like:\n<code>/report Mumbai F1 3 Dec</code>", nil)
		return
	}
	text := strings.TrimSpace(m.Text)
	cmd, args, _ := strings.Cut(text, " ")
	cmd = strings.ToLower(strings.SplitN(cmd, "@", 2)[0])
	args = strings.TrimSpace(args)
	switch cmd {
	case "/start":
		if strings.HasPrefix(args, "w_") {
			b.watchFromLink(chat, args)
			return
		}
		b.send(chat, helpText, [][]tgButton{{{Text: "🔔 Set up an alert", Data: "go"}}})
	case "/help":
		b.send(chat, helpText, nil)
	case "/watch", "/alert":
		if args == "" {
			b.askCountry(chat)
		} else {
			b.watchFromText(chat, args)
		}
	case "/slots", "/s":
		b.slots(chat, args)
	case "/report", "/seen", "/r":
		b.report(chat, m.From, args)
	case "/mine", "/alerts", "/list":
		b.mine(chat, 0)
	case "/stop":
		n := b.alerts.RemoveOwner(owner(chat))
		b.send(chat, fmt.Sprintf("Done. %d alert(s) turned off. Send /watch any time to start again.", n), nil)
	default:
		b.mu.Lock()
		f := b.flows[chat]
		b.mu.Unlock()
		if f != nil && f.cc == "" && !strings.HasPrefix(text, "/") {
			b.pickCountry(chat, text)
			return
		}
		// A bare country name works like /slots.
		if cc, _ := findCountry(text); cc != "" && !strings.HasPrefix(text, "/") {
			b.slots(chat, text)
			return
		}
		b.send(chat, "I didn't get that. Try /watch, /slots India or /report Mumbai F1 3 Dec.", nil)
	}
}

func owner(chat int64) string { return "tg:" + strconv.FormatInt(chat, 10) }

// ---------- guided /watch ----------

func (b *Bot) askCountry(chat int64) {
	b.mu.Lock()
	b.flows[chat] = &flow{at: time.Now()}
	if len(b.flows) > 50000 {
		for k, f := range b.flows {
			if time.Since(f.at) > time.Hour {
				delete(b.flows, k)
			}
		}
	}
	b.mu.Unlock()
	b.send(chat, "Which country will you interview in? Type it, e.g. <b>India</b>.", [][]tgButton{
		{{Text: "India", Data: "cc:IN"}, {Text: "Mexico", Data: "cc:MX"}, {Text: "Brazil", Data: "cc:BR"}},
		{{Text: "Nigeria", Data: "cc:NG"}, {Text: "Canada", Data: "cc:CA"}, {Text: "UK", Data: "cc:GB"}}})
}

func (b *Bot) pickCountry(chat int64, text string) {
	cc, _ := findCountry(text)
	if cc == "" {
		b.send(chat, "I don't know that country. Try its name in English, like <b>Philippines</b>.", nil)
		return
	}
	b.askVisa(chat, 0, cc)
}

func (b *Bot) askVisa(chat int64, msg int, cc string) {
	b.mu.Lock()
	b.flows[chat] = &flow{cc: cc, at: time.Now()}
	b.mu.Unlock()
	kb := [][]tgButton{}
	row := []tgButton{}
	for _, v := range VisaTypes {
		row = append(row, tgButton{Text: v, Data: "v:" + v})
		if len(row) == 4 {
			kb, row = append(kb, row), nil
		}
	}
	row = append(row, tgButton{Text: "Any", Data: "v:"})
	kb = append(kb, row)
	text := fmt.Sprintf("<b>%s</b>. Which visa?", esc(countryName(cc)))
	if msg != 0 {
		b.edit(chat, msg, text, kb)
	} else {
		b.send(chat, text, kb)
	}
}

func (b *Bot) askCity(chat int64, msg int, cc, visa string) {
	b.mu.Lock()
	if f := b.flows[chat]; f != nil {
		f.visa = visa
	}
	b.mu.Unlock()
	ps := postsByCC[cc]
	if len(ps) == 1 {
		b.finishWatch(chat, msg, Watch{CC: cc, Visa: visa, City: ps[0].City})
		return
	}
	kb := [][]tgButton{}
	row := []tgButton{}
	for i, p := range ps {
		row = append(row, tgButton{Text: p.City, Data: "c:" + strconv.Itoa(i)})
		if len(row) == 3 {
			kb, row = append(kb, row), nil
		}
	}
	row = append(row, tgButton{Text: "Any city", Data: "c:-1"})
	kb = append(kb, row)
	b.edit(chat, msg, fmt.Sprintf("<b>%s · %s</b>. Which city?", esc(countryName(cc)), esc(orAny(visa, "any visa"))), kb)
}

func orAny(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (b *Bot) onButton(chat int64, msg int, from int64, data string) {
	k, v, _ := strings.Cut(data, ":")
	b.mu.Lock()
	f := b.flows[chat]
	b.mu.Unlock()
	switch k {
	case "go":
		b.askCountry(chat)
	case "cc":
		if countryName(v) != "" {
			b.askVisa(chat, msg, v)
		}
	case "v":
		if f == nil || f.cc == "" {
			b.askCountry(chat)
			return
		}
		b.askCity(chat, msg, f.cc, v)
	case "c":
		if f == nil || f.cc == "" {
			b.askCountry(chat)
			return
		}
		w := Watch{CC: f.cc, Visa: f.visa}
		if i, err := strconv.Atoi(v); err == nil && i >= 0 && i < len(postsByCC[f.cc]) {
			w.City = postsByCC[f.cc][i].City
		}
		b.finishWatch(chat, msg, w)
	case "rm":
		b.alerts.Remove(owner(chat), v)
		b.mine(chat, msg)
	}
}

func (b *Bot) finishWatch(chat int64, msg int, w Watch) {
	b.mu.Lock()
	delete(b.flows, chat)
	b.mu.Unlock()
	w.Owner = owner(chat)
	saved, ok := b.alerts.Add(w)
	if !ok {
		b.send(chat, fmt.Sprintf("You already have %d alerts. Remove one with /mine first.", maxWatchesPerOwner), nil)
		return
	}
	where := esc(orAny(saved.City, "any city")) + ", " + esc(countryName(saved.CC))
	text := fmt.Sprintf("🔔 <b>Alert on.</b> %s · %s\nI'll message you the moment a slot opens.", where, esc(orAny(saved.Visa, "any visa")))
	if now := b.currentBest(saved); now != "" {
		text += "\n\nRight now: " + now
	}
	if msg != 0 {
		b.edit(chat, msg, text, nil)
	} else {
		b.send(chat, text, nil)
	}
}

// watchFromText handles "/watch India F1 Mumbai before 1 Mar".
func (b *Bot) watchFromText(chat int64, args string) {
	w := Watch{}
	low := strings.ToLower(args)
	if i := strings.Index(low, " before "); i >= 0 {
		if d, ok := parseDate(args[i+8:], time.Now().UTC()); ok {
			w.Before = d
		}
		args = args[:i]
	}
	var rest []string
	for _, f := range strings.Fields(args) {
		if v := parseVisa(f); v != "" && w.Visa == "" {
			w.Visa = v
			continue
		}
		rest = append(rest, f)
	}
	// Try the longest leading run of words as a country, the rest as a city.
	for n := len(rest); n >= 1 && w.CC == ""; n-- {
		if cc, _ := findCountry(strings.Join(rest[:n], " ")); cc != "" {
			w.CC = cc
			if n < len(rest) {
				if c := matchCity(cc, strings.Join(rest[n:], " ")); c != "" {
					w.City = c
				}
			}
		}
	}
	if w.CC == "" && len(rest) > 0 { // maybe they only gave a city
		if ps := findPost(strings.Join(rest, " "), ""); len(ps) == 1 {
			w.CC, w.City = ps[0].CC, ps[0].City
		}
	}
	if w.CC == "" {
		b.askCountry(chat)
		return
	}
	b.finishWatch(chat, 0, w)
}

// watchFromLink handles t.me/<bot>?start=w_IN_F1M1_Mumbai from the web app.
func (b *Bot) watchFromLink(chat int64, payload string) {
	p := strings.Split(strings.TrimPrefix(payload, "w_"), "_")
	w := Watch{CC: strings.ToUpper(p[0])}
	if countryName(w.CC) == "" {
		b.askCountry(chat)
		return
	}
	if len(p) > 1 {
		w.Visa = parseVisa(p[1])
	}
	if len(p) > 2 {
		w.City = matchCity(w.CC, strings.Join(p[2:], " "))
	}
	b.finishWatch(chat, 0, w)
}

func (b *Bot) mine(chat int64, msg int) {
	ws := b.alerts.List(owner(chat))
	if len(ws) == 0 {
		text := "You have no alerts. Send /watch to set one up."
		if msg != 0 {
			b.edit(chat, msg, text, nil)
		} else {
			b.send(chat, text, nil)
		}
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Your alerts</b>\n")
	kb := [][]tgButton{}
	for i, w := range ws {
		label := fmt.Sprintf("%s, %s · %s", orAny(w.City, "any city"), countryName(w.CC), orAny(w.Visa, "any visa"))
		if w.Before != "" {
			label += " · before " + prettyDate(w.Before)
		}
		fmt.Fprintf(&sb, "\n%d. %s", i+1, esc(label))
		kb = append(kb, []tgButton{{Text: fmt.Sprintf("Remove %d", i+1), Data: "rm:" + w.ID}})
	}
	if msg != 0 {
		b.edit(chat, msg, sb.String(), kb)
	} else {
		b.send(chat, sb.String(), kb)
	}
}

// ---------- /slots ----------

func (b *Bot) rows(cc string) []Row {
	s := b.store.Load()
	if s == nil {
		return nil
	}
	p := s.byCountry[cc]
	if p == nil {
		return nil
	}
	var env struct{ Rows []Row }
	json.Unmarshal(p.raw, &env)
	return env.Rows
}

func (b *Bot) currentBest(w *Watch) string {
	var best *Row
	for _, r := range b.rows(w.CC) {
		if w.matches(r) && r.Open > 0 && r.Confirmed && (best == nil || r.Earliest < best.Earliest) {
			r := r
			best = &r
		}
	}
	if best == nil {
		return "nothing open yet."
	}
	return fmt.Sprintf("earliest is <b>%s</b> in %s (%s).", prettyDate(best.Earliest), esc(best.City), esc(best.Visa))
}

func (b *Bot) slots(chat int64, args string) {
	if args == "" {
		b.send(chat, "Which country? e.g. <code>/slots India</code>", nil)
		return
	}
	var visa string
	var rest []string
	for _, f := range strings.Fields(args) {
		if v := parseVisa(f); v != "" && visa == "" {
			visa = v
		} else {
			rest = append(rest, f)
		}
	}
	cc, name := findCountry(strings.Join(rest, " "))
	if cc == "" {
		b.send(chat, "I don't know that country. Try its name in English.", nil)
		return
	}
	// Earliest per (city, visa), confirmed first.
	best := map[string]Row{}
	for _, r := range b.rows(cc) {
		if r.Open == 0 || (visa != "" && r.Visa != visa) {
			continue
		}
		k := r.City + "|" + r.Visa
		if cur, ok := best[k]; !ok || r.Earliest < cur.Earliest {
			best[k] = r
		}
	}
	list := make([]Row, 0, len(best))
	for _, r := range best {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Earliest < list[j].Earliest })
	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>%s · %s</b>\n", esc(name), esc(orAny(visa, "all visas")))
	if len(list) == 0 {
		sb.WriteString("\nNo open slots reported in the last 45 minutes.\nSet an alert and I'll tell you the moment one opens.")
	}
	for i, r := range list {
		if i == 12 {
			break
		}
		mark := "✅"
		if !r.Confirmed {
			mark = "❔"
		}
		fmt.Fprintf(&sb, "\n%s <b>%s</b> %s%s – %s · %s", mark, esc(r.City), esc(r.Visa), catNote(r.Category), prettyDate(r.Earliest), agoText(r.CheckedAt))
	}
	if len(list) > 0 {
		sb.WriteString("\n\n✅ confirmed  ❔ one report, not yet confirmed")
	}
	b.send(chat, sb.String(), [][]tgButton{{{Text: "🔔 Alert me", Data: "cc:" + cc}}})
}

// ---------- /report ----------

func (b *Bot) report(chat int64, from *struct{ ID int64 }, args string) {
	usage := "Tell me what you saw on the booking site:\n<code>/report Mumbai F1 3 Dec</code>\n<code>/report Lagos B1/B2 12/01 14/01</code>\n<code>/report Chennai H1B none</code>\nAdd <code>dropbox</code> for interview-waiver slots."
	if args == "" || from == nil {
		b.send(chat, usage, nil)
		return
	}
	if !b.reports.allow("tg:" + strconv.FormatInt(from.ID, 10)) {
		b.send(chat, "That's a lot of reports. Give it a minute.", nil)
		return
	}
	today := time.Now().UTC()
	var visa, cat string
	var dates, words []string
	none := false
	toks := strings.Fields(args)
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		lt := strings.ToLower(t)
		switch {
		case lt == "none" || lt == "no" || lt == "nothing" || lt == "full":
			none = true
		case lt == "dropbox" || lt == "waiver":
			cat = "Dropbox"
		case visa == "" && parseVisa(t) != "" && len(words) > 0:
			visa = parseVisa(t)
		default:
			if d, ok := parseDate(t, today); ok && strings.ContainsAny(t, "0123456789") && (strings.ContainsAny(t, "/-.")) {
				dates = append(dates, d)
				continue
			}
			// "3 Dec" or "Dec 3": two tokens
			if i+1 < len(toks) {
				if d, ok := parseDate(t+" "+toks[i+1], today); ok {
					dates = append(dates, d)
					i++
					if i+1 < len(toks) { // "3 Dec 2026"
						if y, err := strconv.Atoi(toks[i+1]); err == nil && y > 2000 {
							if d2, ok := parseDate(t+" "+toks[i]+" "+toks[i+1], today); ok {
								dates[len(dates)-1] = d2
								i++
							}
						}
					}
					continue
				}
			}
			if len(dates) == 0 {
				words = append(words, t)
			}
		}
	}
	if visa == "" || len(words) == 0 || (len(dates) == 0 && !none) {
		b.send(chat, usage, nil)
		return
	}
	posts := findPost(strings.Join(words, " "), "")
	if len(posts) == 0 {
		// maybe "India Mumbai"
		for n := 1; n < len(words) && len(posts) == 0; n++ {
			if cc, _ := findCountry(strings.Join(words[:n], " ")); cc != "" {
				posts = findPost(strings.Join(words[n:], " "), cc)
			}
		}
	}
	if len(posts) == 0 {
		b.send(chat, "I don't know the city <b>"+esc(strings.Join(words, " "))+"</b>. Use the consulate city, like Mumbai or Lagos.", nil)
		return
	}
	if len(posts) > 1 {
		names := []string{}
		for _, p := range posts {
			names = append(names, p.City+", "+p.Country)
		}
		b.send(chat, "Which one? "+esc(strings.Join(names, " / "))+"\nAdd the country, like <code>/report India Mumbai F1 3 Dec</code>.", nil)
		return
	}
	p := posts[0]
	rep := Report{Agent: "telegram", CC: p.CC, Visa: visa, Category: orAny(cat, "Regular"), CheckedAt: time.Now().UnixMilli()}
	rep.Posts = append(rep.Posts, struct {
		City  string   `json:"city"`
		Dates []string `json:"dates"`
	}{p.City, dates})
	res, err := b.reports.Ingest(rep, "tg:"+strconv.FormatInt(from.ID, 10), "telegram")
	if err != nil {
		b.send(chat, "Couldn't save that: "+esc(err.Error()), nil)
		return
	}
	switch {
	case none:
		b.send(chat, fmt.Sprintf("Thanks. Marked <b>%s %s</b> as no slots open.", esc(p.City), esc(visa)), nil)
	case res.Confirmed:
		b.send(chat, fmt.Sprintf("✅ Thanks! <b>%s %s</b> on %s is now confirmed (%d people). Everyone watching it is being alerted.", esc(p.City), esc(visa), prettyDate(dates[0]), res.Reports), nil)
	default:
		b.send(chat, fmt.Sprintf("Thanks! <b>%s %s</b> on %s is saved. It shows as confirmed when one more person reports it.", esc(p.City), esc(visa), prettyDate(dates[0])), nil)
	}
}
