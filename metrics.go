package main

import (
	"net/http"
	"sync/atomic"
	"time"
)

// Latency is a lock-free histogram with 10 µs buckets up to 20 ms. It measures
// server time per request: from the handler starting to the response being written.
type Latency struct {
	buckets [2001]atomic.Uint64 // last bucket = overflow
	count   atomic.Uint64
	maxNs   atomic.Int64
}

const bucketNs = 10_000

func (l *Latency) Observe(d time.Duration) {
	i := int(d.Nanoseconds() / bucketNs)
	if i >= len(l.buckets) {
		i = len(l.buckets) - 1
	}
	l.buckets[i].Add(1)
	l.count.Add(1)
	for {
		m := l.maxNs.Load()
		if d.Nanoseconds() <= m || l.maxNs.CompareAndSwap(m, d.Nanoseconds()) {
			break
		}
	}
}

func (l *Latency) Reset() {
	for i := range l.buckets {
		l.buckets[i].Store(0)
	}
	l.count.Store(0)
	l.maxNs.Store(0)
}

// Quantile returns the upper edge of the bucket holding quantile q, in microseconds.
func (l *Latency) Quantile(q float64) float64 {
	n := l.count.Load()
	if n == 0 {
		return 0
	}
	target := uint64(float64(n)*q + 0.5)
	if target == 0 {
		target = 1
	}
	var seen uint64
	for i := range l.buckets {
		seen += l.buckets[i].Load()
		if seen >= target {
			return float64((i+1)*bucketNs) / 1000
		}
	}
	return float64(l.maxNs.Load()) / 1000
}

type LatencySummary struct {
	Count uint64  `json:"count"`
	P50   float64 `json:"p50_us"`
	P90   float64 `json:"p90_us"`
	P99   float64 `json:"p99_us"`
	P999  float64 `json:"p999_us"`
	Max   float64 `json:"max_us"`
}

func (l *Latency) Summary() LatencySummary {
	return LatencySummary{l.count.Load(), l.Quantile(.5), l.Quantile(.9), l.Quantile(.99), l.Quantile(.999),
		float64(l.maxNs.Load()) / 1000}
}

func timed(l *Latency, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		h.ServeHTTP(w, r)
		l.Observe(time.Since(start))
	})
}
