// Command la-control is the control plane (docs/design.md §14,
// spec/control.md): users, projects, tokens, flags, saved queries and
// deletion requests in SQLite, published as snapshots to object storage,
// plus the UI's query proxy.
//
//	la-control [serve]                       run the service
//	la-control bootstrap --email … --password … --org … --project …
//	la-control backup <path>                 consistent copy of the database
//	la-control publish                       publish every snapshot once
//
// Configuration comes from the environment; see control/README.md.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mucan54/izlek/control/app"
	"github.com/mucan54/izlek/control/auth"
	"github.com/mucan54/izlek/control/config"
	"github.com/mucan54/izlek/control/db"
	"github.com/mucan54/izlek/control/publish"
	"github.com/mucan54/izlek/golib/objstore"
)

// Exit codes.
const (
	exitOK      = 0
	exitFailure = 1 // runtime failure
	exitUsage   = 2 // bad arguments or configuration
	exitPublish = 3 // database updated, but publishing to the store failed
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }

func usage(w io.Writer) {
	fmt.Fprint(w, `usage:
  la-control [serve]
  la-control bootstrap --email EMAIL --password PASSWORD --org NAME --project NAME [--rotate-key] [--json] [--no-publish]
  la-control backup PATH
  la-control publish
  la-control help

Configuration is read from the environment (LA_CONTROL_DB, LA_STORE_URL, …);
see control/README.md.
`)
}

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	cfg, cfgErr := config.FromEnvFunc(getenv)
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	switch cmd {
	case "serve":
		// The service logs to stdout like the other components.
		log = slog.New(slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
		if cfgErr != nil {
			log.Error("invalid configuration", "err", cfgErr.Error())
			return exitUsage
		}
		return serve(cfg, log, getenv)
	case "bootstrap":
		return bootstrap(args, cfg, cfgErr, log, stdout, stderr, getenv)
	case "backup":
		return backup(args, cfg, log, stderr)
	case "publish":
		return publishOnce(cfg, log, getenv)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "la-control: unknown command %q\n", cmd)
		usage(stderr)
		return exitUsage
	}
}

func openDB(cfg config.Config, log *slog.Logger) (*db.DB, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		log.Error("cannot open database", "path", cfg.DBPath, "err", err.Error())
		return nil, false
	}
	return d, true
}

