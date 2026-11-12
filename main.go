package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type feedFlags []string

func (f *feedFlags) String() string     { return strings.Join(*f, ",") }
func (f *feedFlags) Set(v string) error { *f = append(*f, v); return nil }

func main() {
	defAddr := ":8080"
	if port := os.Getenv("PORT"); port != "" { // Render and similar hosts pick the port
		defAddr = ":" + port
	}
	addr := flag.String("addr", defAddr, "listen address")
	mock := flag.Bool("mock", false, "also run simulated sources (demo only, not real data)")
	reportTTL := flag.Duration("report-ttl", 45*time.Minute, "drop browser-agent slots not re-checked within this time")
	mockEvery := flag.Duration("mock-interval", 2*time.Second, "how often each mock source is polled")
	dataDir := flag.String("data", envOr("DATA_DIR", "data"), "where alerts and keys are saved")
	site := flag.String("site", envOr("SITE_URL", os.Getenv("RENDER_EXTERNAL_URL")), "public URL of this server, used in alert links")
	var feeds feedFlags
	flag.Var(&feeds, "feed", "extra JSON source as name=url (repeatable), polled every 30s")
	flag.Parse()
	// Snapshot rebuilds allocate a few MB every couple of seconds. A higher GC target
	// means fewer collections competing with request handling.
	debug.SetGCPercent(400)

	reports := NewReportsSource(*reportTTL)
	sources := []Source{reports}
	if *mock {
		// Split posts the way the real world is split: some countries book through
		// the CGI system (usvisascheduling.com), most through AIS (ais.usvisa-info.com).
		cgi := map[string]bool{"IN": true, "CN": true, "NG": true, "PH": true, "VN": true, "BD": true,
			"PK": true, "JP": true, "KR": true, "TW": true, "HK": true, "TH": true, "ID": true, "MY": true,
			"SG": true, "NP": true, "LK": true, "ET": true, "KE": true, "GH": true, "ZA": true}
		var a, c []Post
		for _, p := range AllPosts() {
			if cgi[p.CC] {
				c = append(c, p)
			} else {
				a = append(a, p)
			}
		}
		sources = append(sources,
			NewMockSource("mock:ais", *mockEvery, a, 1),
			NewMockSource("mock:cgi", *mockEvery, c, 2))
	}
	for _, f := range feeds {
		name, url, ok := strings.Cut(f, "=")
		if !ok {
			log.Fatalf("bad -feed %q, want name=url", f)
		}
		sources = append(sources, NewHTTPJSONSource(name, url, 30*time.Second))
	}

	store, hub := &Store{}, NewHub()
	store.cur.Store(buildSnapshot(0, nil, nil)) // answer requests even before the agent starts
	agent := NewAgent(store, hub, sources...)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Alerts: Telegram and phone push both hang off the same watch list.
	kv := OpenKV(*dataDir)
	alerts := NewAlerts(kv)
	if _, ok := kv.(*UpstashKV); ok {
		alerts.SaveEvery = 30 * time.Second // stay well inside Upstash's free request quota
		log.Printf("storage: Upstash Redis")
	}
	agent.OnChange = alerts.OnChange
	go alerts.Run(ctx.Done())
	contact := *site
	if contact == "" {
		contact = "https://github.com/visa-slots"
	}
	push, err := NewPush(kv, contact, alerts)
	if err != nil {
		log.Fatalf("push setup: %v", err)
	}
	botName := ""
	if tok := os.Getenv("TELEGRAM_BOT_TOKEN"); tok != "" {
		bot := NewBot(tok, alerts, reports, store, *site)
		if err := bot.Run(ctx); err != nil {
			log.Printf("telegram: disabled, %v", err)
		} else {
			botName = bot.Username
		}
	} else {
		log.Printf("telegram: off (set TELEGRAM_BOT_TOKEN to turn it on)")
	}
	go agent.Run(ctx)
	go keepAwake(ctx, os.Getenv("RENDER_EXTERNAL_URL"))
	config := newPayload(map[string]any{"telegram": botName, "push": true, "confirmAt": ConfirmAt,
		"visas": VisaTypes, "categories": Categories})
	postsPayload := newPayload(postIndex())

	lat := &Latency{}
	api := http.NewServeMux()
	api.HandleFunc("GET /api/countries", func(w http.ResponseWriter, r *http.Request) {
		s := store.Load()
		servePayload(w, r, s, s.countries)
	})
	api.HandleFunc("GET /api/slots", func(w http.ResponseWriter, r *http.Request) {
		s := store.Load()
		cc := strings.ToUpper(r.URL.Query().Get("country"))
		if cc == "" {
			servePayload(w, r, s, s.all)
			return
		}
		p, ok := s.byCountry[cc]
		if !ok {
			http.Error(w, `{"error":"unknown country"}`, http.StatusNotFound)
			return
		}
		servePayload(w, r, s, p)
	})
	api.HandleFunc("GET /api/earliest", func(w http.ResponseWriter, r *http.Request) {
		s := store.Load()
		p, ok := s.earliest[r.URL.Query().Get("visa")]
		if !ok {
			http.Error(w, `{"error":"unknown visa type"}`, http.StatusNotFound)
			return
		}
		servePayload(w, r, s, p)
	})
	api.HandleFunc("GET /api/sources", func(w http.ResponseWriter, r *http.Request) {
		s := store.Load()
		servePayload(w, r, s, s.status)
	})
	api.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		servePayload(w, r, store.Load(), config)
	})
	api.HandleFunc("GET /api/posts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		servePayload(w, r, store.Load(), postsPayload)
	})
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServerFS(static)
	api.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/" || r.URL.Path == "/sw.js":
			w.Header().Set("Cache-Control", "no-cache") // app updates reach phones on next open
		case strings.HasPrefix(r.URL.Path, "/icons/"):
			w.Header().Set("Cache-Control", "public, max-age=86400")
		}
		if r.URL.Path == "/manifest.webmanifest" {
			w.Header().Set("Content-Type", "application/manifest+json")
		}
		files.ServeHTTP(w, r)
	})

	mux := http.NewServeMux()
	mux.Handle("/", timed(lat, api))
	mux.Handle("GET /api/stream", hub) // long-lived, not timed
	mux.Handle("POST /api/report", reports)
	mux.Handle("OPTIONS /api/report", reports)
	mux.Handle("/api/push/", push)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reset") == "1" {
			if host, _, _ := net.SplitHostPort(r.RemoteAddr); host != "127.0.0.1" && host != "::1" {
				http.Error(w, "reset is local only", http.StatusForbidden)
				return
			}
			lat.Reset()
		}
		s := store.Load()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"server_latency":   lat.Summary(),
			"sse_clients":      hub.Count(),
			"sse_dropped":      hub.dropped.Load(),
			"snapshot_version": s.Version,
			"goroutines":       runtime.NumGoroutine(),
			"watches":          alerts.Count(),
		})
	})

	srv := &http.Server{Addr: *addr, Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 120 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		srv.Shutdown(sctx)
	}()
	log.Printf("visa slots on http://localhost%s with %d sources, %d posts", *addr, len(sources), len(AllPosts()))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// keepAwake visits our own public URL every 10 minutes. Free hosts like Render put an
