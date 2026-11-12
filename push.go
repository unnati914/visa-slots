package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// Push sends alerts to the phone app (the installed web app) through Web Push.
// Works on Android, desktop browsers, and on iPhone once the app is added to the Home Screen.
type Push struct {
	pub, priv string
	contact   string
	alerts    *Alerts
	client    *http.Client

	mu   sync.Mutex
	subs map[string]webpush.Subscription // owner -> subscription
	kv   KV
	out  chan pushJob
}

type pushJob struct {
	owner string
	body  []byte
}

func NewPush(kv KV, contact string, alerts *Alerts) (*Push, error) {
	p := &Push{contact: contact, alerts: alerts, subs: map[string]webpush.Subscription{}, kv: kv,
		out: make(chan pushJob, 20000), client: &http.Client{Timeout: 15 * time.Second}}
	saved, err := kv.LoadAll("push")
	if err != nil {
		return nil, err
	}
	// The key pair must never change, or every phone's subscription stops working.
	var keys struct{ Public, Private string }
	if b, ok := saved["_vapid"]; ok && json.Unmarshal(b, &keys) == nil && keys.Public != "" {
		p.pub, p.priv = keys.Public, keys.Private
	} else {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		keys.Public, keys.Private = pub, priv
		b, _ := json.Marshal(keys)
		if err := kv.Save("push", map[string][]byte{"_vapid": b}, nil); err != nil {
			return nil, err
		}
		p.pub, p.priv = pub, priv
	}
	for owner, b := range saved {
		var sub webpush.Subscription
		if strings.HasPrefix(owner, "push:") && json.Unmarshal(b, &sub) == nil {
			p.subs[owner] = sub
		}
	}
	alerts.Register("push", p)
	for i := 0; i < 8; i++ {
		go p.worker()
	}
	return p, nil
}

func (p *Push) Notify(owner string, w *Watch, rows []Row) {
	r := rows[0]
	title := fmt.Sprintf("Slot open: %s %s", r.City, r.Visa)
	body := fmt.Sprintf("%s (in %d days)", prettyDate(r.Earliest), daysUntil(r.Earliest))
	if len(rows) > 1 {
		body += fmt.Sprintf(" · +%d more", len(rows)-1)
	}
	j, _ := json.Marshal(map[string]string{"title": title, "body": body, "url": "/?country=" + w.CC, "tag": w.ID})
	select {
	case p.out <- pushJob{owner, j}:
	default:
		log.Printf("push: queue full")
	}
}

func (p *Push) worker() {
	for job := range p.out {
		p.mu.Lock()
		sub, ok := p.subs[job.owner]
		p.mu.Unlock()
		if !ok {
			continue
		}
		res, err := webpush.SendNotification(job.body, &sub, &webpush.Options{
			Subscriber: p.contact, VAPIDPublicKey: p.pub, VAPIDPrivateKey: p.priv,
			TTL: 600, Urgency: webpush.UrgencyHigh, HTTPClient: p.client})
		if err != nil {
			log.Printf("push: %v", err)
			continue
		}
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode == 404 || res.StatusCode == 410 { // app uninstalled or permission revoked
			p.forget(job.owner)
		}
	}
}

func (p *Push) forget(owner string) {
	p.mu.Lock()
	delete(p.subs, owner)
	p.mu.Unlock()
	p.persist(owner, nil)
	p.alerts.RemoveOwner(owner)
}

// persist saves one device's subscription, or deletes it when sub is nil.
func (p *Push) persist(owner string, sub *webpush.Subscription) {
	var err error
	if sub == nil {
		err = p.kv.Save("push", nil, []string{owner})
	} else {
		b, _ := json.Marshal(sub)
		err = p.kv.Save("push", map[string][]byte{owner: b}, nil)
	}
	if err != nil {
		log.Printf("push: save: %v", err)
	}
}

func pushOwner(endpoint string) string {
	h := sha1.Sum([]byte(endpoint))
	return "push:" + hex.EncodeToString(h[:10])
}

// ServeHTTP handles the app's alert endpoints:
//
//	GET  /api/push/key            public key for subscribing
//	POST /api/push/watch          {sub, cc, visa, city, before} → replaces this device's alert
//	POST /api/push/list           {endpoint} → this device's alerts
//	POST /api/push/unwatch        {endpoint, id?} → remove one alert, or all of them
func (p *Push) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/key") {
		json.NewEncoder(w).Encode(map[string]string{"key": p.pub})
		return
	}
	var req struct {
		Sub      webpush.Subscription `json:"sub"`
		Endpoint string               `json:"endpoint"`
		ID       string               `json:"id"`
		CC       string               `json:"cc"`
		Visa     string               `json:"visa"`
		City     string               `json:"city"`
		Before   string               `json:"before"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if req.Endpoint == "" {
		req.Endpoint = req.Sub.Endpoint
	}
	if !strings.HasPrefix(req.Endpoint, "https://") {
		http.Error(w, `{"error":"missing push subscription"}`, http.StatusBadRequest)
		return
	}
	owner := pushOwner(req.Endpoint)
	switch {
	case strings.HasSuffix(r.URL.Path, "/watch"):
		req.CC = strings.ToUpper(req.CC)
		if countryName(req.CC) == "" || (req.Visa != "" && !contains(VisaTypes, req.Visa)) || req.Sub.Keys.P256dh == "" {
			http.Error(w, `{"error":"need a country and a valid subscription"}`, http.StatusBadRequest)
			return
		}
		city := ""
		if req.City != "" {
			city = matchCity(req.CC, req.City)
		}
		before := ""
		if dateRe.MatchString(req.Before) {
			before = req.Before
		}
		p.mu.Lock()
		p.subs[owner] = req.Sub
		p.mu.Unlock()
		p.persist(owner, &req.Sub)
		saved, ok := p.alerts.Add(Watch{Owner: owner, CC: req.CC, Visa: req.Visa, City: city, Before: before})
		if !ok {
			http.Error(w, `{"error":"this device has 10 alerts already, remove one first"}`, http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(saved)
	case strings.HasSuffix(r.URL.Path, "/list"):
		list := p.alerts.List(owner)
		if list == nil {
			list = []Watch{}
		}
		for i := range list {
			list[i].Sent, list[i].Owner = nil, ""
		}
		json.NewEncoder(w).Encode(list)
	case strings.HasSuffix(r.URL.Path, "/unwatch"):
		if req.ID != "" {
			p.alerts.Remove(owner, req.ID)
		} else {
			p.alerts.RemoveOwner(owner)
		}
		if len(p.alerts.List(owner)) == 0 {
			p.mu.Lock()
			delete(p.subs, owner)
			p.mu.Unlock()
			p.persist(owner, nil)
		}
		w.Write([]byte(`{"ok":true}`))
	case strings.HasSuffix(r.URL.Path, "/test"):
		p.Notify(owner, &Watch{CC: "IN"}, []Row{{City: "Test", Visa: "alert", Earliest: time.Now().AddDate(0, 0, 7).Format("2006-01-02")}})
		w.Write([]byte(`{"ok":true}`))
	default:
		http.NotFound(w, r)
	}
}
