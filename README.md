# US Visa Slots agent

A background agent fetches US visa appointment availability from pluggable sources
for 213 posts in 160 countries and 7 visa types, regular and dropbox. It keeps the
latest picture in memory and serves it to users in microseconds. New slots are pushed
to open browsers live.

```
 sources (each polled on its own schedule, with backoff)
   mock:ais ─┐
   mock:cgi ─┼─► merger (1 goroutine) ─► immutable snapshot, all responses ─► API reads (atomic pointer, memcpy)
   -feed …  ─┘          │                 pre-encoded + pre-gzipped
                        └─► per-country diff ─► SSE hub (paced over 100 ms) ─► browsers
```

User requests never trigger a fetch. They read bytes that were encoded in advance,
so latency does not depend on how slow the visa websites are.

## Run

```sh
go build -o bin/server . && ./bin/server            # http://localhost:8080
go build -o bin/loadtest ./cmd/loadtest
./bin/loadtest -users 4000 -d 30s -think 1s          # realistic: 4000 users, each with a live stream
./bin/loadtest -users 4000 -workers 400 -think 0     # stress: 400 request loops with no think time
```

Flags: `-addr :8080`, `-mock` (adds simulated demo data, off by default), `-report-ttl 45m`, `-feed name=url` (repeatable).

## API

| Endpoint | What |
|---|---|
| `GET /api/countries` | Countries with post count, open combos, earliest date |
| `GET /api/slots?country=IN` | Every row for a country (`/api/slots` with no country returns the whole world, about 75 KB gzipped) |
| `GET /api/earliest?visa=F1/M1` | Top 100 earliest open slots worldwide (`visa` empty = all types) |
| `GET /api/sources` | Health of each source: rows, fetch time, failures |
| `GET /api/stream?country=IN` | SSE: `diff` events with changed rows for that country; without `country` you get version ticks |
| `POST /api/report` | Where the browser agent sends slots it saw |
| `GET /metrics` | Server-side latency histogram (p50/p90/p99/p99.9/max), SSE count |

Responses carry `ETag`, so a browser revalidating gets a `304`.

## Measured (MacBook, 8 cores, load generator on the same machine)

| Scenario | Throughput | Server p99 | Client p99 |
|---|---|---|---|
| 4000 users, 1 s think time, 4000 live SSE streams | 3,990 req/s, 0 errors | **0.03 ms** | **1.7 ms** |
| Stress: 4000 SSE + 400 request loops with no pause | 25,000 req/s, 0 errors | **0.09 ms** | load generator saturated the CPU |

"Server p99" is from handler start to response written. A real user also pays the
network round trip to the server, roughly 20 to 200 ms depending on distance. No server
design removes that, so deploy close to users or behind a CDN if that matters.

Two fixes brought the client p99 down from 27 ms to 1.7 ms:
1. **Per-country streams.** A viewer gets only its own country's changes, not the worldwide diff.
2. **Paced broadcast.** Updates go out in 16 batches over about 100 ms, so 4000 sockets don't wake in the same instant.

## Real data: the browser agent (extension/)

Live US visa slots are only visible inside a logged-in visa account. The agent is a
Chrome extension that runs in **your own logged-in session**. It never logs in or solves
CAPTCHAs by itself. Every N minutes (10 by default, 5 at least) it:

1. Finds your open visa tab: ais.usvisa-info.com (Canada, Mexico, Brazil, Colombia, Europe and others)
   or usvisascheduling.com (India, China, Nigeria and others).
2. Reads the calendar of every consulate in your account's dropdown, a few seconds apart, the same
   requests the page makes when you pick a consulate by hand.
3. Sends the dates to `POST /api/report`. They show up live on the dashboard for everyone.
4. Shows a Chrome alert when a date opens earlier than before, or earlier than your target date.
   Click the alert to jump to your visa tab and book.

**Install:** open `chrome://extensions` → turn on Developer mode → Load unpacked → pick the
`extension` folder. Click the icon and set your visa type, appointment type and target date.
For usvisascheduling.com also set the country, since its URL doesn't include one. Log in to your
visa account, open the appointment / reschedule page, and leave that tab open.

**Limits:**
- One account only sees its own country and visa category. More people running the agent
  means more coverage. Slots nobody re-checks within 45 minutes (`-report-ttl`) drop off.
- Visa sites do lock accounts that refresh too often. Keep the interval at 10 minutes or more.
  The agent stops and alerts you if you get signed out.
- `/api/report` is public, so anyone could post fake dates. Inputs are validated (known country,
  city and visa type; future dates only) and each IP is rate-limited. Before opening this to the
  public, add per-user tokens.

## Adding a real source

The mock sources simulate two booking systems. To add a real one, either:

- **Without code:** run a separate scraper or collector that serves `{"rows":[{"cc","country","city","visa","cat","dates":[...],"open"}]}`
  and pass `-feed mysource=https://…`.
- **In code:** implement `Source` (`Name`, `Interval`, `Fetch`) in `sources.go` and register it in `main.go`.
  `Fetch` must return the full current picture, because rows a source stops returning are removed.

Before scraping the official booking sites (ais.usvisa-info.com, usvisascheduling.com):
they require a login, use CAPTCHAs, and their terms forbid automated access. Accounts that
poll them can get locked. Safer options are crowdsourced reports (users or a browser
extension submit what they see) or a data partner.
