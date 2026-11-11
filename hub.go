package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hub fans out server-sent events to every connected browser. Each frame is
// encoded once and the same bytes go to all clients.
type Hub struct {
	mu      sync.RWMutex
	clients map[*sseClient]struct{}
	dropped atomic.Int64
	queue   chan broadcastJob
}

type broadcastJob struct {
	frames   map[string][]byte
	defFrame []byte
}

// Waking thousands of streams in the same instant causes a CPU spike that delays
// ordinary requests. Each broadcast is spread over ~spread in batches instead.
const (
	spread  = 100 * time.Millisecond
	batches = 16
)

type sseClient struct {
	topic string // country code, or "" for the worldwide view
	ch    chan []byte
	kick  chan struct{}
	once  sync.Once
}

func NewHub() *Hub {
	h := &Hub{clients: map[*sseClient]struct{}{}, queue: make(chan broadcastJob, 64)}
	go h.run()
	return h
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast sends each client the frame for its topic (country), or def if that
// topic had no changes. Every frame is encoded once, however many clients share it.
func (h *Hub) Broadcast(event string, byTopic map[string][]byte, def []byte) {
	enc := func(d []byte) []byte { return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, d)) }
	frames := make(map[string][]byte, len(byTopic))
	for t, d := range byTopic {
		frames[t] = enc(d)
	}
	h.queue <- broadcastJob{frames, enc(def)}
}

// run delivers broadcasts in order, one at a time, each paced across `spread`.
func (h *Hub) run() {
	for job := range h.queue {
		h.mu.RLock()
		clients := make([]*sseClient, 0, len(h.clients))
		for c := range h.clients {
			clients = append(clients, c)
		}
		h.mu.RUnlock()
		per := (len(clients) + batches - 1) / batches
		for i := 0; i < len(clients); i += max(per, 1) {
			if i > 0 {
				time.Sleep(spread / batches)
			}
			for _, c := range clients[i:min(i+per, len(clients))] {
				frame, ok := job.frames[c.topic]
				if !ok {
					frame = job.defFrame
				}
				select {
				case c.ch <- frame:
				default:
					// Client is too slow. Disconnect it; the browser reconnects and refetches.
					c.once.Do(func() { close(c.kick) })
					h.dropped.Add(1)
				}
			}
		}
	}
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // stream stays open
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	c := &sseClient{topic: strings.ToUpper(r.URL.Query().Get("country")),
		ch: make(chan []byte, 32), kick: make(chan struct{})}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
	}()

	fmt.Fprintf(w, "retry: 2000\nevent: hello\ndata: {}\n\n")
	rc.Flush()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-c.kick:
			return
		case f := <-c.ch:
			if _, err := w.Write(f); err != nil {
				return
			}
			rc.Flush()
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			rc.Flush()
		}
	}
}
