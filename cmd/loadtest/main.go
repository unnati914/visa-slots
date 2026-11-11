// Load test: N simulated users. Each holds a live SSE stream (like an open dashboard)
// and keeps making API requests with a think time between them.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

var countries = []string{"IN", "IN", "IN", "MX", "MX", "CN", "BR", "CO", "NG", "PH", "CA", "GB", "DE", "VN", "PK", "AE", "JP", "KR", "AR", "PE"}
var visas = []string{"", "B1/B2", "F1/M1", "J1", "H1B", "L1", "O1", "C1/D"}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "server base URL")
	users := flag.Int("users", 4000, "concurrent users")
	dur := flag.Duration("d", 30*time.Second, "measured duration")
	think := flag.Duration("think", time.Second, "mean pause between a user's requests (0 = hammer)")
	sse := flag.Bool("sse", true, "each user also holds a live SSE stream")
	workers := flag.Int("workers", 0, "if >0, only this many users send requests (all still hold SSE); use with -think 0 for stress")
	ramp := flag.Duration("ramp", 5*time.Second, "time to bring all users online")
	flag.Parse()

	dial := (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	api := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: dial, MaxIdleConns: *users * 2, MaxIdleConnsPerHost: *users * 2, IdleConnTimeout: 90 * time.Second}}
	stream := &http.Client{Transport: &http.Transport{DialContext: dial, MaxIdleConnsPerHost: *users * 2}}

	ctx, cancel := context.WithCancel(context.Background())
	var measuring atomic.Bool
	var reqs, errs, events, sseUp, notMod atomic.Int64
	lat := make([][]time.Duration, *users)
	var wg sync.WaitGroup

	fmt.Printf("ramping %d users over %s (sse=%v, think=%s)...\n", *users, *ramp, *sse, *think)
	for u := 0; u < *users; u++ {
		wg.Add(1)
		go func(u int) {
			defer wg.Done()
			time.Sleep(time.Duration(int64(*ramp) * int64(u) / int64(*users)))
			if *sse {
				go func() {
					path := "/api/stream" // 30% watch the worldwide view, 70% one country
					if u%10 < 7 {
						path += "?country=" + countries[u%len(countries)]
					}
					req, _ := http.NewRequestWithContext(ctx, "GET", *base+path, nil)
					resp, err := stream.Do(req)
					if err != nil {
						errs.Add(1)
						return
					}
					defer resp.Body.Close()
					sseUp.Add(1)
					sc := bufio.NewScanner(resp.Body)
					sc.Buffer(make([]byte, 1<<20), 8<<20)
					for sc.Scan() {
						if len(sc.Bytes()) > 6 && string(sc.Bytes()[:6]) == "event:" && measuring.Load() {
							events.Add(1)
						}
					}
				}()
			}
			if *workers > 0 && u >= *workers {
				<-ctx.Done()
				return
			}
			rng := rand.New(rand.NewPCG(uint64(u), 7))
			etags := map[string]string{}
			for ctx.Err() == nil {
				var path string
				switch x := rng.IntN(100); {
				case x < 55:
					path = "/api/slots?country=" + countries[rng.IntN(len(countries))]
				case x < 85:
					path = "/api/earliest?visa=" + visas[rng.IntN(len(visas))]
				case x < 95:
					path = "/api/countries"
				default:
					path = "/api/sources"
				}
				req, _ := http.NewRequestWithContext(ctx, "GET", *base+path, nil)
				if et := etags[path]; et != "" && rng.IntN(2) == 0 { // half the time the browser revalidates
					req.Header.Set("If-None-Match", et)
				}
				t := time.Now()
				resp, err := api.Do(req)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
				d := time.Since(t)
				if measuring.Load() {
					if err != nil || (resp.StatusCode != 200 && resp.StatusCode != 304) {
						if ctx.Err() == nil {
							errs.Add(1)
						}
					} else {
						reqs.Add(1)
						lat[u] = append(lat[u], d)
						if resp.StatusCode == 304 {
							notMod.Add(1)
						}
					}
				}
				if err == nil {
					etags[path] = resp.Header.Get("ETag")
				}
				if *think > 0 {
					pause := time.Duration(float64(*think) * (0.5 + rng.Float64()))
					select {
					case <-time.After(pause):
					case <-ctx.Done():
					}
				}
			}
		}(u)
	}

	time.Sleep(*ramp + 3*time.Second) // ramp + warm-up
	http.Get(*base + "/metrics?reset=1")
	measuring.Store(true)
	fmt.Printf("measuring for %s...\n", *dur)
	time.Sleep(*dur)
	measuring.Store(false)

	resp, err := http.Get(*base + "/metrics")
	var m struct {
		Raw        map[string]float64 `json:"server_latency"`
		SSEClients int                `json:"sse_clients"`
		SSEDropped int                `json:"sse_dropped"`
	}
	if err == nil {
		json.NewDecoder(resp.Body).Decode(&m)
		resp.Body.Close()
	}
	cancel()
	wg.Wait()

	var all []time.Duration
	for _, l := range lat {
		all = append(all, l...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	q := func(p float64) time.Duration {
		if len(all) == 0 {
			return 0
		}
		return all[min(len(all)-1, int(float64(len(all))*p))]
	}
	ms := func(us float64) string { return fmt.Sprintf("%.3f ms", us/1000) }

	fmt.Printf("\n=== %d users, %s ===\n", *users, *dur)
	fmt.Printf("requests ok: %d (%.0f req/s), 304s: %d, errors: %d\n", reqs.Load(), float64(reqs.Load())/dur.Seconds(), notMod.Load(), errs.Load())
	fmt.Printf("live SSE streams: %d connected, server saw %d, dropped as slow: %d, events received: %d\n",
		sseUp.Load(), m.SSEClients, m.SSEDropped, events.Load())
	fmt.Printf("\nSERVER-SIDE latency (handler start to response written):\n")
	fmt.Printf("  p50 %s   p90 %s   p99 %s   p99.9 %s   max %s   (n=%.0f)\n",
		ms(m.Raw["p50_us"]), ms(m.Raw["p90_us"]), ms(m.Raw["p99_us"]), ms(m.Raw["p999_us"]), ms(m.Raw["max_us"]), m.Raw["count"])
	fmt.Printf("\nCLIENT-OBSERVED latency on this machine (includes load generator + loopback TCP):\n")
	fmt.Printf("  p50 %s   p90 %s   p99 %s   p99.9 %s   max %s\n", q(.5), q(.9), q(.99), q(.999), q(1))
}
