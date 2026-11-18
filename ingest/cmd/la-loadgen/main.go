// Command la-loadgen is a closed-loop, stepped load generator for the native
// ingestion endpoint (POST /v1/e).
//
// For every step it runs a fixed number of concurrent senders, each posting
// unique batches back to back, and reports throughput, acknowledgement
// latency percentiles and, from the server's /metrics, the WAL fsync latency
// during that step. Saturation is where adding senders stops adding
// throughput.
//
//	la-loadgen -url http://127.0.0.1:8080 -token pk_load -steps 1,4,16,64,256 -step-duration 10s
//
// The target must not rate-limit the generator (LA_RATE_IP=0 and a high
// rate_limit on the token).
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mucan54/izlek/ingest/wire"
)

type stepResult struct {
	conns     int
	elapsed   time.Duration
	requests  int
	events    int
	bytes     int64
	codes     map[int]int
	netErrors int
	lat       []time.Duration // sorted, successful requests only
	fsync     histDelta
}

func main() {
	var (
		target   = flag.String("url", "http://127.0.0.1:8080", "ingestion base URL")
		token    = flag.String("token", "", "write token (pk_...)")
		stepsArg = flag.String("steps", "1,2,4,8,16,32,64,128,256", "concurrent senders per step")
		stepDur  = flag.Duration("step-duration", 10*time.Second, "duration of each step")
		warmup   = flag.Duration("warmup", 2*time.Second, "warm-up before the first step (not reported)")
		events   = flag.Int("events", 20, "events per batch")
		useGzip  = flag.Bool("gzip", false, "send Content-Encoding: gzip bodies")
		metrics  = flag.String("metrics-url", "", "server /metrics URL (default <url>/metrics)")
	)
	flag.Parse()
	if *token == "" {
		fmt.Fprintln(os.Stderr, "la-loadgen: -token is required")
		os.Exit(2)
	}
	if *events < 1 || *events > wire.MaxRecords {
		fmt.Fprintf(os.Stderr, "la-loadgen: -events must be 1..%d\n", wire.MaxRecords)
		os.Exit(2)
	}
	var steps []int
	for _, s := range strings.Split(*stepsArg, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			fmt.Fprintf(os.Stderr, "la-loadgen: bad -steps entry %q\n", s)
			os.Exit(2)
		}
		steps = append(steps, n)
	}
	if *metrics == "" {
		*metrics = strings.TrimRight(*target, "/") + "/metrics"
	}

	maxConns := 0
	for _, n := range steps {
		maxConns = max(maxConns, n)
	}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConns:        maxConns + 8,
			MaxIdleConnsPerHost: maxConns + 8,
			IdleConnTimeout:     time.Minute,
		},
	}
	g := &generator{client: client, url: strings.TrimRight(*target, "/") + "/v1/e", token: *token, events: *events, gzip: *useGzip}
	tmpl, _, _ := g.template()
	fmt.Printf("target %s, %d events/batch, batch body %d bytes (%d bytes/event), gzip=%v\n",
		g.url, *events, len(tmpl), len(tmpl) / *events, *useGzip)

	if *warmup > 0 {
		g.run(min(steps[0], 8), *warmup)
	}
	var results []stepResult
	for _, n := range steps {
		before, _ := scrapeFsync(client, *metrics)
		r := g.run(n, *stepDur)
		after, err := scrapeFsync(client, *metrics)
		if err == nil {
			r.fsync = after.sub(before)
		}
		results = append(results, r)
		printStep(r)
	}
	printSummary(results)
}

type generator struct {
	client *http.Client
	url    string
	token  string
	events int
	gzip   bool
	seq    atomic.Uint64
}

