// Command la-ingest is the ingestion service (docs/design.md §6).
//
// Configuration is read from the environment; see ingest/README.md for the
// full list (LA_LISTEN, LA_NODE_ID, LA_WAL_DIR, LA_STORE_URL, ...).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mucan54/izlek/golib/objstore"
	"github.com/mucan54/izlek/ingest/server"
)

func main() {
	cfg, err := server.FromEnv()
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if err != nil {
		log.Error("invalid configuration", "err", err)
		os.Exit(2)
	}
	store, err := objstore.FromEnv()
	if err != nil {
		log.Error("object store", "err", err)
		os.Exit(2)
	}
	srv, err := server.New(cfg, store, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	exit := 0
	select {
	case err := <-serveErr:
		if err != nil {
			log.Error("listener failed", "err", err)
			exit = 1
		}
	case got := <-sig:
		log.Info("signal received, shutting down", "signal", got.String())
	}
	// A second signal aborts the graceful path; whatever is still on local
	// disk is recovered and uploaded by the next start.
	go func() {
		<-sig
		log.Error("second signal: exiting without waiting for uploads")
		os.Exit(1)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown incomplete", "err", err)
		exit = 1
	}
	os.Exit(exit)
}