// app to sleep after 15 minutes with no incoming visits, which would stop alerts.
func keepAwake(ctx context.Context, url string) {
	if url == "" {
		return
	}
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if res, err := http.Get(strings.TrimRight(url, "/") + "/api/config"); err == nil {
				res.Body.Close()
			} else {
				log.Printf("keep-awake: %v", err)
			}
		}
	}
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// postIndex is every country and its posts, for the app's pickers.
func postIndex() []map[string]any {
	out := []map[string]any{}
	seen := map[string]bool{}
	for _, p := range AllPosts() {
		if seen[p.CC] {
			continue
		}
		seen[p.CC] = true
		cities := []string{}
		for _, q := range postsByCC[p.CC] {
			cities = append(cities, q.City)
		}
		out = append(out, map[string]any{"cc": p.CC, "country": p.Country, "cities": cities})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["country"].(string) < out[j]["country"].(string) })
	return out
}

// servePayload writes a pre-encoded body. There is no JSON encoding or DB call on
// this path, only header setup and a memory copy.
func servePayload(w http.ResponseWriter, r *http.Request, s *Snapshot, p *payload) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", p.etag)
	h.Set("X-Snapshot-Version", strconv.FormatUint(s.Version, 10))
	h.Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == p.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := p.raw
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = p.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}
