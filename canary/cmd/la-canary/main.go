// la-canary: end-to-end loss/duplicate probe (docs/design.md §16.3).
//
//	la-canary                 run forever, expose /metrics
//	la-canary -once -batches 3 -timeout 120s   send, wait until visible, exit 0/1
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mucan54/izlek/canary"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envDur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		log.Fatalf("%s: not a duration: %q", k, v)
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			log.Fatalf("%s: not a number: %q", k, v)
		}
		return n
	}
	return def
}

func main() {
	once := flag.Bool("once", false, "send, wait for visibility, then exit (0 = all events visible exactly once)")
	batches := flag.Int("batches", 3, "with -once: number of batches to send")
	timeout := flag.Duration("timeout", 3*time.Minute, "with -once: how long to wait for visibility")
	poll := flag.Duration("poll", 2*time.Second, "with -once: verification interval")
	flag.Parse()

	cfg := canary.Config{
		IngestURL:   env("LA_CANARY_INGEST_URL", ""),
		Token:       env("LA_CANARY_TOKEN", ""),
		QueryURL:    env("LA_CANARY_QUERY_URL", ""),
		QueryKey:    env("LA_CANARY_QUERY_KEY", ""),
		Interval:    envDur("LA_CANARY_INTERVAL", time.Minute),
		PerTick:     envInt("LA_CANARY_EVENTS_PER_TICK", 5),
		Grace:       envDur("LA_CANARY_GRACE", 3*time.Minute),
		ResendEvery: envInt("LA_CANARY_RESEND_EVERY", 10),
		StatePath:   env("LA_CANARY_STATE", ""),
		Logf:        log.Printf,
	}
	c, err := canary.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *once {
		lag, err := c.Probe(ctx, *batches, *timeout, *poll)
		s := c.Stats()
		if err != nil {
			fmt.Fprintf(os.Stderr, "canary FAILED: %v (sent=%d visible=%d missing=%d duplicates=%d)\n",
				err, s.Sent, s.Visible, s.Missing, s.Duplicates)
			os.Exit(1)
		}
		fmt.Printf("canary OK: run=%s events=%d visible=%d duplicates=0 freshness<=%.1fs\n", s.Run, s.Sent, s.Visible, lag.Seconds())
		return
	}

	listen := env("LA_CANARY_LISTEN", ":8099")
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(c.Metrics()))
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !c.Stats().Healthy {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if listen != "off" {
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("canary: metrics listener: %v", err)
			}
		}()
	}
	log.Printf("canary: run %s, interval %s, metrics on %s", c.State().Run, cfg.Interval, listen)
	_ = c.Run(ctx)
	shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
}