// template builds one realistic batch. It returns the body, the offset of
// the 16-byte batch_id and the offsets of every record uuid; each request
// overwrites those (as real batches differ in exactly these high-entropy
// fields), which also keeps the WAL from compressing unrealistically well.
func (g *generator) template() (body []byte, idOff int, uuidOffs []int) {
	b := &wire.Batch{
		SentAt: uint64(time.Now().UnixMilli()),
		SDK:    "loadgen/0.1.0",
		AnonID: "0193f1a2-0000-7000-8000-000000000001",
		Context: []wire.Prop{
			{Key: "platform", Tag: wire.TagString, Str: "android"},
			{Key: "os_version", Tag: wire.TagString, Str: "15"},
			{Key: "device_model", Tag: wire.TagString, Str: "Pixel 9"},
			{Key: "app_version", Tag: wire.TagString, Str: "3.2.0"},
			{Key: "locale", Tag: wire.TagString, Str: "tr-TR"},
			{Key: "timezone", Tag: wire.TagString, Str: "Europe/Istanbul"},
		},
	}
	body = wire.AppendHeader(nil, b)
	_, n := binary.Uvarint(body[wire.MagicLen:])
	idOff = wire.MagicLen + n
	names := []string{"screen", "button_tapped", "item_viewed", "add_to_cart", "checkout_started", "purchase_completed"}
	for i := 0; i < g.events; i++ {
		r := wire.Record{Kind: wire.KindEvent, TS: b.SentAt - uint64(g.events-i)*250, Name: names[i%len(names)]}
		r.Props = []wire.Prop{
			{Key: "screen", Tag: wire.TagString, Str: "ProductDetail"},
			{Key: "item_id", Tag: wire.TagString, Str: "sku-" + strconv.Itoa(100000+i*7919%900000)},
			{Key: "price", Tag: wire.TagFloat, Float: 129.90 + float64(i)},
			{Key: "position", Tag: wire.TagInt, Int: int64(i)},
			{Key: "logged_in", Tag: wire.TagTrue},
			{Key: "experiment", Tag: wire.TagString, Str: "new_checkout:b"},
		}
		start := len(body)
		body = wire.AppendRecord(body, &r)
		_, ln := binary.Uvarint(body[start:])
		uuidOffs = append(uuidOffs, start+ln+1) // after varint(len) and kind
	}
	return body, idOff, uuidOffs
}

func (g *generator) run(conns int, d time.Duration) stepResult {
	res := stepResult{conns: conns, codes: map[int]int{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	deadline := time.Now().Add(d)
	start := time.Now()
	for w := 0; w < conns; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			body, idOff, uuidOffs := g.template()
			var prefix [8]byte
			rand.Read(prefix[:])
			rng := mrand.New(mrand.NewPCG(binary.LittleEndian.Uint64(prefix[:]), uint64(w)))
			var gzBuf bytes.Buffer
			gzw := gzip.NewWriter(&gzBuf)
			lat := make([]time.Duration, 0, 4096)
			codes := map[int]int{}
			netErrs, reqs := 0, 0
			var sent int64
			for time.Now().Before(deadline) {
				// Unique batch_id: 8 random bytes per worker + a global counter.
				copy(body[idOff:], prefix[:])
				binary.BigEndian.PutUint64(body[idOff+8:], g.seq.Add(1))
				for _, off := range uuidOffs {
					binary.LittleEndian.PutUint64(body[off:], rng.Uint64())
					binary.LittleEndian.PutUint64(body[off+8:], rng.Uint64())
				}
				payload := body
				if g.gzip {
					gzBuf.Reset()
					gzw.Reset(&gzBuf)
					gzw.Write(body)
					gzw.Close()
					payload = gzBuf.Bytes()
				}
				req, _ := http.NewRequest(http.MethodPost, g.url, bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer "+g.token)
				if g.gzip {
					req.Header.Set("Content-Encoding", "gzip")
				}
				t0 := time.Now()
				resp, err := g.client.Do(req)
				if err != nil {
					netErrs++
					time.Sleep(10 * time.Millisecond)
					continue
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				took := time.Since(t0)
				reqs++
				codes[resp.StatusCode]++
				if resp.StatusCode == http.StatusOK {
					lat = append(lat, took)
					sent += int64(len(payload))
				} else if resp.StatusCode == 429 || resp.StatusCode == 503 {
					time.Sleep(20 * time.Millisecond)
				}
			}
			mu.Lock()
			res.lat = append(res.lat, lat...)
			res.requests += reqs
			res.netErrors += netErrs
			res.bytes += sent
			for c, n := range codes {
				res.codes[c] += n
			}
			mu.Unlock()
		}(w)
	}
	wg.Wait()
	res.elapsed = time.Since(start)
	res.events = len(res.lat) * g.events
	sort.Slice(res.lat, func(i, j int) bool { return res.lat[i] < res.lat[j] })
	return res
}

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted))*p+0.5) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func ms(d time.Duration) string {
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 2, 64)
}

func printStep(r stepResult) {
	ok := len(r.lat)
	secs := r.elapsed.Seconds()
	var other []string
	for c, n := range r.codes {
		if c != 200 {
			other = append(other, fmt.Sprintf("%d:%d", c, n))
		}
	}
	sort.Strings(other)
	fmt.Printf("conns=%-4d ok=%-8d req/s=%-9.0f events/s=%-10.0f MB/s=%-6.1f ack p50=%sms p90=%sms p99=%sms max=%sms | fsync n=%d mean=%sms p50=%sms p99=%sms | non-200=%v neterr=%d\n",
		r.conns, ok, float64(ok)/secs, float64(r.events)/secs, float64(r.bytes)/secs/1e6,
		ms(pct(r.lat, 0.50)), ms(pct(r.lat, 0.90)), ms(pct(r.lat, 0.99)), ms(pct(r.lat, 1)),
		r.fsync.count, ms(r.fsync.mean()), ms(r.fsync.quantile(0.50)), ms(r.fsync.quantile(0.99)), other, r.netErrors)
}

