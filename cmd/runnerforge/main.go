// Command runnerforge provisions one throwaway machine per CI job for GitHub
// Actions, GitLab CI and Forgejo Actions.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Cloud drivers register themselves on import.
	_ "github.com/slop-place/runnerforge/internal/cloud/dockerdrv"
	_ "github.com/slop-place/runnerforge/internal/cloud/openstack"

	// Forge implementations register themselves on import.
	_ "github.com/slop-place/runnerforge/internal/forge/forgejo"
	_ "github.com/slop-place/runnerforge/internal/forge/github"
	_ "github.com/slop-place/runnerforge/internal/forge/gitlab"

	"github.com/slop-place/runnerforge/internal/auth"
	"github.com/slop-place/runnerforge/internal/config"
	"github.com/slop-place/runnerforge/internal/controller"
	"github.com/slop-place/runnerforge/internal/k8s"
	"github.com/slop-place/runnerforge/internal/store"
	"github.com/slop-place/runnerforge/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	// oneShotTimeout bounds a single `reap` or `destroy-all` invocation.
	oneShotTimeout = 5 * time.Minute
	// readHeaderTimeout guards the web listener against slow-header attacks.
	readHeaderTimeout = 10 * time.Second
	// shutdownTimeout is how long in-flight web requests get to finish.
	shutdownTimeout = 10 * time.Second
	// errBuffer sizes the channel the two long-running goroutines report on.
	errBuffer = 2
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "runnerforge:", err)
		os.Exit(1)
	}
}

func usage() string {
	return `runnerforge — ephemeral CI runners on demand

Usage:
  runnerforge serve [-config FILE]    run the controller and web UI
  runnerforge genkey                  print a new secret_key
  runnerforge reap [-config FILE]     sweep every cloud once and exit
  runnerforge destroy-all [-config FILE]
                                      destroy every machine this deployment owns
  runnerforge version

Clouds, forges and pools are managed in the web UI, not in the config file.
The config file holds only what is needed to start: identity, database and
the key that encrypts credentials at rest.`
}

// run dispatches a subcommand. Output goes to out rather than straight to
// stdout so the command surface is testable.
func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(out, usage())
		return nil
	}
	cmd, rest := args[0], args[1:]

	switch cmd {
	case "version":
		_, _ = fmt.Fprintln(out, version)
		return nil
	case "genkey":
		k, err := config.GenerateSecretKey()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(out, k)
		return nil
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return serve(ctx, rest)
	case "reap":
		return oneShot(rest, func(ctx context.Context, c *controller.Controller) error {
			return c.Reap(ctx)
		})
	case "destroy-all":
		return oneShot(rest, func(ctx context.Context, c *controller.Controller) error {
			n, err := c.DestroyAll(ctx)
			_, _ = fmt.Fprintf(out, "destroyed %d machine(s)\n", n)
			return err
		})
	case "-h", "--help", "help":
		_, _ = fmt.Fprintln(out, usage())
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage())
	}
}

// setup loads config, opens the store and builds a controller.
func setup(args []string) (*config.Config, *store.DB, *controller.Controller, error) {
	fs := flag.NewFlagSet("runnerforge", flag.ContinueOnError)
	path := fs.String("config", "runnerforge.yaml", "path to the bootstrap config file")
	debug := fs.Bool("debug", false, "enable debug logging")
	useK8s := fs.Bool("k8s", false,
		"reconcile Kubernetes custom resources into this deployment")
	if err := fs.Parse(args); err != nil {
		return nil, nil, nil, fmt.Errorf("parse flags: %w", err)
	}

	cfg, err := config.Load(*path)
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := cfg.Key()
	if err != nil {
		return nil, nil, nil, err
	}
	if err := store.SetKey(key); err != nil {
		return nil, nil, nil, err
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	db, err := store.Open(cfg.Database.Driver, cfg.Database.DSN)
	if err != nil {
		return nil, nil, nil, err
	}
	if *useK8s {
		cfg.Kubernetes.Enabled = true
	}
	return cfg, db, controller.New(db, cfg, log), nil
}

func oneShot(args []string, fn func(context.Context, *controller.Controller) error) error {
	_, _, ctrl, err := setup(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), oneShotTimeout)
	defer cancel()
	return fn(ctx, ctrl)
}

// serve runs the web UI and the controller until ctx is cancelled.
//
// The context is a parameter rather than being built here so the whole command
// can be started and stopped by a test, and so it can be embedded.
func serve(ctx context.Context, args []string) error {
	cfg, db, ctrl, err := setup(args)
	if err != nil {
		return err
	}
	key, err := cfg.Key()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := context.WithCancel(ctx)
	defer stop()

	// Discovery talks to the issuer, so this can fail when the provider is
	// unreachable — better at startup than on an operator's first request.
	authn, err := auth.New(ctx, cfg.OIDC, key, log)
	if err != nil {
		return fmt.Errorf("configure sign-in: %w", err)
	}
	if authn.Enabled() {
		log.Info("web UI requires sign-in", "issuer", cfg.OIDC.Issuer)
	} else {
		log.Warn("web UI is NOT protected: anyone who can reach it can add a cloud " +
			"and destroy machines. Set oidc.issuer to require sign-in.")
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           web.New(db, ctrl, cfg, log, authn).Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// The Kubernetes reconciler is optional and additive: with it on, resources
	// applied to the cluster appear here, and everything else keeps working.
	if cfg.Kubernetes.Enabled {
		rec, err := k8s.New(cfg.Kubernetes, db, log)
		if err != nil {
			return fmt.Errorf("configure the Kubernetes reconciler: %w", err)
		}
		go func() {
			if err := rec.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				log.Error("kubernetes reconciler stopped", "err", err)
			}
		}()
	}

	errs := make(chan error, errBuffer)
	go func() {
		log.Info("web UI listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()
	go func() {
		if err := ctrl.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		stop()
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Give in-flight requests a moment, but do not wait on the controller: any
	// machine it was mid-flight on is the reaper's problem on next startup,
	// which is exactly what the reaper is for.
	//
	// This deliberately does not inherit ctx: ctx is already cancelled by the
	// time we get here, and a shutdown deadline derived from it would expire
	// immediately, cutting off requests that are still being served.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shut down web server: %w", err)
	}
	return nil
}
