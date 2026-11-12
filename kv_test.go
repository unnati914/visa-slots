package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// fakeUpstash speaks the Upstash REST protocol for HGETALL / HSET / HDEL.
func fakeUpstash(t *testing.T) (*httptest.Server, *int) {
	var mu sync.Mutex
	data := map[string]map[string]string{}
	calls := 0
	run := func(c []string) any {
		h := data[c[1]]
		if h == nil {
			h = map[string]string{}
			data[c[1]] = h
		}
		switch c[0] {
		case "HGETALL":
			out := []string{}
			for k, v := range h {
				out = append(out, k, v)
			}
			return out
		case "HSET":
			for i := 2; i+1 < len(c); i += 2 {
				h[c[i]] = c[i+1]
			}
			return len(c)/2 - 1
		case "HDEL":
			for _, k := range c[2:] {
				delete(h, k)
			}
			return len(c) - 2
		}
		t.Fatalf("unexpected command %v", c)
		return nil
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.URL.Path == "/pipeline" {
			var cmds [][]string
			json.NewDecoder(r.Body).Decode(&cmds)
			out := []map[string]any{}
			for _, c := range cmds {
				out = append(out, map[string]any{"result": run(c)})
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		var c []string
		json.NewDecoder(r.Body).Decode(&c)
		json.NewEncoder(w).Encode(map[string]any{"result": run(c)})
	}))
	return srv, &calls
}

func TestUpstashKeepsAlertsAcrossRestart(t *testing.T) {
	srv, calls := fakeUpstash(t)
	defer srv.Close()
	t.Setenv("UPSTASH_REDIS_REST_URL", srv.URL)
	t.Setenv("UPSTASH_REDIS_REST_TOKEN", "secret")

	kv := OpenKV(t.TempDir())
	if _, ok := kv.(*UpstashKV); !ok {
		t.Fatal("Upstash env set but file storage chosen")
	}
	a := NewAlerts(kv)
	p, err := NewPush(kv, "https://x.test", a)
	if err != nil {
		t.Fatal(err)
	}
	w1, _ := a.Add(Watch{Owner: "tg:1", CC: "IN", Visa: "F1/M1"})
	w2, _ := a.Add(Watch{Owner: "tg:2", CC: "MX"})
	a.save()
	a.Remove("tg:2", w2.ID)
	before := *calls
	a.save()
	a.save() // nothing changed: no request
	if *calls != before+1 {
		t.Fatalf("expected exactly one request for one change, got %d", *calls-before)
	}

	// "Restart": new objects, same Upstash.
	kv2 := OpenKV(t.TempDir())
	a2 := NewAlerts(kv2)
	p2, err := NewPush(kv2, "https://x.test", a2)
	if err != nil {
		t.Fatal(err)
	}
	if a2.Count() != 1 || a2.List("tg:1")[0].ID != w1.ID {
		t.Fatalf("after restart: %d watches", a2.Count())
	}
	if p2.pub != p.pub {
		t.Fatal("push keys changed after restart, every phone would lose alerts")
	}
}
