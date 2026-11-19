// Package app wires the control plane together: database, publisher,
// deletion checker, HTTP servers. It also implements the operations of the
// la-control command line (bootstrap, backup, publish).
package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mucan54/izlek/control/api"
	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/metrics"
	"github.com/mucan54/izlek/control/oauth"
	"github.com/mucan54/izlek/control/publish"
	"github.com/mucan54/izlek/golib/objstore"
)

// Options are the dependencies of an App. Zero values get production
// defaults; tests override Now, Hasher and the timings in Config.
type Options struct {
	Config config.Config
	DB     *db.DB
	Store  objstore.Store
	Logger *slog.Logger
	Now    func() time.Time
	Hasher *auth.Hasher
	// QueryTransport overrides how the query service is reached (tests).
	QueryTransport http.RoundTripper
	// PublishRetryMin/Max override the publisher's backoff (tests).
	PublishRetryMin, PublishRetryMax time.Duration
	// ClientMetadata overrides how OAuth Client ID Metadata Documents are
	// fetched (tests).
	ClientMetadata *oauth.Fetcher
}

// App is a running control plane minus its listeners.
type App struct {
	opt       Options
	DB        *db.DB
	Publisher *publish.Publisher
	Checker   *publish.Checker
	Server    *api.Server
	Metrics   *metrics.Registry

	pendingDeletions atomic.Int64
	wg               sync.WaitGroup
	cancel           context.CancelFunc
}

// New builds an App. Nothing runs until Start.
func New(opt Options) (*App, error) {
	if opt.DB == nil || opt.Store == nil {
		return nil, errors.New("app: database and object store are required")
	}
	if opt.Logger == nil {
		opt.Logger = slog.New(slog.DiscardHandler)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Hasher == nil {
		// Each hash holds 64 MiB while it runs; four at a time bounds the
		// memory a burst of logins can take.
		opt.Hasher = auth.NewHasher(auth.DefaultParams, 4)
	}
	a := &App{opt: opt, DB: opt.DB, Metrics: metrics.New()}
	a.Publisher = publish.New(publish.Options{
		DB: opt.DB, Store: opt.Store, Logger: opt.Logger, Now: opt.Now,
		Debounce: opt.Config.PublishDebounce, Interval: opt.Config.RepublishEvery,
		RetryMin: opt.PublishRetryMin, RetryMax: opt.PublishRetryMax,
	})
	a.Checker = &publish.Checker{DB: opt.DB, Store: opt.Store, Logger: opt.Logger, Now: opt.Now}

	reg := a.Metrics
	reg.Gauge("publish_lag_seconds", "Age of the oldest change not yet published to the object store (0 = up to date).", a.Publisher.LagSeconds)
	reg.CounterFunc("publish_success_total", "Successful publishes.", func() uint64 { return a.Publisher.Stats().Published })
	reg.CounterFunc("publish_failures_total", "Failed publishes.", func() uint64 { return a.Publisher.Stats().Failed })
	reg.CounterFunc("publish_objects_total", "Objects written to or deleted from the object store.", func() uint64 { return a.Publisher.Stats().Objects })
	reg.Gauge("publish_last_success_timestamp_seconds", "Unix time of the last successful publish (0 = never).", func() float64 {
		return float64(a.Publisher.Stats().LastSuccessMillis) / 1000
	})
	reg.Gauge("deletions_pending", "Deletion requests not yet applied by the engine.", func() float64 { return float64(a.pendingDeletions.Load()) })
	reg.Gauge("sessions", "Live login sessions.", func() float64 {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		n, err := db.CountSessions(ctx, opt.DB.R(), opt.Now().UnixMilli())
		if err != nil {
			return -1
		}
		return float64(n)
	})
	reg.Gauge("build_info", "Always 1.", func() float64 { return 1 })

	srv, err := api.New(api.Options{
		Config: opt.Config, DB: opt.DB, Notifier: a.Publisher, Hasher: opt.Hasher, Logger: opt.Logger,
		Metrics: reg, Now: opt.Now, QueryTransport: opt.QueryTransport, ClientMetadata: opt.ClientMetadata,
	})
	if err != nil {
		return nil, err
	}
	a.Server = srv
	return a, nil
}

// Start launches the background work: the publisher (which publishes once
// immediately), the deletion checker and session clean-up.
func (a *App) Start(ctx context.Context) {
	ctx, a.cancel = context.WithCancel(ctx)
	a.wg.Add(3)
	go func() {
		defer a.wg.Done()
		a.Publisher.Run(ctx)
	}()
	go func() {
		defer a.wg.Done()
		a.Checker.Run(ctx, a.opt.Config.DeletionCheck, func(pending int) { a.pendingDeletions.Store(int64(pending)) })
	}()
	go func() {
		defer a.wg.Done()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			now := a.opt.Now().UnixMilli()
			if err := a.DB.Write(ctx, func(tx *sql.Tx) error {
				_, err := db.DeleteExpiredSessions(ctx, tx, now)
				return err
			}); err != nil && ctx.Err() == nil {
				a.opt.Logger.Warn("session clean-up failed", "err", err.Error())
			}
			if err := a.DB.Write(ctx, func(tx *sql.Tx) error { return api.PruneOAuth(ctx, tx, a.opt.Now()) }); err != nil && ctx.Err() == nil {
				a.opt.Logger.Warn("oauth clean-up failed", "err", err.Error())
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Stop ends the background work. Changes that are still unpublished get one
// last attempt within ctx; whatever remains is published by the next start
// (the database is the source of truth).
func (a *App) Stop(ctx context.Context) error {
	if a.cancel != nil {
		a.cancel()
	}
	a.wg.Wait()
	err := a.Publisher.Flush(ctx)
	a.Server.Close()
	return err
}

// Serve runs the HTTP listeners until ctx is done, then shuts them down
// gracefully. It returns the first listener error, if any.
func (a *App) Serve(ctx context.Context) error {
	cfg := a.opt.Config
	main := &http.Server{
		Addr:              cfg.Listen,
		Handler:           a.Server,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// Write deadlines are set per request (the query proxy may stream
		// for as long as a query is allowed to run).
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 32 << 10,
		ErrorLog:       slog.NewLogLogger(a.opt.Logger.Handler(), slog.LevelWarn),
	}
	servers := []*http.Server{main}
	if cfg.AdminListen != "" {
		servers = append(servers, &http.Server{
			Addr: cfg.AdminListen, Handler: a.Server.AdminHandler(),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
		})
	}
	errc := make(chan error, len(servers))
	for _, srv := range servers {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, s := range servers {
				_ = s.Close()
			}
			return fmt.Errorf("app: listen %s: %w", srv.Addr, err)
		}
		a.opt.Logger.Info("listening", "addr", ln.Addr().String(), "admin", srv != main)
		go func(srv *http.Server) {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}(srv)
	}
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errc:
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, srv := range servers {
		if err := srv.Shutdown(sctx); err != nil {
			_ = srv.Close()
		}
	}
	return serveErr
}