func printSummary(results []stepResult) {
	fmt.Println()
	fmt.Println("| senders | req/s | events/s | ack p50 (ms) | ack p99 (ms) | fsync mean (ms) | fsync p99 (ms) | non-200 |")
	fmt.Println("|---:|---:|---:|---:|---:|---:|---:|---:|")
	best := 0
	for i, r := range results {
		secs := r.elapsed.Seconds()
		if float64(r.events)/secs > float64(results[best].events)/results[best].elapsed.Seconds() {
			best = i
		}
		fmt.Printf("| %d | %.0f | %.0f | %s | %s | %s | %s | %d |\n", r.conns, float64(len(r.lat))/secs, float64(r.events)/secs,
			ms(pct(r.lat, 0.50)), ms(pct(r.lat, 0.99)), ms(r.fsync.mean()), ms(r.fsync.quantile(0.99)), r.requests-len(r.lat)+r.netErrors)
	}
	b := results[best]
	fmt.Printf("\npeak: %.0f events/s (%.0f req/s) at %d senders; ack p50 %s ms, p99 %s ms\n",
		float64(b.events)/b.elapsed.Seconds(), float64(len(b.lat))/b.elapsed.Seconds(), b.conns, ms(pct(b.lat, 0.50)), ms(pct(b.lat, 0.99)))
}

// histDelta is the la_ingest_fsync_seconds histogram (or the difference of
// two scrapes).
type histDelta struct {
	bounds []float64 // upper bounds, seconds; +Inf last
	cum    []float64 // cumulative counts per bound
	sum    float64
	count  int64
}

func (h histDelta) sub(o histDelta) histDelta {
	if len(o.cum) != len(h.cum) {
		return h
	}
	out := histDelta{bounds: h.bounds, cum: make([]float64, len(h.cum)), sum: h.sum - o.sum, count: h.count - o.count}
	for i := range h.cum {
		out.cum[i] = h.cum[i] - o.cum[i]
	}
	return out
}

func (h histDelta) mean() time.Duration {
	if h.count == 0 {
		return 0
	}
	return time.Duration(h.sum / float64(h.count) * float64(time.Second))
}

// quantile estimates a quantile by linear interpolation inside the bucket
// (the usual Prometheus histogram_quantile approximation).
func (h histDelta) quantile(q float64) time.Duration {
	if h.count == 0 || len(h.cum) == 0 {
		return 0
	}
	rank := q * float64(h.count)
	prevBound, prevCum := 0.0, 0.0
	for i, c := range h.cum {
		if c >= rank {
			bound := h.bounds[i]
			if bound > 1e17 { // +Inf bucket: report the last finite bound
				return time.Duration(prevBound * float64(time.Second))
			}
			frac := 1.0
			if c > prevCum {
				frac = (rank - prevCum) / (c - prevCum)
			}
			return time.Duration((prevBound + (bound-prevBound)*frac) * float64(time.Second))
		}
		prevBound, prevCum = h.bounds[i], c
	}
	return time.Duration(prevBound * float64(time.Second))
}

func scrapeFsync(client *http.Client, url string) (histDelta, error) {
	var h histDelta
	resp, err := client.Get(url)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, `la_ingest_fsync_seconds_bucket{le="`):
			rest := strings.TrimPrefix(line, `la_ingest_fsync_seconds_bucket{le="`)
			le, val, ok := strings.Cut(rest, `"} `)
			if !ok {
				continue
			}
			bound := 1e18
			if le != "+Inf" {
				if bound, err = strconv.ParseFloat(le, 64); err != nil {
					continue
				}
			}
			v, _ := strconv.ParseFloat(val, 64)
			h.bounds = append(h.bounds, bound)
			h.cum = append(h.cum, v)
		case strings.HasPrefix(line, "la_ingest_fsync_seconds_sum "):
			h.sum, _ = strconv.ParseFloat(strings.TrimPrefix(line, "la_ingest_fsync_seconds_sum "), 64)
		case strings.HasPrefix(line, "la_ingest_fsync_seconds_count "):
			h.count, _ = strconv.ParseInt(strings.TrimPrefix(line, "la_ingest_fsync_seconds_count "), 10, 64)
		}
	}
	return h, sc.Err()
}
