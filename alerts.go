package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"
)

// Watch is one alert someone asked for: "tell me when an F1 slot opens in Mumbai before March".
type Watch struct {
	ID     string          `json:"id"`
	Owner  string          `json:"owner"` // "tg:<chat id>" or "push:<device>"
	CC     string          `json:"cc"`
	Visa   string          `json:"visa,omitempty"` // "" = any visa type
	City   string          `json:"city,omitempty"` // "" = any post in the country
	Before string          `json:"before,omitempty"`
	Sent   map[string]sent `json:"sent,omitempty"` // row key -> what we last alerted
}

type sent struct {
	Date string `json:"d"`
	At   int64  `json:"t"`
}

func (w *Watch) matches(r Row) bool {
	return w.CC == r.CC && (w.Visa == "" || w.Visa == r.Visa) && (w.City == "" || w.City == r.City)
}

// Notifier delivers alerts for one owner prefix ("tg", "push"). It must not block.
type Notifier interface {
	Notify(owner string, w *Watch, rows []Row)
}

// Alerts keeps every watch, matches each change against them, and hands matches to
// the notifier for that owner. Watches are saved to disk so restarts keep them.
type Alerts struct {
	mu        sync.Mutex
	kv        KV
	watches   map[string]*Watch // id -> watch
	notifiers map[string]Notifier
	in        chan []Row
	dirty     map[string]bool // watch ids changed or removed since the last save
	SaveEvery time.Duration
}

const maxWatchesPerOwner = 10

func NewAlerts(kv KV) *Alerts {
	a := &Alerts{kv: kv, watches: map[string]*Watch{}, notifiers: map[string]Notifier{},
		in: make(chan []Row, 256), dirty: map[string]bool{}, SaveEvery: 5 * time.Second}
	saved, err := kv.LoadAll("watches")
	if err != nil {
		log.Fatalf("alerts: can't load saved alerts: %v", err) // don't start empty and overwrite them
	}
	for id, b := range saved {
		var w Watch
		if json.Unmarshal(b, &w) == nil {
			w.ID = id
			a.watches[id] = &w
		}
	}
	log.Printf("alerts: %d watches loaded", len(a.watches))
	return a
}

func (a *Alerts) Register(prefix string, n Notifier) { a.notifiers[prefix] = n }

// OnChange is wired to the Agent. It never blocks the merge loop.
func (a *Alerts) OnChange(rows []Row) {
	select {
	case a.in <- rows:
	default:
		log.Printf("alerts: queue full, dropped %d changes", len(rows))
	}
}

func (a *Alerts) Run(stop <-chan struct{}) {
	save := time.NewTicker(a.SaveEvery)
	defer save.Stop()
	for {
		select {
		case <-stop:
			a.save()
			return
		case rows := <-a.in:
			a.match(rows)
		case <-save.C:
			a.save()
		}
	}
}

func (a *Alerts) match(rows []Row) {
	now := time.Now().UnixMilli()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range a.watches {
		var hits []Row
		for _, r := range rows {
			if !w.matches(r) {
				continue
			}
			good := r.Open > 0 && r.Confirmed && (w.Before == "" || r.Earliest <= w.Before)
			prev, had := w.Sent[r.Key]
			if !good {
				if had { // slot gone: alert again next time it opens
					delete(w.Sent, r.Key)
					a.dirty[w.ID] = true
				}
				continue
			}
			// Alert on a new opening or an earlier date. Re-alert the same date after 12 h.
			if !had || r.Earliest < prev.Date || (r.Earliest != prev.Date && now-prev.At > 12*3600*1000) {
				if w.Sent == nil {
					w.Sent = map[string]sent{}
				}
				w.Sent[r.Key] = sent{r.Earliest, now}
				a.dirty[w.ID] = true
				hits = append(hits, r)
			}
		}
		if len(hits) == 0 {
			continue
		}
		sort.Slice(hits, func(i, j int) bool { return hits[i].Earliest < hits[j].Earliest })
		if n := a.notifiers[prefixOf(w.Owner)]; n != nil {
			c := *w // notifiers run later on other goroutines; give them a copy
			c.Sent = nil
			n.Notify(w.Owner, &c, hits)
		}
	}
}

func prefixOf(owner string) string {
	for i := 0; i < len(owner); i++ {
		if owner[i] == ':' {
			return owner[:i]
		}
	}
	return owner
}

// Add stores a watch. An identical watch from the same owner is replaced, not duplicated.
func (a *Alerts) Add(w Watch) (*Watch, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for id, x := range a.watches {
		if x.Owner != w.Owner {
			continue
		}
		if x.CC == w.CC && x.Visa == w.Visa && x.City == w.City {
			delete(a.watches, id)
			a.dirty[id] = true
			continue
		}
		n++
	}
	if n >= maxWatchesPerOwner {
		return nil, false
	}
	b := make([]byte, 6)
	rand.Read(b)
	w.ID = hex.EncodeToString(b)
	a.watches[w.ID] = &w
	a.dirty[w.ID] = true
	return &w, true
}

func (a *Alerts) List(owner string) []Watch {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []Watch
	for _, w := range a.watches {
		if w.Owner == owner {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CC+out[i].City+out[i].Visa < out[j].CC+out[j].City+out[j].Visa })
	return out
}

func (a *Alerts) Remove(owner, id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if w := a.watches[id]; w != nil && w.Owner == owner {
		delete(a.watches, id)
		a.dirty[id] = true
		return true
	}
	return false
}

// RemoveOwner drops every watch of one owner (they said /stop, blocked the bot, or the push subscription died).
func (a *Alerts) RemoveOwner(owner string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for id, w := range a.watches {
		if w.Owner == owner {
			delete(a.watches, id)
			a.dirty[id] = true
			n++
		}
	}
	return n
}

func (a *Alerts) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.watches)
}

func (a *Alerts) save() {
	a.mu.Lock()
	if len(a.dirty) == 0 {
		a.mu.Unlock()
		return
	}
	put, del := map[string][]byte{}, []string{}
	for id := range a.dirty {
		if w := a.watches[id]; w != nil {
			put[id], _ = json.Marshal(w)
		} else {
			del = append(del, id)
		}
	}
	pending := a.dirty
	a.dirty = map[string]bool{}
	a.mu.Unlock()
	if err := a.kv.Save("watches", put, del); err != nil {
		log.Printf("alerts: save failed, will retry: %v", err)
		a.mu.Lock()
		for id := range pending {
			a.dirty[id] = true
		}
		a.mu.Unlock()
	}
}