func serve(cfg config.Config, log *slog.Logger, getenv func(string) string) int {
	store, err := objstore.FromEnvFunc(getenv)
	if err != nil {
		log.Error("object store", "err", err.Error())
		return exitUsage
	}
	d, ok := openDB(cfg, log)
	if !ok {
		return exitFailure
	}
	defer d.Close()
	a, err := app.New(app.Options{Config: cfg, DB: d, Store: store, Logger: log})
	if err != nil {
		log.Error("startup failed", "err", err.Error())
		return exitFailure
	}
	if cfg.InternalSecret == "" {
		log.Warn("LA_QUERY_INTERNAL_SECRET is not set: the query proxy answers 503")
	}
	if cfg.PublicURLIgnored != "" {
		log.Warn("LA_PUBLIC_URL ignored: OAuth sign-in for MCP clients is off; /mcp accepts secret keys only", "reason", cfg.PublicURLIgnored)
	}
	if !cfg.CookieSecure {
		log.Warn("LA_COOKIE_SECURE=false: session cookies travel without the Secure flag; use only for local development")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	a.Start(ctx)
	code := exitOK
	if err := a.Serve(ctx); err != nil {
		log.Error("listener failed", "err", err.Error())
		code = exitFailure
	}
	stop() // a second signal now kills the process the default way
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := a.Stop(sctx); err != nil {
		log.Warn("unpublished changes remain; the next start publishes them", "err", err.Error())
	}
	return code
}

func bootstrap(args []string, cfg config.Config, cfgErr error, log *slog.Logger, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var in app.BootstrapInput
	fs.StringVar(&in.Email, "email", "", "e-mail address of the owner")
	fs.StringVar(&in.Password, "password", "", "password of the owner (or LA_BOOTSTRAP_PASSWORD); only used when the user is created")
	fs.StringVar(&in.Org, "org", "", "organisation name")
	fs.StringVar(&in.Project, "project", "", "project name")
	fs.BoolVar(&in.RotateKey, "rotate-key", false, "create a new secret key even if a bootstrap key exists")
	asJSON := fs.Bool("json", false, "print the result as one JSON object")
	noPublish := fs.Bool("no-publish", false, "do not publish to the object store")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "la-control bootstrap: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	// Each flag can also come from the environment, so that a container
	// platform can pass them without interpolating them into the command.
	for _, f := range []struct {
		v   *string
		env string
	}{{&in.Email, "LA_BOOTSTRAP_EMAIL"}, {&in.Password, "LA_BOOTSTRAP_PASSWORD"}, {&in.Org, "LA_BOOTSTRAP_ORG"}, {&in.Project, "LA_BOOTSTRAP_PROJECT"}} {
		if *f.v == "" {
			*f.v = strings.TrimSpace(getenv(f.env))
		}
	}
	if in.Email == "" || in.Org == "" || in.Project == "" {
		fmt.Fprintln(stderr, "la-control bootstrap: --email, --org and --project are required (or LA_BOOTSTRAP_EMAIL, LA_BOOTSTRAP_ORG, LA_BOOTSTRAP_PROJECT)")
		return exitUsage
	}
	if cfgErr != nil {
		// Only the database path matters here; other settings may be incomplete.
		log.Warn("configuration problems (ignored by bootstrap)", "err", cfgErr.Error())
	}
	d, ok := openDB(cfg, log)
	if !ok {
		return exitFailure
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := app.Bootstrap(ctx, d, auth.NewHasher(auth.DefaultParams, 1), in, time.Now())
	if err != nil {
		fmt.Fprintf(stderr, "la-control: %v\n", err)
		return exitFailure
	}
	// Results first: the secret is shown exactly once and must not be lost
	// to a later publishing error.
	if *asJSON {
		_ = json.NewEncoder(stdout).Encode(res)
	} else {
		fmt.Fprintf(stdout, "user_id=%d\norg_id=%d\nproject_id=%d\ntoken=%s\n", res.UserID, res.OrgID, res.ProjectID, res.Token)
		if res.Secret != "" {
			fmt.Fprintf(stdout, "secret=%s\n", res.Secret)
		}
	}
	if res.Secret == "" {
		fmt.Fprintln(stderr, "la-control: the bootstrap secret key already exists and cannot be shown again (use --rotate-key for a new one)")
	}
	if *noPublish {
		return exitOK
	}
	if getenv("LA_STORE_URL") == "" && getenv("LA_S3_BUCKET") == "" {
		fmt.Fprintln(stderr, "la-control: no object store configured; the service publishes when it starts")
		return exitOK
	}
	if code := publishWith(ctx, d, log, getenv); code != exitOK {
		fmt.Fprintln(stderr, "la-control: the database is updated but publishing failed; a running service retries on its own, or run `la-control publish`")
		return code
	}
	return exitOK
}

func publishWith(ctx context.Context, d *db.DB, log *slog.Logger, getenv func(string) string) int {
	store, err := objstore.FromEnvFunc(getenv)
	if err != nil {
		log.Error("object store", "err", err.Error())
		return exitPublish
	}
	p := publish.New(publish.Options{DB: d, Store: store, Logger: log})
	if err := p.PublishNow(ctx); err != nil {
		log.Error("publish failed", "err", err.Error())
		return exitPublish
	}
	return exitOK
}

func publishOnce(cfg config.Config, log *slog.Logger, getenv func(string) string) int {
	d, ok := openDB(cfg, log)
	if !ok {
		return exitFailure
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	return publishWith(ctx, d, log, getenv)
}

func backup(args []string, cfg config.Config, log *slog.Logger, stderr io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(stderr, "usage: la-control backup PATH")
		return exitUsage
	}
	if _, err := os.Stat(cfg.DBPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "la-control: database %s does not exist\n", cfg.DBPath)
		return exitFailure
	}
	d, ok := openDB(cfg, log)
	if !ok {
		return exitFailure
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := d.Backup(ctx, args[0]); err != nil {
		fmt.Fprintf(stderr, "la-control: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stderr, "la-control: backup written to %s\n", args[0])
	return exitOK
}
